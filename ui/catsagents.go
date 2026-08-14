package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Handing a query to the agent in the next pane.
//
// cats already arbitrates which pane is running which agent and what state it
// is in, so "claude is in pane 26 and it is blocked" arrives for free from
// pane.list. That turns "ask the agent about this query" from an integration
// into a menu.
//
// TWO DESIGN RULES CARRY THIS FILE:
//
//   - THE TEXT IS STAGED, NEVER SUBMITTED. pane.send_input can press Enter
//     and deliberately does not. Putting a question in an agent's mouth is
//     one thing; running it is another, and the user is one keystroke away
//     from doing that themselves once they can see what was written.
//   - ONLY AGENT PANES ARE OFFERED. A fenced SQL block typed at a plain shell
//     prompt is junk, so a pane cats has not identified as an agent is not a
//     destination. cats' own chat panel is the other one, and it is the
//     fallback when no sibling agent exists.
//
// THE PANE LIST IS CACHED because the picker is opened by a keystroke and a
// keystroke must not dial a socket. It is refreshed when Tier 1 comes up,
// when the picker opens (for the NEXT open), and on the events that mean the
// answer changed.

// catsPanePollMin rate-limits the pane.list refresh. Agent state changes a
// few times a minute at most, and every refresh is a round trip.
const catsPanePollMin = 2 * time.Second

// catsAgentRank orders the picker by how much a pane wants attention. A
// blocked agent is the one worth interrupting; an idle one is the one with
// nothing to say.
func catsAgentRank(state string) int {
	switch state {
	case cats.StateBlocked:
		return 3
	case cats.StateWorking:
		return 2
	case cats.StateIdle:
		return 1
	}
	return 0
}

// catsPollPanes refreshes the cached pane list, unless it was refreshed
// recently. Runs the call on a goroutine and installs the result on the UI
// goroutine.
func (a *App) catsPollPanes(force bool) {
	if !a.catsTier1() {
		return
	}
	if !force && time.Since(a.cats.panesAt) < catsPanePollMin {
		return
	}
	a.cats.panesAt = time.Now()
	client := a.cats.client
	go func() {
		panes, err := client.PaneList()
		if err != nil {
			return // the cache keeps what it had; a stale list beats none
		}
		a.catsPost(func() { a.cats.panes = panes })
	}()
}

