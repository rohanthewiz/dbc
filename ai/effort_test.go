package ai

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// levelValues lists an Effort's level values, for compact comparisons.
func levelValues(e Effort) []string {
	var out []string
	for _, l := range e.Levels {
		out = append(out, l.Value)
	}
	return out
}

// awaitEffort reads events until an EventEffort satisfies ok, and returns
// it. Effort events are whole states, so the last one is what counts; a
// test waits for the state it expects rather than counting events.
func awaitEffort(t *testing.T, c *Chat, ok func(Effort) bool) Effort {
	t.Helper()
	for {
		e := next(t, c)
		switch e.Kind {
		case EventEffort:
			if ok(e.Effort) {
				return e.Effort
			}
		case EventExit:
			t.Fatalf("connection ended: %v", e.Err)
		}
	}
}

// configCalls returns the values of the set_config_option requests sent.
func (fa *fakeAgent) configCalls() []string {
	var out []string
	for _, m := range fa.received("session/set_config_option") {
		var p struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(m.Params, &p)
		out = append(out, p.Value)
	}
	return out
}

// The effort option is found by its category among the others, with its
// levels' values and names, and arrives with EventReady.
func TestReadyCarriesTheEffort(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w"})
	e := next(t, c)
	if e.Kind != EventReady {
		t.Fatalf("first event = %+v, want Ready", e)
	}
	want := Effort{ID: "reasoning_effort", Current: "medium", Levels: []EffortLevel{
		{"low", "Low"}, {"medium", "Medium"}, {"high", "High"}}}
	if e.Effort.ID != want.ID || e.Effort.Current != want.Current || !slices.Equal(e.Effort.Levels, want.Levels) {
		t.Errorf("effort = %+v, want %+v", e.Effort, want)
	}
	if e.Effort.Name() != "Medium" {
		t.Errorf("Name = %q", e.Effort.Name())
	}
	if n := len(fa.configCalls()); n != 0 {
		t.Errorf("no preference, yet %d set_config_option calls", n)
	}
}

// A preferred model and level: the level is asked for after the switch, the
// switch's own update (the new model's levels) is not dropped for arriving
// mid-handshake, and EventReady reports the outcome — not an EventEffort
// ahead of it.
func TestHandshakeAppliesThePreferredEffortAfterTheModel(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w", Model: "claude-sonnet", Effort: "max"})
	e := next(t, c)
	if e.Kind != EventReady {
		t.Fatalf("first event = %+v, want Ready", e)
	}
	if e.Effort.Current != "max" || len(e.Effort.Levels) != 4 {
		t.Errorf("effort = %+v, want max of claude-sonnet's four levels", e.Effort)
	}
	got := fa.received("session/set_config_option")
	if len(got) != 1 {
		t.Fatalf("set_config_option calls = %d, want 1", len(got))
	}
	if s := string(got[0].Params); s != `{"configId":"reasoning_effort","sessionId":"s1","value":"max"}` {
		t.Errorf("set_config_option params = %s", s)
	}
}

// Without a model switch the preference is only sent when it changes
// something the model offers; one it does not offer keeps the default.
func TestPreferredEffortWithoutAModelSwitch(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w", Effort: "high"})
	if e := next(t, c); e.Kind != EventReady || e.Effort.Current != "high" {
		t.Fatalf("event = %+v, want Ready at high", e)
	}
	if got := fa.configCalls(); !slices.Equal(got, []string{"high"}) {
		t.Errorf("set_config_option values = %v", got)
	}

	// gpt-4.1 has no "max": nothing is asked, nothing fails
	c, fa = startFake(t, Options{Dir: "/w", Effort: "max"})
	if e := next(t, c); e.Kind != EventReady || e.Effort.Current != "medium" {
		t.Fatalf("event = %+v, want Ready at the default", e)
	}
	if n := len(fa.configCalls()); n != 0 {
		t.Errorf("an unoffered level was sent %d times", n)
	}

	// the default already: nothing to ask
	c, fa = startFake(t, Options{Dir: "/w", Effort: "medium"})
	if e := next(t, c); e.Kind != EventReady {
		t.Fatalf("event = %+v", e)
	}
	if n := len(fa.configCalls()); n != 0 {
		t.Errorf("the level in force was asked for again %d times", n)
	}
}

