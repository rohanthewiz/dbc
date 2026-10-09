//go:build ignore

// Waits until a file appears — an export another system drops, a done
// marker — so the fragments after it start only once it is there. A gate
// at the head of a pipeline or a job's first step.
//
// A pipeline plugin: copy it into plugins_dir (~/.config/dbc/plugins) and
// wait.file joins the palette under Yours, and `dbc plugins`.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/rohanthewiz/dbc/sdb"
)

var Plugin = sdb.Plugin{
	Name:  "wait.file",
	Kind:  sdb.KindAction,
	Label: "Wait for a file",
	Fields: []sdb.Field{
		{Name: "path", Type: sdb.FieldString, Required: true, Doc: "The file to wait for; ${run.date} and params work here."},
		{Name: "timeout", Type: sdb.FieldDuration, Default: "10m", Doc: "How long to wait before failing."},
		{Name: "every", Type: sdb.FieldDuration, Default: "5s", Doc: "How often to look."},
	},
}

// Run is an action's one call. The Stats' Vars are read by later
// fragments as ${frag.<this fragment>.size}.
func Run(e *sdb.Env) (sdb.Stats, error) {
	path := e.Cfg.Str("path", "")
	timeout, err := e.Cfg.Duration("timeout", 10*time.Minute)
	if err != nil {
		return sdb.Stats{}, err
	}
	every, err := e.Cfg.Duration("every", 5*time.Second)
	if err != nil {
		return sdb.Stats{}, err
	}
	deadline := time.Now().Add(timeout)
	for {
		if st, err := os.Stat(path); err == nil {
			e.Logf("%s is there (%d bytes)", path, st.Size())
			return sdb.Stats{Vars: map[string]string{"size": strconv.FormatInt(st.Size(), 10)}}, nil
		}
		if time.Now().After(deadline) {
			return sdb.Stats{}, fmt.Errorf("%s did not appear within %s", path, timeout)
		}
		// Stop (Ctrl+K, ■, a job's timeout) cancels e.Ctx: give up at once
		select {
		case <-e.Ctx.Done():
			return sdb.Stats{}, e.Ctx.Err()
		case <-time.After(every):
		}
	}
}