// catsAgentPanes is the cached list, minus our own pane, ordered by rank.
//
// Excluding ourselves is not cosmetic: dbc reports itself to cats as the
// agent "dbc", so without this it would offer to send the query to itself.
func (a *App) catsAgentPanes() []cats.PaneInfo {
	var out []cats.PaneInfo
	for _, p := range a.cats.panes {
		if p.Agent == "" || a.catsIsSelf(p) {
			continue
		}
		out = append(out, p)
	}
	// A stable order the user can learn: most-blocked first, then by pane id
	// so equal-ranked rows do not shuffle between openings.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			ri, rj := catsAgentRank(out[j].AgentState), catsAgentRank(out[j-1].AgentState)
			if ri < rj || (ri == rj && out[j].Pane > out[j-1].Pane) {
				break
			}
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// catsIsSelf reports whether a pane is the one dbc is running in. The numeric
// id is the reliable answer; the handle is the fallback for a host that did
// not resolve.
func (a *App) catsIsSelf(p cats.PaneInfo) bool {
	if a.cats.selfOK && p.Pane == a.cats.self {
		return true
	}
	return p.Handle != "" && p.Handle == a.cats.caps.PaneHandle
}

// catsQuestion builds what gets sent: the SQL in question, in context.
//
// The connection and driver lead because they are the part an agent cannot
// infer and most needs — the same statement means different things to
// Postgres and SQLite. The error, when the last run produced one, follows the
// query rather than replacing it: "why did this fail" is a question about
// both halves.
func (a *App) catsQuestion() string {
	sql := strings.TrimSpace(a.catsSelectedSQL())
	if sql == "" {
		return ""
	}
	var b strings.Builder
	if cc, ok := a.cfg.ConnByName(a.active); ok {
		fmt.Fprintf(&b, "In dbc, on %s (%s):\n\n", cc.Name, cc.Driver)
	} else {
		b.WriteString("In dbc:\n\n")
	}
	b.WriteString("```sql\n" + sql + "\n```\n")
	if a.lastRunErr != "" {
		b.WriteString("\nIt failed with: " + a.lastRunErr + "\n")
	}
	return b.String()
}

// catsSelectedSQL is the selection if there is one, and otherwise the
// statement the cursor is sitting in — the same rule Ctrl+R runs by, so what
// gets asked about is what would get executed.
func (a *App) catsSelectedSQL() string {
	sel, cursor, _ := a.editor.GetSelection()
	if strings.TrimSpace(sel) != "" {
		return sel
	}
	all := sqlsplit.Split(a.editor.GetText())
	if len(all) == 0 {
		return ""
	}
	return all[sqlsplit.IndexAt(all, cursor)].Text
}

// showAgentModal is Ctrl+G: pick who to ask about the current statement.
func (a *App) showAgentModal() {
	if !a.catsTier1() {
		a.log(tagWarn + "asking an agent needs cats — dbc is running standalone")
		return
	}
	question := a.catsQuestion()
	if question == "" {
		a.log(tagWarn + "nothing to ask about — put the cursor in a statement first")
		return
	}

	panes := a.catsAgentPanes()
	a.catsPollPanes(false) // refresh for the next open, not this one

	list := tview.NewList().ShowSecondaryText(true)
	list.SetMainTextColor(colFg).SetSecondaryTextColor(colMuted).
		SetSelectedStyle(tcell.StyleDefault.
			Background(colSel).Foreground(colFg).Bold(true))
	list.SetHighlightFullLine(true)

	for _, p := range panes {
		name := p.Agent
		where := p.Handle
		if where == "" {
			where = fmt.Sprintf("pane %d", p.Pane)
		}
		state := p.AgentState
		if state == "" {
			state = cats.StateUnknown
		}
		list.AddItem(name, "  "+state+" · "+where, 0, func() {
			a.pages.RemovePage("agents")
			a.catsSendToPane(p, question)
		})
	}
	list.AddItem("cats chat", "  the host's own chat panel", 0, func() {
		a.pages.RemovePage("agents")
		a.catsSendToChat(question)
	})

	list.SetBorder(true)
	list.SetTitle(" Ask about this statement (Enter sends, Esc closes) ")
	themeModal(list.Box)

	h := list.GetItemCount() + 4
	if h > 20 {
		h = 20
	}
	a.pages.AddPage("agents", center(list, 64, h), true, true)
	a.app.SetFocus(list)
}

// catsSendToPane stages the question in an agent's pane and moves the user
// there.
//
// Focusing is the honest completion of the gesture rather than an extra
// liberty: the text is deliberately NOT submitted, so it is inert until
// somebody presses Enter in that pane. Sending without focusing would leave
// the user hunting for where their question went.
func (a *App) catsSendToPane(p cats.PaneInfo, question string) {
	client := a.cats.client
	name := p.Agent
	go func() {
		err := client.PaneSendInput(p.Pane, question, false)
		if err == nil {
			err = client.PaneFocus(p.Pane)
		}
		a.catsPost(func() {
			if err != nil {
				a.logf(tagErr+"could not reach %s: %s", name, err)
				return
			}
			a.logf(tagOk+"sent to %s"+tagOff+" — it is waiting there unsent, press Enter to ask", name)
		})
	}()
}

// catsSendToChat posts the question to cats' own chat panel. Unlike a pane,
// chat.send submits by nature — the panel is a conversation, not a prompt
// somebody is typing at.
func (a *App) catsSendToChat(question string) {
	client := a.cats.client
	go func() {
		err := client.ChatSend(question)
		a.catsPost(func() {
			if err != nil {
				a.logf(tagErr+"could not reach cats chat: %s", err)
				return
			}
			a.log(tagOk + "asked cats chat")
		})
	}()
}