// A preference the switched-to model refuses is not an error: the chat is
// ready, at that model's default, with that model's levels.
func TestRefusedPreferredEffortKeepsTheDefault(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w", Model: "claude-sonnet", Effort: "turbo"})
	e := next(t, c)
	if e.Kind != EventReady {
		t.Fatalf("first event = %+v, want Ready", e)
	}
	if e.Effort.Current != "medium" || len(e.Effort.Levels) != 4 {
		t.Errorf("effort = %+v, want claude-sonnet's default", e.Effort)
	}
	if got := fa.configCalls(); !slices.Equal(got, []string{"turbo"}) {
		t.Errorf("set_config_option values = %v", got)
	}
}

// SetEffort changes the level and reports it; a model switch then puts the
// chosen level back, and a model that lacks it leaves its own default.
func TestSetEffortSurvivesModelSwitches(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w"})
	if e := next(t, c); e.Kind != EventReady {
		t.Fatalf("first event = %+v", e)
	}
	if err := c.SetEffort("high"); err != nil {
		t.Fatal(err)
	}
	awaitEffort(t, c, func(e Effort) bool { return e.Current == "high" })

	// claude-sonnet resets to medium, then dbc asks for high again; by the
	// time SetModel returns, the state is final
	if err := c.SetModel("claude-sonnet"); err != nil {
		t.Fatal(err)
	}
	if e := c.Effort(); e.Current != "high" || len(e.Levels) != 4 {
		t.Errorf("after the switch effort = %+v, want high of four levels", e)
	}
	awaitEffort(t, c, func(e Effort) bool { return e.Current == "high" && len(e.Levels) == 4 })

	if err := c.SetEffort("max"); err != nil {
		t.Fatal(err)
	}
	// gpt-4.1 has no max: asked, refused, the default stands
	if err := c.SetModel("gpt-4.1"); err != nil {
		t.Fatalf("a refused re-application must not fail the switch: %v", err)
	}
	if e := c.Effort(); e.Current != "medium" || len(e.Levels) != 3 {
		t.Errorf("after the switch effort = %+v, want gpt-4.1's default", e)
	}
	if got := fa.configCalls(); !slices.Equal(got, []string{"high", "high", "max", "max"}) {
		t.Errorf("set_config_option values = %v", got)
	}
}

// A level the agent refuses is SetEffort's error, and it is not kept as the
// choice to re-apply.
func TestSetEffortRefusalIsAnError(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w"})
	if e := next(t, c); e.Kind != EventReady {
		t.Fatalf("first event = %+v", e)
	}
	if err := c.SetEffort("bogus"); err == nil {
		t.Fatal("a refused level must be an error")
	}
	if err := c.SetModel("claude-sonnet"); err != nil {
		t.Fatal(err)
	}
	if got := fa.configCalls(); !slices.Equal(got, []string{"bogus"}) {
		t.Errorf("set_config_option values = %v — the refused level was re-applied", got)
	}
}

// An agent with no configOptions offers no effort: the zero Effort,
// ErrNoEffort from SetEffort, and a preference that is never sent.
func TestAgentWithoutConfigOptions(t *testing.T) {
	c, fa := startFakeWith(t, Options{Dir: "/w", Model: "claude-sonnet", Effort: "high"},
		func(fa *fakeAgent) { fa.noConfig = true })
	e := next(t, c)
	if e.Kind != EventReady {
		t.Fatalf("first event = %+v", e)
	}
	if e.Effort.Offered() || e.Effort.ID != "" {
		t.Errorf("effort = %+v, want none", e.Effort)
	}
	if err := c.SetEffort("high"); !errors.Is(err, ErrNoEffort) {
		t.Errorf("SetEffort err = %v, want ErrNoEffort", err)
	}
	if n := len(fa.configCalls()); n != 0 {
		t.Errorf("set_config_option sent %d times to an agent without options", n)
	}
}

