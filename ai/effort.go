package ai

import (
	"encoding/json"
	"errors"
)

// Reasoning effort: how hard the model thinks before it answers.
//
// WHERE IT COMES FROM. ACP has no effort method of its own. An agent offers
// effort the way it offers any other per-session setting: as one of the
// session's "config options" (session/new's configOptions), changed with
// session/set_config_option. The spec reserves the category "thought_level"
// for this one, so that is what dbc looks for — never an id, since every
// agent names its option differently (Copilot's is "reasoning_effort").
//
// IT BELONGS TO THE MODEL, NOT THE SESSION. What the agent offers depends on
// the model in force. Copilot, measured against copilot-language-server
// 1.526, on a model switch:
//
//	model             levels offered                   after the switch
//	Claude Opus 5.5   low medium high xhigh max        medium
//	GPT-5.3 Codex     low medium high xhigh            medium
//	Gemini 3.8 Flash  low medium high                  medium
//	Claude Haiku 4.5  (no option at all)               —
//
// A switch therefore resets effort to the agent's default, and the new list
// arrives AFTER the switch's response, as a session/update notification
// ("config_option_update") carrying every option again. So the Chat:
//
//   - keeps the latest Effort the agent reported, from session/new, from a
//     set_config_option response, or from that notification, whichever is
//     newest — the read loop delivers notifications in wire order, ahead of
//     any response read after them, so "newest applied" is "newest sent";
//   - remembers the level the user CHOSE (Options.Effort, then SetEffort),
//     and asks for it again after every model switch. A model that does not
//     offer it refuses, and the agent's default stands — visible in the UI,
//     never an error, as a stale ai_model is never one either.

// Effort is the agent's reasoning-effort setting for the model in force.
// The zero value means it offers none (an agent without config options, or
// a model without the setting).
type Effort struct {
	ID      string        // the config option's id, as set_config_option names it
	Current string        // the level in force (a Levels value)
	Levels  []EffortLevel // what the model accepts, in the agent's order (lowest first, for Copilot)
}

// EffortLevel is one choice of an Effort.
type EffortLevel struct {
	Value string // what set_config_option sends
	Name  string // what a menu shows
}

// Offered reports whether there is a choice to make.
func (e Effort) Offered() bool { return e.ID != "" && len(e.Levels) > 0 }

// Has reports whether the model accepts the level value.
func (e Effort) Has(value string) bool {
	for _, l := range e.Levels {
		if l.Value == value {
			return true
		}
	}
	return false
}

// Name is the display name of the level in force: its own name, or the
// value when the agent gave none (or reported a value outside its list).
func (e Effort) Name() string {
	for _, l := range e.Levels {
		if l.Value == e.Current && l.Name != "" {
			return l.Name
		}
	}
	return e.Current
}

// ErrNoEffort is SetEffort's answer when the model in force offers no
// effort setting.
var ErrNoEffort = errors.New("this model has no reasoning-effort setting")

// thoughtLevel is ACP's reserved category for the effort option.
const thoughtLevel = "thought_level"

// configOption is the slice of an ACP SessionConfigOption dbc reads. Its
// options are either a flat list of values or a list of named groups of
// them (SessionConfigSelectGroup); json.RawMessage defers the choice until
// the shape is known.
type configOption struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Category     string          `json:"category"`
	CurrentValue json.RawMessage `json:"currentValue"`
	Options      json.RawMessage `json:"options"`
}

// selectEntry is either a value (Value set) or a group of them (Options set).
type selectEntry struct {
	Value   string        `json:"value"`
	Name    string        `json:"name"`
	Options []selectEntry `json:"options"`
}

