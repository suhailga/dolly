package mcpserver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tmux-manager/registry"
	"tmux-manager/tmux"
)

// toolDef is one MCP tool advertised in tools/list.
type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

// obj is a tiny helper for building JSON-schema fragments inline.
type obj = map[string]interface{}

func toolDefinitions() []toolDef {
	return []toolDef{
		{
			Name: "list_sessions",
			Description: "List tmux sessions with type, live window count, working directory, " +
				"and alive status. Includes both dolly-managed sessions and sessions a user " +
				"created by hand (marked \"managed\": false, type \"unmanaged\"). Window counts " +
				"are queried live, so windows added manually are reflected. Start here to " +
				"discover which sessions exist.",
			InputSchema: obj{
				"type":       "object",
				"properties": obj{},
			},
		},
		{
			Name: "list_panes",
			Description: "List every window and pane in a session — queried live from tmux, so " +
				"panes and windows added manually (not from a dolly YAML config) are always " +
				"included. Each pane reports the command currently running, its working " +
				"directory, pane id, title, start_command, and whether it is active. " +
				"Notes for identifying a manually-added pane: pane ids are assigned in creation " +
				"order (a higher \"%N\" was created later); dolly config panes have a meaningful " +
				"title and a start_command like \"zsh -l\", whereas hand-split panes typically " +
				"show the hostname as title and a start_command like \"exec /bin/zsh\". " +
				"Use this to understand a session's layout and find the pane id you want to read.",
			InputSchema: obj{
				"type": "object",
				"properties": obj{
					"session": obj{
						"type":        "string",
						"description": "Name of the tmux session (from list_sessions).",
					},
					"compact": obj{
						"type":        "boolean",
						"description": "Optional: return only pane_id, window, title, command, and cwd per pane — skips pid/size/start_command. Use to avoid a wall of metadata.",
					},
					"filter": obj{
						"type":        "string",
						"description": "Optional: case-insensitive substring; only panes whose title, command, or cwd contain it are returned. e.g. \"ai\" to find the ai-service pane.",
					},
				},
				"required": []string{"session"},
			},
		},
		{
			Name: "read_pane",
			Description: "Read the contents (output/logs) of a single pane as full, untruncated " +
				"lines. Target a pane by its tmux pane id (e.g. \"%3\", from list_panes) or a " +
				"\"session:window.pane\" reference. By default returns the visible screen; set " +
				"history_lines to include that many lines of scrollback. Alternatively pass a " +
				"start/end range to read a specific scrollback block (tmux line coordinates: 0 = " +
				"top of screen, negative = scrollback history) — useful to expand a search_panes hit.",
			InputSchema: obj{
				"type": "object",
				"properties": obj{
					"pane": obj{
						"type":        "string",
						"description": "Pane id like \"%3\", or a target like \"my-session:1.0\".",
					},
					"history_lines": obj{
						"type":        "integer",
						"description": "Optional: number of scrollback lines to include above the visible screen. Omit or 0 for just the visible screen. Ignored if start/end are given.",
						"minimum":     0,
					},
					"start": obj{
						"type":        "integer",
						"description": "Optional range start as a tmux line coordinate (0 = top of visible screen, negative = scrollback). Requires end.",
					},
					"end": obj{
						"type":        "integer",
						"description": "Optional range end as a tmux line coordinate (0 = bottom; negative = scrollback). Requires start.",
					},
				},
				"required": []string{"pane"},
			},
		},
		{
			Name: "tail_pane",
			Description: "Incrementally tail a pane's output for live monitoring. The first call " +
				"starts tailing and returns the current screen as context; each later call " +
				"returns ONLY the output produced since the previous call (lossless — backed " +
				"by tmux pipe-pane, so nothing is missed between calls). Poll it in a loop to " +
				"stream a pane's logs. Pass {\"stop\": true} to stop tailing and free resources. " +
				"Target the pane the same way as read_pane.",
			InputSchema: obj{
				"type": "object",
				"properties": obj{
					"pane": obj{
						"type":        "string",
						"description": "Pane id like \"%3\", or a target like \"my-session:1.0\".",
					},
					"lines": obj{
						"type":        "integer",
						"description": "Optional: number of current screen lines to return as context on the first call. Default 40.",
						"minimum":     0,
					},
					"stop": obj{
						"type":        "boolean",
						"description": "Set true to stop tailing this pane and release the buffer.",
					},
				},
				"required": []string{"pane"},
			},
		},
		{
			Name: "search_panes",
			Description: "Search every pane in a session for a regular expression and return the " +
				"matching lines (full and untruncated) with their pane id, window, and line number. " +
				"Useful for finding errors or activity across a whole session without reading each " +
				"pane. Set context to also return the surrounding lines (like grep -C) — handy for " +
				"seeing what a tool call returned. Set tail to search only the most recent N lines " +
				"per pane (scope to the latest run). Matches are capped by max_matches (newest kept).",
			InputSchema: obj{
				"type": "object",
				"properties": obj{
					"session": obj{
						"type":        "string",
						"description": "Name of the tmux session to search.",
					},
					"pattern": obj{
						"type":        "string",
						"description": "Regular expression (Go/RE2 syntax) to match against each full line.",
					},
					"history_lines": obj{
						"type":        "integer",
						"description": "Optional: scrollback depth per pane to search. Defaults to 2000.",
						"minimum":     0,
					},
					"context": obj{
						"type":        "integer",
						"description": "Optional: include this many lines before and after each match (like grep -C). Default 0.",
						"minimum":     0,
					},
					"tail": obj{
						"type":        "integer",
						"description": "Optional: search only the last N lines of each pane (most recent output), to scope to the latest activity.",
						"minimum":     0,
					},
					"max_matches": obj{
						"type":        "integer",
						"description": "Optional: cap on matches returned (most recent kept). Default 200.",
						"minimum":     1,
					},
				},
				"required": []string{"session", "pattern"},
			},
		},
		{
			Name: "wait_for_pattern",
			Description: "Block until a pane's output matches a regular expression, or until a " +
				"timeout. Returns the matching line (with optional context) as soon as it appears, " +
				"or a timeout result. Use this instead of a sleep/poll loop to wait for a signal " +
				"such as \"status=ok\", a worker-finished line, or an error — it collapses the " +
				"poll-and-recheck dance into one call.",
			InputSchema: obj{
				"type": "object",
				"properties": obj{
					"pane": obj{
						"type":        "string",
						"description": "Pane id like \"%3\", or a target like \"my-session:1.0\".",
					},
					"pattern": obj{
						"type":        "string",
						"description": "Regular expression (Go/RE2 syntax) to wait for.",
					},
					"timeout_seconds": obj{
						"type":        "integer",
						"description": "Max seconds to wait before giving up. Default 30, max 300. (Your MCP client may impose its own call timeout.)",
						"minimum":     1,
						"maximum":     300,
					},
					"context": obj{
						"type":        "integer",
						"description": "Optional: lines of surrounding context to return with the match. Default 0.",
						"minimum":     0,
					},
				},
				"required": []string{"pane", "pattern"},
			},
		},
	}
}