// An update without the effort option (a model without the setting) clears
// it; the choice made before is still restored once a model offers it
// again, through the option id remembered from before.
func TestEffortGoneAndBack(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w"})
	if e := next(t, c); e.Kind != EventReady {
		t.Fatalf("first event = %+v", e)
	}
	if err := c.SetEffort("high"); err != nil {
		t.Fatal(err)
	}
	awaitEffort(t, c, func(e Effort) bool { return e.Current == "high" })

	// as Copilot reports a switch to Claude Haiku 4.5: every option but effort
	fa.configUpdate([]any{map[string]any{"type": "select", "id": "mode", "category": "mode",
		"currentValue": "agent", "options": []any{map[string]any{"value": "agent", "name": "Agent"}}}})
	awaitEffort(t, c, func(e Effort) bool { return !e.Offered() })
	if err := c.SetEffort("high"); !errors.Is(err, ErrNoEffort) {
		t.Errorf("SetEffort with no option = %v, want ErrNoEffort", err)
	}

	if err := c.SetModel("claude-sonnet"); err != nil {
		t.Fatal(err)
	}
	if e := c.Effort(); e.Current != "high" || e.ID != "reasoning_effort" {
		t.Errorf("after the switch effort = %+v, want high restored", e)
	}
}

func TestSetEffortBeforeReady(t *testing.T) {
	c := &Chat{}
	if err := c.SetEffort("high"); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
}

func TestParseEffort(t *testing.T) {
	cases := []struct {
		name, raw string
		want      Effort
	}{
		{"none", `[]`, Effort{}},
		{"null", `null`, Effort{}},
		{"not a list", `{"id":"x"}`, Effort{}},
		{"other categories only", `[{"type":"select","id":"model","category":"model","currentValue":"a","options":[{"value":"a","name":"A"}]}]`, Effort{}},
		{"a boolean in the category is no level list",
			`[{"type":"boolean","id":"think","category":"thought_level","currentValue":true}]`, Effort{}},
		{"flat",
			`[{"type":"select","id":"reasoning_effort","category":"thought_level","currentValue":"high",
			  "options":[{"value":"low","name":"low"},{"value":"high","name":"high"}]}]`,
			Effort{ID: "reasoning_effort", Current: "high", Levels: []EffortLevel{{"low", "low"}, {"high", "high"}}}},
		{"grouped, in order",
			`[{"type":"select","id":"t","category":"thought_level","currentValue":"b",
			  "options":[{"group":"g1","name":"Fast","options":[{"value":"a","name":"A"}]},
			             {"group":"g2","name":"Deep","options":[{"value":"b","name":"B"},{"value":"c","name":"C"}]}]}]`,
			Effort{ID: "t", Current: "b", Levels: []EffortLevel{{"a", "A"}, {"b", "B"}, {"c", "C"}}}},
	}
	for _, tc := range cases {
		got := parseEffort(json.RawMessage(tc.raw))
		if got.ID != tc.want.ID || got.Current != tc.want.Current || !slices.Equal(got.Levels, tc.want.Levels) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestEffortName(t *testing.T) {
	e := Effort{ID: "x", Current: "hi", Levels: []EffortLevel{{"hi", "High"}, {"lo", ""}}}
	if e.Name() != "High" {
		t.Errorf("Name = %q", e.Name())
	}
	e.Current = "lo" // no name given: the value
	if e.Name() != "lo" {
		t.Errorf("Name = %q", e.Name())
	}
	if !e.Has("lo") || e.Has("mid") {
		t.Error("Has")
	}
	if (Effort{ID: "x"}).Offered() {
		t.Error("an option with no levels offers no choice")
	}
}
