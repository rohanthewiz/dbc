//go:build ignore

// Posts the rows to a URL as one JSON document when the fragment ends:
// {"pipeline": …, "fragment": …, "rows": [{column: value, …}, …]}. A failed
// fragment posts nothing, and a reply other than 2xx fails it.
//
// A pipeline plugin: copy it into plugins_dir (~/.config/dbc/plugins) and
// webhook.post joins the palette under Yours, and `dbc plugins`.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

var Plugin = sdb.Plugin{
	Name:  "webhook.post",
	Kind:  sdb.KindSink,
	Label: "Post to a webhook",
	Fields: []sdb.Field{
		{Name: "url", Type: sdb.FieldString, Required: true, Doc: "Where to POST the rows."},
		{Name: "headers", Type: sdb.FieldText, Doc: "Extra request headers, one Name: value per line (an Authorization, say)."},
		{Name: "max_rows", Type: sdb.FieldInt, Default: "10000", Doc: "The most rows to send; more fails the fragment (they are held in memory until it ends)."},
		{Name: "timeout", Type: sdb.FieldDuration, Default: "30s", Doc: "How long the POST may take."},
	},
}

// rows waits here until Commit: a sink makes nothing visible before the
// fragment succeeds, so a failure part-way posts nothing at all.
var rows []map[string]any

func Write(e *sdb.Env, b *sdb.Batch) error {
	limit, err := e.Cfg.Int("max_rows", 10000)
	if err != nil {
		return err
	}
	if len(rows)+b.Len() > limit {
		return fmt.Errorf("more than max_rows (%d) rows to post", limit)
	}
	for i := range b.Rows {
		row := map[string]any{}
		for j, c := range b.Cols {
			row[c.Name] = b.Rows[i][j]
		}
		rows = append(rows, row)
	}
	return nil
}

// Commit sends the rows. Its Stats are what the run reports for the node.
func Commit(e *sdb.Env) (sdb.Stats, error) {
	body, err := json.Marshal(map[string]any{"pipeline": e.Pipeline, "fragment": e.Fragment, "rows": rows})
	if err != nil {
		return sdb.Stats{}, err
	}
	timeout, err := e.Cfg.Duration("timeout", 0)
	if err != nil {
		return sdb.Stats{}, err
	}
	url := e.Cfg.Str("url", "")
	req, err := http.NewRequestWithContext(e.Ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return sdb.Stats{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, line := range e.Cfg.Lines("headers") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return sdb.Stats{}, fmt.Errorf("a header is Name: value, not %q", line)
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return sdb.Stats{}, err
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return sdb.Stats{}, fmt.Errorf("%s answered %s: %s", url, resp.Status, strings.TrimSpace(string(reply)))
	}
	e.Logf("posted %d rows to %s (%s)", len(rows), url, resp.Status)
	return sdb.Stats{Rows: int64(len(rows)), Note: fmt.Sprintf("posted %d rows", len(rows))}, nil
}

// Abort is called instead of Commit when the fragment fails.
func Abort() error {
	rows = nil
	return nil
}