// parseEffort finds the effort option in a configOptions list. A list with
// none — or raw that is not a list — yields the zero Effort; that is a fact
// to report (the model has no setting), not an error.
func parseEffort(raw json.RawMessage) Effort {
	var opts []configOption
	if json.Unmarshal(raw, &opts) != nil {
		return Effort{}
	}
	for _, o := range opts {
		// "select" is the only shape that holds levels; a boolean option in
		// the category could only be "think or not", which no agent sends
		// today and a level menu cannot show.
		if o.Category != thoughtLevel || o.ID == "" || (o.Type != "" && o.Type != "select") {
			continue
		}
		e := Effort{ID: o.ID}
		_ = json.Unmarshal(o.CurrentValue, &e.Current) // a non-string leaves "", shown as no level picked
		var entries []selectEntry
		_ = json.Unmarshal(o.Options, &entries)
		e.Levels = flattenLevels(entries, nil)
		return e
	}
	return Effort{}
}

// flattenLevels lists the values of a select's options, groups opened in
// place. Grouping is presentation the agent may use for a long list; a level
// menu shows the levels in order either way.
func flattenLevels(entries []selectEntry, into []EffortLevel) []EffortLevel {
	for _, s := range entries {
		switch {
		case s.Value != "":
			into = append(into, EffortLevel{Value: s.Value, Name: s.Name})
		case len(s.Options) > 0:
			into = flattenLevels(s.Options, into)
		}
	}
	return into
}

// setEffort records what the agent reported and, once the conversation is
// ready, tells the UI. Before then EventReady will carry it.
//
// emitMu is held across the record and the emit, and connect holds it
// across marking the chat ready and sending EventReady, so the events go
// out in the order the states were recorded. Without it an update landing
// just as the handshake finishes could be emitted ahead of EventReady,
// whose older snapshot would then overwrite it in the UI.
func (c *Chat) setEffort(e Effort) {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.mu.Lock()
	c.effort = e
	if e.ID != "" {
		c.effortID = e.ID
	}
	ready := c.ready
	c.mu.Unlock()
	if ready {
		c.emit(Event{Kind: EventEffort, Effort: e})
	}
}

// applyEffort asks the agent for an effort level and records the options
// its answer carries. The caller decides whether a refusal matters: to
// SetEffort it is the user's error to show, to a re-application after a
// model switch it only means the new model lacks that level.
func (c *Chat) applyEffort(conn *rpcConn, sid, configID, value string) error {
	var res struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	err := conn.call("session/set_config_option",
		map[string]any{"sessionId": sid, "configId": configID, "value": value}, &res, sessionTimeout)
	if err != nil {
		return err
	}
	// The spec has the response carry every option's new state. An agent
	// that answers {} instead leaves the update to its notification.
	if len(res.ConfigOptions) > 0 && string(res.ConfigOptions) != "null" {
		c.setEffort(parseEffort(res.ConfigOptions))
	}
	return nil
}

// reapplyEffort asks again for the level the user chose, after a model
// switch has put the agent's default back. Errors are dropped: the new
// model may not offer the level, and then its default is the right answer.
//
// The option's id is the last one ever seen, not the current one: a switch
// from a model WITHOUT the setting (current id "") to one with it must still
// restore the choice, and the new model's option has not been reported yet
// when this runs.
func (c *Chat) reapplyEffort(conn *rpcConn, sid string) {
	c.mu.Lock()
	want, id := c.wantEffort, c.effortID
	c.mu.Unlock()
	if want == "" || id == "" {
		return
	}
	_ = c.applyEffort(conn, sid, id, want)
}

// Effort returns the effort setting as last reported.
func (c *Chat) Effort() Effort {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.effort
}

// SetEffort sets the reasoning effort for the model in force, and keeps the
// choice for later model switches. It blocks for the round trip, so a UI
// calls it off its event loop. The new state also arrives as EventEffort.
func (c *Chat) SetEffort(value string) error {
	c.mu.Lock()
	if !c.ready || c.conn == nil {
		c.mu.Unlock()
		return ErrNotReady
	}
	conn, sid, id := c.conn, c.sessionID, c.effort.ID
	c.mu.Unlock()
	if id == "" {
		return ErrNoEffort
	}
	if err := c.applyEffort(conn, sid, id, value); err != nil {
		return err
	}
	c.mu.Lock()
	c.wantEffort = value
	c.mu.Unlock()
	return nil
}
