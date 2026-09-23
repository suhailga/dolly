package tmux

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// PaneInfo describes a single tmux pane and what is currently running in it.
// All fields are read straight from a live tmux server, so they reflect the
// real state of the pane at query time.
type PaneInfo struct {
	SessionName    string `json:"session_name"`
	WindowIndex    int    `json:"window_index"`
	WindowName     string `json:"window_name"`
	WindowActive   bool   `json:"window_active"`
	PaneIndex      int    `json:"pane_index"`
	PaneID         string `json:"pane_id"`         // tmux global id, e.g. "%3"
	Title          string `json:"title"`           // pane title (dolly label when set)
	Active         bool   `json:"active"`          // is this the active pane in its window
	CurrentCommand string `json:"current_command"` // foreground command, e.g. "node", "zsh"
	CurrentPath    string `json:"current_path"`    // pane working directory
	StartCommand   string `json:"start_command"`   // command the pane was launched with; dolly panes use "<shell> -l", hand-split panes differ
	PID            int    `json:"pid"`             // shell pid for the pane
	Width          int    `json:"width"`
	Height         int    `json:"height"`
}

// WindowInfo groups the panes of one window in a session.
type WindowInfo struct {
	Index  int        `json:"index"`
	Name   string     `json:"name"`
	Active bool       `json:"active"`
	Panes  []PaneInfo `json:"panes"`
}

// unitSep separates fields in our tmux -F format string. It is the ASCII unit
// separator, which will never appear in a path, command, or title.
const unitSep = "\x1f"

var paneFormat = strings.Join([]string{
	"#{window_index}",
	"#{window_name}",
	"#{window_active}",
	"#{pane_index}",
	"#{pane_id}",
	"#{pane_title}",
	"#{pane_active}",
	"#{pane_current_command}",
	"#{pane_current_path}",
	"#{pane_start_command}",
	"#{pane_pid}",
	"#{pane_width}",
	"#{pane_height}",
}, unitSep)

// ListPanes returns every pane across every window in the named session,
// ordered by window then pane index.
func ListPanes(session string) ([]PaneInfo, error) {
	cmd := exec.Command("tmux", "list-panes", "-s", "-t", session, "-F", paneFormat)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		if !IsSessionAlive(session) {
			return nil, fmt.Errorf("no tmux session named %q is running", session)
		}
		return nil, fmt.Errorf("failed to list panes for session %q: %w", session, err)
	}

	var panes []PaneInfo
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, unitSep)
		if len(f) < 13 {
			continue
		}
		panes = append(panes, PaneInfo{
			SessionName:    session,
			WindowIndex:    atoi(f[0]),
			WindowName:     f[1],
			WindowActive:   f[2] == "1",
			PaneIndex:      atoi(f[3]),
			PaneID:         f[4],
			Title:          f[5],
			Active:         f[6] == "1",
			CurrentCommand: f[7],
			CurrentPath:    f[8],
			StartCommand:   f[9],
			PID:            atoi(f[10]),
			Width:          atoi(f[11]),
			Height:         atoi(f[12]),
		})
	}
	return panes, nil
}

// ListWindows returns the windows of a session with their panes nested,
// which mirrors how an LLM tends to reason about a session's layout.
func ListWindows(session string) ([]WindowInfo, error) {
	panes, err := ListPanes(session)
	if err != nil {
		return nil, err
	}

	var windows []WindowInfo
	idx := make(map[int]int) // window index -> position in windows slice
	for _, p := range panes {
		pos, ok := idx[p.WindowIndex]
		if !ok {
			windows = append(windows, WindowInfo{
				Index:  p.WindowIndex,
				Name:   p.WindowName,
				Active: p.WindowActive,
			})
			pos = len(windows) - 1
			idx[p.WindowIndex] = pos
		}
		windows[pos].Panes = append(windows[pos].Panes, p)
	}
	return windows, nil
}

// CapturePane returns the textual contents of a single pane.
//
// target is any tmux pane target: a global pane id such as "%3", or a
// "session:window.pane" reference. When historyLines > 0, that many lines of
// scrollback above the visible screen are included; otherwise only the visible
// screen is captured. Trailing blank lines are trimmed.
//
// Lines wrapped at the pane's width are rejoined (-J) so callers get full
// logical lines rather than width-clipped fragments.
func CapturePane(target string, historyLines int) (string, error) {
	args := []string{"capture-pane", "-p", "-J", "-t", target}
	if historyLines > 0 {
		// -S -N starts the capture N lines back in the scrollback buffer.
		args = append(args, "-S", "-"+strconv.Itoa(historyLines))
	}
	return runCapture(target, args)
}

// CapturePaneRange returns pane contents between two tmux line coordinates,
// with wrapped lines rejoined (-J). Coordinates follow tmux's capture-pane
// convention: line 0 is the top of the visible screen, positive numbers go down
// the screen, and negative numbers go up into the scrollback history (e.g.
// start=-200, end=-150 reads that historical block). end may be 0 to mean the
// bottom of the visible screen.
func CapturePaneRange(target string, start, end int) (string, error) {
	args := []string{
		"capture-pane", "-p", "-J", "-t", target,
		"-S", strconv.Itoa(start),
		"-E", strconv.Itoa(end),
	}
	return runCapture(target, args)
}

func runCapture(target string, args []string) (string, error) {
	cmd := exec.Command("tmux", args...)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to capture pane %q (is the target valid? try list_panes): %w", target, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// ResolvePaneID resolves any pane target (a pane id, or "session:window.pane")
// to its stable global pane id (e.g. "%3"). This gives a consistent key for
// tracking a pane across calls even if window/pane indices shift.
func ResolvePaneID(target string) (string, error) {
	cmd := exec.Command("tmux", "display-message", "-p", "-t", target, "#{pane_id}")
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("unknown pane %q (try list_panes)", target)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("unknown pane %q (try list_panes)", target)
	}
	return id, nil
}

// StartPipePane tees a pane's output to the given file via tmux pipe-pane. New
// output is appended as it is produced, so reading the file incrementally yields
// a lossless tail. tmux allows only one pipe per pane; calling this replaces any
// existing pipe on that pane.
func StartPipePane(target, file string) error {
	// tmux runs the shell-command argument via /bin/sh, so redirection works.
	shellCmd := "cat >> " + shellQuote(file)
	cmd := exec.Command("tmux", "pipe-pane", "-t", target, shellCmd)
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start pipe-pane on %q: %w", target, err)
	}
	return nil
}

// StopPipePane turns off output piping for a pane (tmux pipe-pane with no command).
func StopPipePane(target string) error {
	cmd := exec.Command("tmux", "pipe-pane", "-t", target)
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// shellQuote single-quotes a string for safe use in an sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
