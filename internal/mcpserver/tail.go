package mcpserver

import (
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"tmux-manager/tmux"
)

const defaultTailSeedLines = 40

// tailManager tracks active pane "tails". Each tail tees a pane's output to a
// temp file via tmux pipe-pane; reading the file forward from a stored offset
// yields only the output produced since the previous call (a lossless tail).
//
// State lives for the lifetime of the server process, so successive tool calls
// from the same client see incremental output. closeAll tears everything down
// when the client disconnects.
type tailManager struct {
	mu    sync.Mutex
	tails map[string]*tailEntry // keyed by stable pane id (e.g. "%3")
}

type tailEntry struct {
	paneID string
	path   string
	offset int64
}

func newTailManager() *tailManager {
	return &tailManager{tails: map[string]*tailEntry{}}
}

// tail handles the tail_pane tool. First call for a pane starts the pipe and
// returns the current screen as context; subsequent calls return only new output.
// Passing {"stop": true} stops tailing and cleans up.
func (m *tailManager) tail(args json.RawMessage) (interface{}, *rpcError) {
	var a struct {
		Pane  string `json:"pane"`
		Lines *int   `json:"lines"`
		Stop  bool   `json:"stop"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(a.Pane) == "" {
		return errorResult("the 'pane' argument is required"), nil
	}

	paneID, err := tmux.ResolvePaneID(a.Pane)
	if err != nil {
		return errorResult("%v", err), nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if a.Stop {
		if e, ok := m.tails[paneID]; ok {
			m.teardown(e)
			delete(m.tails, paneID)
			return textResult("stopped tailing " + paneID), nil
		}
		return textResult(paneID + " was not being tailed"), nil
	}

	entry, ok := m.tails[paneID]
	if !ok {
		return m.start(paneID, a.Lines)
	}
	return m.readNew(entry)
}

// start begins tailing a pane and returns its current screen as initial context.
func (m *tailManager) start(paneID string, lines *int) (interface{}, *rpcError) {
	f, err := os.CreateTemp("", "dolly-tail-*.log")
	if err != nil {
		return errorResult("could not create tail buffer: %v", err), nil
	}
	path := f.Name()
	f.Close()

	if err := tmux.StartPipePane(paneID, path); err != nil {
		os.Remove(path)
		return errorResult("%v", err), nil
	}
	m.tails[paneID] = &tailEntry{paneID: paneID, path: path, offset: 0}

	seedLines := defaultTailSeedLines
	if lines != nil && *lines >= 0 {
		seedLines = *lines
	}
	seed, _ := tmux.CapturePane(paneID, seedLines)

	var b strings.Builder
	b.WriteString("Started tailing " + paneID + ". Current screen:\n\n")
	if seed != "" {
		b.WriteString(seed)
		b.WriteString("\n")
	}
	b.WriteString("\n(call tail_pane again to stream new output; pass {\"stop\":true} when done)")
	return textResult(b.String()), nil
}

// readNew returns output produced since the last read, advancing the offset.
func (m *tailManager) readNew(e *tailEntry) (interface{}, *rpcError) {
	f, err := os.Open(e.path)
	if err != nil {
		// Pipe file vanished — treat the tail as broken and reset it.
		m.teardown(e)
		delete(m.tails, e.paneID)
		return errorResult("tail buffer for %s was lost; call tail_pane again to restart", e.paneID), nil
	}
	defer f.Close()

	if _, err := f.Seek(e.offset, io.SeekStart); err != nil {
		return errorResult("could not read tail buffer: %v", err), nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return errorResult("could not read tail buffer: %v", err), nil
	}
	e.offset += int64(len(data))

	text := stripANSI(string(data))
	text = strings.Trim(text, "\n")
	if text == "" {
		return textResult("(no new output since last call)"), nil
	}
	return textResult(text), nil
}

func (m *tailManager) teardown(e *tailEntry) {
	tmux.StopPipePane(e.paneID)
	os.Remove(e.path)
}

// closeAll stops every active tail and removes its buffer. Called when the
// server shuts down so we never leave pipe-pane state or temp files behind.
func (m *tailManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.tails {
		m.teardown(e)
		delete(m.tails, id)
	}
}

// ansiRE matches ANSI CSI/OSC escape sequences and bare carriage returns, which
// pipe-pane captures verbatim from the raw terminal stream.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]|\r`)

func stripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}
