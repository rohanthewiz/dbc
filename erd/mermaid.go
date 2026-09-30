package erd

import (
	"fmt"
	"regexp"
	"strings"
)

// Mermaid renders the schema as a Mermaid erDiagram, for a pull request, an
// issue or a wiki page that draws ```mermaid blocks (GitHub, GitLab, Notion,
// Obsidian …). The page that draws it applies its own theme, so there is no
// palette here.
//
//	erDiagram
//	    %% dbc · demo (sqlite) · 3 tables · 2 relationships
//	    owners ||--o{ cats : "owner_id"
//	    cats ||--|{ visits : "cat_id"
//	    cats {
//	        INTEGER id PK
//	        INTEGER owner_id FK
//	    }
//
// THE NOTATION. A relationship line is written parent-first, so its left
// end is the "one" side and its right end the child's:
//
//	parent end  ||  exactly one      the foreign key's columns are NOT NULL
//	            |o  zero or one      one of them is nullable (Rel.Optional)
//	child end   o{  zero or many     the usual foreign key
//	            o|  zero or one      the key's columns are unique in the
//	                                 child (Rel.OneToOne): a 1:1 extension
//	line        --  identifying      the key is inside the child's PK
//	            ..  non-identifying  the rest (Rel.Identifying)
//
// The label is the child's key columns, which is what a reader needs to
// write the join; the constraint's name is usually a generated
// "<table>_<col>_fkey" that says the same thing less usefully.
//
// NAMES. Mermaid's grammar is narrow: an entity name is a word, and an
// attribute's type and name are words of letters, digits, _ - ( ) [ ].
// A table whose label is not such a word ("public.cats" on a multi-schema
// connection, a name with a space) is given a generated id with the label
// as its alias — id["label"] — which Mermaid draws as the label (entity
// name aliases need Mermaid 10.5 or later; a plain word is used whenever
// possible so older renderers still read most diagrams). A column name or
// type that is not a word is made one (spaces and commas become _), and the
// original is kept as the attribute's comment, so "numeric(10,2)" is still
// readable in the drawing.
func (s *Schema) Mermaid() string {
	var b strings.Builder
	b.WriteString("erDiagram\n")
	fmt.Fprintf(&b, "    %%%% %s\n", mermaidComment(s.Title()))

	ids := s.mermaidIDs()
	for _, r := range s.Rels {
		left := "||"
		if r.Optional() {
			left = "|o"
		}
		right := "o{"
		if r.OneToOne() {
			right = "o|"
		}
		line := ".."
		if r.Identifying() {
			line = "--"
		}
		fmt.Fprintf(&b, "    %s %s%s%s %s : \"%s\"\n",
			ids[r.Parent], left, line, right, ids[r.Child], mermaidQuoted(strings.Join(r.ChildCols, ", ")))
	}
	for _, t := range s.Tables {
		b.WriteString("    " + ids[t])
		if ids[t] != t.Label {
			b.WriteString(`["` + mermaidQuoted(t.Label) + `"]`)
		}
		if len(t.Cols) == 0 {
			// an entity with no attribute block is still declared, so a
			// table with no relationships and no readable columns shows
			b.WriteString("\n")
			continue
		}
		b.WriteString(" {\n")
		for _, c := range t.Cols {
			typ, name := mermaidWord(c.Type, "unknown"), mermaidWord(c.Name, "column")
			b.WriteString("        " + typ + " " + name)
			var keys []string
			if c.PK {
				keys = append(keys, "PK")
			}
			if c.FK {
				keys = append(keys, "FK")
			}
			if c.Unique && !c.PK {
				keys = append(keys, "UK")
			}
			if len(keys) > 0 {
				b.WriteString(" " + strings.Join(keys, ", "))
			}
			// the comment carries what the words had to lose
			var note []string
			if name != c.Name {
				note = append(note, c.Name)
			}
			if typ != c.Type && c.Type != "" {
				note = append(note, c.Type)
			}
			if len(note) > 0 {
				b.WriteString(` "` + mermaidQuoted(strings.Join(note, ": ")) + `"`)
			}
			b.WriteString("\n")
		}
		b.WriteString("    }\n")
	}
	return b.String()
}

// Title is the one-line description every rendering carries: where the
// schema came from and how big the diagram is.
func (s *Schema) Title() string {
	var parts []string
	if s.Conn != "" {
		src := s.Conn
		if s.Driver != "" {
			src += " (" + s.Driver + ")"
		}
		parts = append(parts, src)
	}
	parts = append(parts, plural(len(s.Tables), "table"), plural(len(s.Rels), "relationship"))
	return "dbc · " + strings.Join(parts, " · ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// mermaidEntity is what Mermaid accepts as a bare entity name.
var mermaidEntity = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// mermaidIDs gives every table its entity id: the label when that is a plain
// word, else t<n> (aliased to the label by the caller). A generated id that
// happens to equal another table's label gets a further suffix, so no two
// tables ever share an id — a clash would silently merge two boxes.
func (s *Schema) mermaidIDs() map[*Table]string {
	ids := make(map[*Table]string, len(s.Tables))
	used := map[string]bool{}
	for _, t := range s.Tables {
		if mermaidEntity.MatchString(t.Label) && !used[t.Label] {
			ids[t] = t.Label
			used[t.Label] = true
		}
	}
	n := 0
	for _, t := range s.Tables {
		if _, ok := ids[t]; ok {
			continue
		}
		for {
			n++
			id := fmt.Sprintf("t%d", n)
			if !used[id] {
				ids[t], used[id] = id, true
				break
			}
		}
	}
	return ids
}

// mermaidWord makes s one attribute word: letters, digits and _ - ( ) [ ]
// are kept, runs of anything else become one _, and a leading character
// Mermaid would not start a word with (a digit, a bracket) gets a _ in
// front. An empty result is replaced by def.
func mermaidWord(s, def string) string {
	var b strings.Builder
	under := false
	for _, r := range strings.TrimSpace(s) {
		ok := r == '_' || r == '-' || r == '(' || r == ')' || r == '[' || r == ']' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			if !under {
				b.WriteByte('_')
				under = true
			}
			continue
		}
		b.WriteRune(r)
		under = false
	}
	// a name's own underscores are kept (_id stays _id); a word that is
	// nothing but underscores said nothing, so it falls back to def
	w := b.String()
	if strings.Trim(w, "_") == "" {
		return def
	}
	if c := w[0]; !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		w = "_" + w
	}
	return w
}

// mermaidQuoted makes s safe inside a double-quoted Mermaid string: the
// grammar has no escape for a double quote, so it becomes a single one, and
// a line break would end the statement.
func mermaidQuoted(s string) string {
	return strings.NewReplacer(`"`, "'", "\n", " ", "\r", " ").Replace(s)
}

// mermaidComment keeps a %% comment on one line.
func mermaidComment(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}
