package explain

import "encoding/json"

// Document is a plan as JSON consumers see it: the plan itself plus the few
// things every consumer would otherwise recompute — its headline and the
// metrics it can be viewed by. It is what `dbc explain -t json` prints and
// what the HTML view embeds.
type Document struct {
	*Plan
	Headline string   `json:"headline"`
	Metrics  []Metric `json:"metrics"`
}

// JSON renders the plan as an indented Document.
func (p *Plan) JSON() ([]byte, error) {
	return json.MarshalIndent(Document{Plan: p, Headline: p.Headline(), Metrics: p.Metrics()}, "", "  ")
}