// callParams is the params object of a tools/call request.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolCall dispatches a tools/call to the right tool. It returns a
// protocol error only for malformed calls (unknown tool / bad arguments);
// execution failures are returned as a normal result with isError set so the
// model can read the message.
func (s *Server) handleToolCall(params json.RawMessage) (interface{}, *rpcError) {
	var cp callParams
	if err := json.Unmarshal(params, &cp); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}

	switch cp.Name {
	case "list_sessions":
		return toolListSessions()
	case "list_panes":
		return toolListPanes(cp.Arguments)
	case "read_pane":
		return toolReadPane(cp.Arguments)
	case "tail_pane":
		return s.tails.tail(cp.Arguments)
	case "search_panes":
		return toolSearchPanes(cp.Arguments)
	case "wait_for_pattern":
		return toolWaitForPattern(cp.Arguments)
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + cp.Name}
	}
}

// ── result helpers ────────────────────────────────────────────────────────────

// textResult wraps plain text as a successful tool result.
func textResult(text string) interface{} {
	return obj{
		"content": []obj{{"type": "text", "text": text}},
	}
}

// jsonResult marshals v and returns it as pretty-printed text content.
func jsonResult(v interface{}) (interface{}, *rpcError) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: "could not encode result: " + err.Error()}
	}
	return textResult(string(b)), nil
}

// errorResult reports a tool execution failure to the model (not a protocol error).
func errorResult(format string, a ...interface{}) interface{} {
	return obj{
		"content": []obj{{"type": "text", "text": fmt.Sprintf(format, a...)}},
		"isError": true,
	}
}

// ── tool implementations ───────────────────────────────────────────────────────

