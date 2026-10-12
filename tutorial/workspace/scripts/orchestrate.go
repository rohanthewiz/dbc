//go:build ignore

// Run a saved pipeline, then an ad-hoc one built in Go.
package main

import (
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	// A saved pipeline by name, with params.
	st, err := s.RunPipelineNamed("shop-sync", sdb.PipelineOpts{Params: sdb.Params{"min_total": "100"}})
	if err != nil {
		return err
	}
	s.Print("shop-sync: %s, %d rows", st.Status, st.Rows())

	// A pipeline built in code; ThenFunc is a transform written as a Go func.
	p := sdb.NewPipeline("emails")
	f := p.Fragment("domains")
	f.Node("sql.read", sdb.Cfg{"conn": "warehouse", "query": "SELECT id, email FROM dim_customer ORDER BY id"})
	f.ThenFunc(func(b *sdb.Batch) (*sdb.Batch, error) {
		b.AddCol("domain", "text", func(row int) any {
			e, _ := b.Get(row, "email").(string)
			_, d, _ := strings.Cut(e, "@")
			return d
		})
		return b, nil
	})
	f.Then("preview", sdb.Cfg{"rows": "10"})
	st, err = s.RunPipeline(p, sdb.PipelineOpts{})
	if err != nil {
		return err
	}
	s.Print("emails: %s", st.Status)

	// A whole job, waited for.
	run, err := s.RunJob("nightly-shop", sdb.Params{"min_total": "50"})
	if err != nil {
		return err
	}
	for _, p := range run.Pipelines {
		s.Print("  step %-8s %-10s %d rows", p.ID, p.Status, p.Rows())
	}
	s.Print("job %s: %s", run.Job, run.Status)
	return nil
}
