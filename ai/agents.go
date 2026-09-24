// Package ai is dbc's AI chat assistance: a conversation with a coding agent
// about the query in the editor and the result it produced.
//
// WHY AN AGENT PROCESS AND NOT AN HTTP API. dbc does not talk to any model
// provider itself. It runs an agent binary that speaks the Agent Client
// Protocol (ACP) over stdio — GitHub's copilot-language-server by default —
// the same way ced (github.com/rohanthewiz/ced) does. That buys three things
// dbc would otherwise have to build and maintain:
//
//   - AUTH IS THE AGENT'S. Copilot's OAuth tokens, refresh and keychain
//     storage live in the binary, and are shared with every editor the user
//     already signed in from. dbc stores no credential and reads no key.
//   - IT IS THE SUPPORTED PATH. ACP is how Zed and JetBrains host Copilot
//     chat; api.githubcopilot.com is an internal endpoint that can change
//     under anyone calling it directly.
//   - OTHER BACKENDS COME FREE. Any ACP agent is a drop-in: Claude Code and
//     Gemini are registered below and differ only in the binary name.
//
// The package is UI-agnostic. A Chat reports everything that happens as
// Events on a channel, so any UI — today the Bubble Tea one in package tui —
// consumes it the same way.
package ai

import "os/exec"

// Agent describes one ACP backend.
type Agent struct {
	ID     string   // stable config value (ai_agent)
	Name   string   // display name for the pane title and transcript
	Binary string   // executable resolved on PATH; its presence is the opt-in
	Args   []string // argv after the binary

	// Install says how to get the binary, shown when it is missing — the chat
	// pane is the one place a missing integration is worth explaining, since
	// the user opened it on purpose.
	Install string

	// Auth says how to sign in, shown when the agent refuses for lack of it.
	Auth string
}

// CopilotID is the default agent.
const CopilotID = "copilot"

// Agents lists the known backends, default first.
func Agents() []Agent {
	return []Agent{
		{
			ID: CopilotID, Name: "Copilot",
			Binary: "copilot-language-server", Args: []string{"--acp"},
			Install: "npm install -g @github/copilot-language-server",
			Auth: "sign in to GitHub Copilot once from any editor that uses the language server " +
				"(ced, VS Code, Neovim) — the credential is shared",
		},
		{
			ID: "claude", Name: "Claude Code",
			Binary:  "claude-code-acp",
			Install: "npm install -g @zed-industries/claude-code-acp",
			Auth:    "run `claude` once to log in, or set ANTHROPIC_API_KEY",
		},
		{
			ID: "gemini", Name: "Gemini",
			Binary: "gemini", Args: []string{"--experimental-acp"},
			Install: "npm install -g @google/gemini-cli",
			Auth:    "run `gemini` once to log in, or set GEMINI_API_KEY",
		},
	}
}

// AgentByID resolves a configured agent id. Empty means the default, and so
// does an unknown id — with ok=false so the caller can say it fell back,
// because a typo in ai_agent should not silently look like a choice.
func AgentByID(id string) (a Agent, ok bool) {
	all := Agents()
	if id == "" {
		return all[0], true
	}
	for _, a := range all {
		if a.ID == id {
			return a, true
		}
	}
	return all[0], false
}

// lookPath is exec.LookPath, as a var so tests can pretend a binary is or is
// not installed without touching PATH.
var lookPath = exec.LookPath

// Installed reports whether the agent's binary is on PATH.
func (a Agent) Installed() bool {
	_, err := lookPath(a.Binary)
	return err == nil
}