func toolListSessions() (interface{}, *rpcError) {
	type sessionOut struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Managed    bool   `json:"managed"` // false => running tmux session not (yet) tracked by dolly
		Alive      bool   `json:"alive"`
		Windows    int    `json:"windows"` // live window count for alive sessions, registry value otherwise
		WorkingDir string `json:"working_dir"`
		Terminal   string `json:"terminal"`
	}

	sessions, err := registry.ListSessions()
	if err != nil {
		return errorResult("could not list sessions: %v", err), nil
	}

	// Live tmux sessions — the source of truth for what is actually running,
	// including sessions and windows created by hand outside of dolly.
	live, _ := tmux.ListSessions()
	liveSet := make(map[string]bool, len(live))
	for _, n := range live {
		liveSet[n] = true
	}

	managed := make(map[string]bool, len(sessions))
	out := make([]sessionOut, 0, len(sessions)+len(live))

	for _, s := range sessions {
		managed[s.Name] = true
		windows := s.Windows
		// For alive sessions, re-query tmux so manually-added windows are reflected.
		if s.Alive {
			if w, _, derr := tmux.GetSessionDetails(s.Name); derr == nil {
				windows = w
			}
		}
		out = append(out, sessionOut{
			Name:       s.Name,
			Type:       string(s.Type),
			Managed:    true,
			Alive:      s.Alive,
			Windows:    windows,
			WorkingDir: s.WorkingDir,
			Terminal:   s.Terminal,
		})
	}

	// Surface running tmux sessions dolly does not yet track, so an LLM can still
	// inspect sessions a user created entirely by hand.
	for _, name := range live {
		if managed[name] {
			continue
		}
		windows, workingDir, _ := tmux.GetSessionDetails(name)
		out = append(out, sessionOut{
			Name:       name,
			Type:       "unmanaged",
			Managed:    false,
			Alive:      true,
			Windows:    windows,
			WorkingDir: workingDir,
			Terminal:   tmux.DetectShell(),
		})
	}

	return jsonResult(out)
}

