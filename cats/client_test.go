package cats

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCallSendsTheEnvelopeAndDecodesTheResult(t *testing.T) {
	path := sockPath(t, "c.sock")
	got := serveOnce(t, path, func(map[string]any) any {
		return okReply(ConfigGetResult{
			Path:  "/home/u/.cats.yaml",
			Theme: ConfigTheme{Name: "cats-green", Colors: map[string]string{"bg": "#0b0f0c"}},
		})
	})

	res, err := NewClient(path).ConfigGet()
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if res.Theme.Name != "cats-green" || res.Theme.Colors["bg"] != "#0b0f0c" {
		t.Errorf("theme not decoded: %+v", res.Theme)
	}

	if got.len() != 1 {
		t.Fatalf("expected one request, got %d", got.len())
	}
	req := got.at(t, 0)
	if req["method"] != MethodConfigGet {
		t.Errorf("method = %v, want %v", req["method"], MethodConfigGet)
	}
	if req["id"] != "dbc" {
		t.Errorf(`id = %v, want "dbc"`, req["id"])
	}
}

// ok:false is the server refusing, which must surface as an error carrying
// the server's own words — a caller falling back to Tier 0 logs this.
func TestCallSurfacesAServerRefusal(t *testing.T) {
	path := sockPath(t, "c.sock")
	serveOnce(t, path, func(map[string]any) any {
		return map[string]any{"ok": false, "error": "unknown pane"}
	})

	err := NewClient(path).PaneFocus(9)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unknown pane") {
		t.Errorf("the server's message was lost: %v", err)
	}
}

func TestCallWithoutASocketFails(t *testing.T) {
	if err := NewClient("").PaneFocus(1); err == nil {
		t.Error("a client with no socket should refuse rather than dial")
	}
	var nilClient *Client
	if err := nilClient.Call(MethodPing, nil, nil); err == nil {
		t.Error("a nil client should refuse rather than panic")
	}
}

// A reply can exceed bufio's default 4KB line. Reading a line rather than
// decoding a stream would truncate it, which is why Call uses json.Decoder.
func TestCallHandlesALargeReply(t *testing.T) {
	path := sockPath(t, "c.sock")
	panes := make([]PaneInfo, 200)
	for i := range panes {
		panes[i] = PaneInfo{
			Pane:   uint32(i),
			Handle: "w1:p" + strings.Repeat("0", 40),
			Title:  strings.Repeat("x", 80),
		}
	}
	serveOnce(t, path, func(map[string]any) any {
		return okReply(PaneListResult{Panes: panes})
	})

	got, err := NewClient(path).PaneList()
	if err != nil {
		t.Fatalf("PaneList: %v", err)
	}
	if len(got) != len(panes) {
		t.Errorf("got %d panes, want %d — a large reply was truncated", len(got), len(panes))
	}
}

// cats flattens the agent metadata into the pane object rather than nesting
// it, so the mirror struct has to inline those fields.
func TestPaneInfoDecodesFlattenedAgentMeta(t *testing.T) {
	var p PaneInfo
	raw := `{"pane":26,"handle":"w1:p2","focused":true,"visible":true,
	         "agent":"claude","agent_state":"blocked","title":"claude","cwd":"/tmp"}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.Agent != "claude" || p.AgentState != "blocked" {
		t.Errorf("agent meta not flattened into PaneInfo: %+v", p)
	}
}

func TestPaneSendInputStagesWithoutSubmitting(t *testing.T) {
	path := sockPath(t, "c.sock")
	got := serveOnce(t, path, func(map[string]any) any { return okReply(nil) })

	if err := NewClient(path).PaneSendInput(7, "SELECT 1", false); err != nil {
		t.Fatalf("PaneSendInput: %v", err)
	}
	params := got.at(t, 0)["params"].(map[string]any)
	if params["text"] != "SELECT 1" {
		t.Errorf("text = %v", params["text"])
	}
	// submit is omitempty, so staged input carries no submit key at all —
	// which is the point: dbc never presses Enter in someone else's pane.
	if _, ok := params["submit"]; ok {
		t.Errorf("staged input must not submit, got %v", params["submit"])
	}
}

// The "p_<n>" form is decoded locally; anything else costs one pane.list.
func TestResolvePane(t *testing.T) {
	path := sockPath(t, "c.sock")
	got := serveOnce(t, path, func(map[string]any) any {
		return okReply(PaneListResult{Panes: []PaneInfo{
			{Pane: 12, Handle: "w1:p1"},
			{Pane: 289, Handle: "w1:p3"},
		}})
	})
	c := NewClient(path)

	id, err := c.ResolvePane("p_41")
	if err != nil || id != 41 {
		t.Errorf(`ResolvePane("p_41") = %d,%v; want 41,nil`, id, err)
	}
	if got.len() != 0 {
		t.Error("the local form should not cost a round trip")
	}

	id, err = c.ResolvePane("w1:p3")
	if err != nil || id != 289 {
		t.Errorf(`ResolvePane("w1:p3") = %d,%v; want 289,nil`, id, err)
	}

	if _, err = c.ResolvePane("w9:p9"); err == nil {
		t.Error("an unknown handle should be an error, not pane 0")
	}
}

func TestChatSendCarriesTheText(t *testing.T) {
	path := sockPath(t, "c.sock")
	got := serveOnce(t, path, func(map[string]any) any { return okReply(nil) })

	if err := NewClient(path).ChatSend("why is this slow?"); err != nil {
		t.Fatalf("ChatSend: %v", err)
	}
	params := got.at(t, 0)["params"].(map[string]any)
	if params["text"] != "why is this slow?" {
		t.Errorf("text = %v", params["text"])
	}
}
