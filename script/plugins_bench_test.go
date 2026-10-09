package script

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/pipeline"
)

// How much an interpreted transform costs against a compiled one, per
// batch of 1000 rows with one text column lower-cased and trimmed: the
// number the README quotes so a user knows when to reach for a built-in
// (text.clean, measured beside them) over go.transform.
//
//	go test ./script -bench Transform -run XXX -benchmem

func benchBatch() *pipeline.Batch {
	rows := make([][]any, 1000)
	for i := range rows {
		rows[i] = []any{int64(i), "  Some.Customer@Example.ORG  ", float64(i) * 1.5}
	}
	return pipeline.NewBatch(pipeline.ColsOf([]string{"id", "email", "total"}, nil), rows)
}

const benchSnippet = `func Apply(b *sdb.Batch) (*sdb.Batch, error) {
	c := b.Col("email")
	for i := range b.Rows {
		if s, ok := b.Rows[i][c].(string); ok {
			b.Rows[i][c] = strings.ToLower(strings.TrimSpace(s))
		}
	}
	return b, nil
}`

func BenchmarkTransformInterpreted(b *testing.B) {
	p, _ := pipeline.Lookup("go.transform")
	node, err := p.New(pipeline.Config{"code": benchSnippet})
	if err != nil {
		b.Fatal(err)
	}
	t := node.(pipeline.Transform)
	env := &pipeline.Env{}
	batch := benchBatch()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := t.Apply(env, batch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTransformCompiled(b *testing.B) {
	apply := func(batch *pipeline.Batch) {
		c := batch.Col("email")
		for i := range batch.Rows {
			if s, ok := batch.Rows[i][c].(string); ok {
				batch.Rows[i][c] = strings.ToLower(strings.TrimSpace(s))
			}
		}
	}
	batch := benchBatch()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		apply(batch)
	}
}

// BenchmarkTransformTextClean is the same work done by the built-in
// text.clean — what a node costs when no interpreted code is involved.
func BenchmarkTransformTextClean(b *testing.B) {
	p, _ := pipeline.Lookup("text.clean")
	node, err := p.New(p.Defaults(pipeline.Config{"columns": "email", "case": "lower"}))
	if err != nil {
		b.Fatal(err)
	}
	t := node.(pipeline.Transform)
	env := &pipeline.Env{}
	batch := benchBatch()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := t.Apply(env, batch); err != nil {
			b.Fatal(err)
		}
	}
}