func toolListPanes(args json.RawMessage) (interface{}, *rpcError) {
	var a struct {
		Session string `json:"session"`
		Compact bool   `json:"compact"`
		Filter  string `json:"filter"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(a.Session) == "" {
		return errorResult("the 'session' argument is required"), nil
	}

	windows, err := tmux.ListWindows(a.Session)
	if err != nil {
		return errorResult("%v", err), nil
	}

	filter := strings.ToLower(strings.TrimSpace(a.Filter))
	matchesFilter := func(p tmux.PaneInfo) bool {
		if filter == "" {
			return true
		}
		hay := strings.ToLower(p.Title + " " + p.CurrentCommand + " " + p.CurrentPath)
		return strings.Contains(hay, filter)
	}

	outWindows := make([]obj, 0, len(windows))
	for _, w := range windows {
		compactPanes := make([]obj, 0, len(w.Panes))
		fullPanes := make([]tmux.PaneInfo, 0, len(w.Panes))
		for _, p := range w.Panes {
			if !matchesFilter(p) {
				continue
			}
			fullPanes = append(fullPanes, p)
			compactPanes = append(compactPanes, obj{
				"pane_id":         p.PaneID,
				"title":           p.Title,
				"current_command": p.CurrentCommand,
				"current_path":    p.CurrentPath,
				"active":          p.Active,
			})
		}
		if len(fullPanes) == 0 {
			continue // drop windows with no matching panes
		}
		win := obj{"index": w.Index, "name": w.Name, "active": w.Active}
		if a.Compact {
			win["panes"] = compactPanes
		} else {
			win["panes"] = fullPanes
		}
		outWindows = append(outWindows, win)
	}

	return jsonResult(obj{"session": a.Session, "windows": outWindows})
}

func toolReadPane(args json.RawMessage) (interface{}, *rpcError) {
	var a struct {
		Pane         string `json:"pane"`
		HistoryLines int    `json:"history_lines"`
		Start        *int   `json:"start"`
		End          *int   `json:"end"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(a.Pane) == "" {
		return errorResult("the 'pane' argument is required (a pane id like \"%%3\" or a \"session:window.pane\" target)"), nil
	}

	var content string
	var err error
	switch {
	case a.Start != nil && a.End != nil:
		content, err = tmux.CapturePaneRange(a.Pane, *a.Start, *a.End)
	case a.Start != nil || a.End != nil:
		return errorResult("'start' and 'end' must be provided together"), nil
	default:
		content, err = tmux.CapturePane(a.Pane, a.HistoryLines)
	}
	if err != nil {
		return errorResult("%v", err), nil
	}
	if content == "" {
		return textResult("(pane is empty — no output captured)"), nil
	}
	return textResult(content), nil
}

const defaultSearchHistory = 2000

const defaultMaxMatches = 200

type searchMatch struct {
	PaneID     string `json:"pane_id"`
	WindowName string `json:"window_name"`
	LineNumber int    `json:"line_number"`
	Text       string `json:"text"`
	Context    string `json:"context,omitempty"`
}

func toolSearchPanes(args json.RawMessage) (interface{}, *rpcError) {
	var a struct {
		Session      string `json:"session"`
		Pattern      string `json:"pattern"`
		HistoryLines *int   `json:"history_lines"`
		Context      int    `json:"context"`
		Tail         int    `json:"tail"`
		MaxMatches   *int   `json:"max_matches"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(a.Session) == "" || a.Pattern == "" {
		return errorResult("both 'session' and 'pattern' arguments are required"), nil
	}

	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return errorResult("invalid regular expression: %v", err), nil
	}

	history := defaultSearchHistory
	if a.HistoryLines != nil {
		history = *a.HistoryLines
	}
	maxMatches := defaultMaxMatches
	if a.MaxMatches != nil && *a.MaxMatches > 0 {
		maxMatches = *a.MaxMatches
	}

	panes, err := tmux.ListPanes(a.Session)
	if err != nil {
		return errorResult("%v", err), nil
	}

	var matches []searchMatch
	for _, p := range panes {
		content, err := tmux.CapturePane(p.PaneID, history)
		if err != nil {
			continue // skip panes we cannot read rather than failing the whole search
		}
		matches = append(matches, searchLines(strings.Split(content, "\n"), re, a.Tail, a.Context, p.PaneID, p.WindowName)...)
	}

	// Cap the result set, keeping the most recent matches (later panes/lines).
	total := len(matches)
	truncated := false
	if len(matches) > maxMatches {
		matches = matches[len(matches)-maxMatches:]
		truncated = true
	}

	return jsonResult(obj{
		"session":       a.Session,
		"pattern":       a.Pattern,
		"total_matches": total,
		"returned":      len(matches),
		"truncated":     truncated,
		"matches":       matches,
	})
}

// searchLines finds regex matches in lines, optionally limited to the last
// `tail` lines and including `context` lines on either side of each match.
func searchLines(lines []string, re *regexp.Regexp, tail, context int, paneID, windowName string) []searchMatch {
	start := 0
	if tail > 0 && len(lines) > tail {
		start = len(lines) - tail
	}
	var out []searchMatch
	for i := start; i < len(lines); i++ {
		if !re.MatchString(lines[i]) {
			continue
		}
		m := searchMatch{
			PaneID:     paneID,
			WindowName: windowName,
			LineNumber: i + 1,
			Text:       lines[i],
		}
		if context > 0 {
			lo := max(0, i-context)
			hi := min(len(lines), i+context+1)
			m.Context = strings.Join(lines[lo:hi], "\n")
		}
		out = append(out, m)
	}
	return out
}

const (
	defaultWaitTimeout = 30 * time.Second
	maxWaitTimeout     = 300 * time.Second
	waitPollInterval   = 400 * time.Millisecond
	waitCaptureHistory = 500 // lines captured each poll, to catch fast output
)

func toolWaitForPattern(args json.RawMessage) (interface{}, *rpcError) {
	var a struct {
		Pane           string `json:"pane"`
		Pattern        string `json:"pattern"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		Context        int    `json:"context"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(a.Pane) == "" || a.Pattern == "" {
		return errorResult("both 'pane' and 'pattern' arguments are required"), nil
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return errorResult("invalid regular expression: %v", err), nil
	}
	paneID, err := tmux.ResolvePaneID(a.Pane)
	if err != nil {
		return errorResult("%v", err), nil
	}

	timeout := defaultWaitTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
		if timeout > maxWaitTimeout {
			timeout = maxWaitTimeout
		}
	}

	deadline := time.Now().Add(timeout)
	for {
		content, capErr := tmux.CapturePane(paneID, waitCaptureHistory)
		if capErr != nil {
			return errorResult("%v", capErr), nil
		}
		lines := strings.Split(content, "\n")
		// Scan newest-first so we return the most recent matching line.
		for i := len(lines) - 1; i >= 0; i-- {
			if re.MatchString(lines[i]) {
				m := obj{"pane_id": paneID, "matched": true, "line": lines[i]}
				if a.Context > 0 {
					lo := max(0, i-a.Context)
					hi := min(len(lines), i+a.Context+1)
					m["context"] = strings.Join(lines[lo:hi], "\n")
				}
				return jsonResult(m)
			}
		}
		if time.Now().After(deadline) {
			return jsonResult(obj{
				"pane_id": paneID,
				"matched": false,
				"message": fmt.Sprintf("timed out after %s waiting for /%s/", timeout, a.Pattern),
			})
		}
		time.Sleep(waitPollInterval)
	}
}
