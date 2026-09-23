package tmux

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"tmux-manager/config"
	"tmux-manager/shortcuts"
)

// IsSessionAlive reports whether a tmux session with the given name is running.
func IsSessionAlive(name string) bool {
	cmd := exec.Command("tmux", "has-session", "-t", name)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// ListSessions returns the names of all running tmux sessions.
// Returns nil, nil (not an error) when no tmux server is running.
func ListSessions() ([]string, error) {
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}")
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		// Non-zero exit typically means no server is running — treat as empty list
		return nil, nil
	}
	var names []string
	for _, line := range strings.Split(string(bytes.TrimSpace(out)), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// GetSessionDetails queries tmux for the window count and active pane's working
// directory for the named session. On failure it returns zero values and an
// error — callers should warn and continue rather than abort.
func GetSessionDetails(name string) (windows int, workingDir string, err error) {
	runQuery := func(format string) (string, error) {
		cmd := exec.Command("tmux", "display-message", "-t", name, "-p", format)
		cmd.Stderr = io.Discard
		out, e := cmd.Output()
		return strings.TrimSpace(string(out)), e
	}

	winStr, err := runQuery("#{session_windows}")
	if err != nil {
		return 0, "", fmt.Errorf("could not query session windows for %q: %w", name, err)
	}
	windows, err = strconv.Atoi(winStr)
	if err != nil {
		return 0, "", fmt.Errorf("unexpected window count %q for session %q", winStr, name)
	}

	workingDir, err = runQuery("#{pane_current_path}")
	if err != nil {
		return windows, "", fmt.Errorf("could not query working dir for %q: %w", name, err)
	}

	return windows, workingDir, nil
}

func shouldShowPaneLabelsGlobal(cfg *config.TmuxConfig) bool {
	if cfg.ShowPaneLabels == nil {
		return true // default is enabled
	}
	return *cfg.ShowPaneLabels
}

func getDefaultLabelColor(cfg *config.TmuxConfig) string {
	if cfg.DefaultLabelColor != "" {
		return cfg.DefaultLabelColor
	}
	return "blue"
}

func enablePaneBordersForWindow(sessionName, windowName string, cfg *config.TmuxConfig) error {
	windowTarget := fmt.Sprintf("%s:%s", sessionName, windowName)

	// Enable pane border status for this specific window
	cmd := exec.Command("tmux", "set-window-option", "-t", windowTarget, "pane-border-status", "top")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable pane border status for window %s: %w", windowName, err)
	}

	// Simple colored format - just background color and the pane title
	defaultColor := getDefaultLabelColor(cfg)
	simpleFormat := fmt.Sprintf("#[bg=%s,fg=white,bold] #{pane_title} #[default]", defaultColor)

	// Set pane border format for this specific window
	cmd = exec.Command("tmux", "set-window-option", "-t", windowTarget, "pane-border-format", simpleFormat)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to set pane border format for window %s: %w", windowName, err)
	}

	return nil
}

// Default color palette for auto-coloring windows using user-friendly names
var defaultColorPalette = [16]string{
	"#8A9A5B", // Sage Green
	"#1B3A5C", // Navy Blue
	"#800020", // Deep Burgundy
	"yellow",
	"#00FFFF", // Cyan
	"magenta",
	"white",
	"#B87333", // Copper
	"#C0C0C0", // Silver
	"#39FF14", // Neon Green
	"brightblue",
	"brightred",
	"brightyellow",
	"brightcyan",
	"brightmagenta",
	"brightwhite",
}

// getAutoColor returns a color from the default palette based on window index
func getAutoColor(windowIndex int) string {
	return defaultColorPalette[windowIndex%len(defaultColorPalette)]
}

// shouldUseAutoColor checks if auto-coloring is enabled (default: true)
func shouldUseAutoColor(cfg *config.TmuxConfig) bool {
	if cfg.AutoColor == nil {
		return true // default is enabled
	}
	return *cfg.AutoColor
}

func setWindowColor(sessionName, windowName, color string) error {
	if color == "" {
		return nil
	}

	// Set the window tab background color in the status bar
	// This colors the "1:development", "2:monitoring" etc tabs at the bottom
	windowTarget := fmt.Sprintf("%s:%s", sessionName, windowName)

	// Set window status format with colored background
	cmd := exec.Command("tmux", "set-window-option", "-t", windowTarget, "window-status-style", fmt.Sprintf("bg=%s,fg=black", color))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to set window tab color '%s' for window %s: %w", color, windowName, err)
	}

	// Also set the current window style to make it more visible when selected
	cmd = exec.Command("tmux", "set-window-option", "-t", windowTarget, "window-status-current-style", fmt.Sprintf("bg=bright%s,fg=black,bold", color))
	if err := cmd.Run(); err != nil {
		// If bright version fails, just use regular color
		exec.Command("tmux", "set-window-option", "-t", windowTarget, "window-status-current-style", fmt.Sprintf("bg=%s,fg=white,bold", color)).Run()
	}

	return nil
}

func CreateTmuxSession(cfg *config.TmuxConfig) error {
	// Merge shortcut layers: defaults <- global <- per-session
	globalSC, _ := shortcuts.LoadGlobal()
	var defaults map[string]string
	if cfg.DefaultShortcuts == nil || *cfg.DefaultShortcuts {
		defaults = shortcuts.DefaultShortcuts
	}
	merged := shortcuts.Merge(defaults, globalSC, cfg.Shortcuts)
	cfg.Shortcuts = merged

	if len(cfg.Shortcuts) > 0 {
		path, err := shortcuts.WriteShellFile(cfg.SessionName, cfg.Terminal, cfg.Shortcuts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not write shortcuts file: %v\n", err)
		} else {
			cfg.ShortcutsFilePath = path
		}
	}

	// Kill existing session if it exists
	exec.Command("tmux", "kill-session", "-t", cfg.SessionName).Run()

	// Ensure tmux server is running so options can be set globally
	exec.Command("tmux", "start-server").Run()

	// Set base-index to 1 so window numbering starts from 1
	exec.Command("tmux", "set-option", "-g", "base-index", "1").Run()

	// Create new session with first window
	if len(cfg.Windows) == 0 {
		return fmt.Errorf("no windows defined in config")
	}

	firstWindow := cfg.Windows[0]

	// Create session with working directory (use first pane's dir if specified, otherwise session's dir)
	var cmd *exec.Cmd
	firstPaneWorkingDir := cfg.WorkingDirectory
	if len(firstWindow.Panes) > 0 && firstWindow.Panes[0].WorkingDirectory != "" {
		firstPaneWorkingDir = firstWindow.Panes[0].WorkingDirectory
	}

	// Determine the shell command to use
	shellCmd := GetShellCommand(cfg.Terminal)

	if firstPaneWorkingDir != "" {
		cmd = exec.Command("tmux", "new-session", "-d", "-s", cfg.SessionName, "-n", firstWindow.Name, "-c", firstPaneWorkingDir, shellCmd)
	} else {
		cmd = exec.Command("tmux", "new-session", "-d", "-s", cfg.SessionName, "-n", firstWindow.Name, shellCmd)
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Setup panes for first window
	err := SetupWindowPanes(cfg.SessionName, firstWindow.Name, firstWindow.Panes, cfg.WorkingDirectory, cfg)
	if err != nil {
		return fmt.Errorf("failed to setup panes for first window: %w", err)
	}

	// Enable pane borders for first window if labels are enabled
	if shouldShowPaneLabelsGlobal(cfg) {
		err := enablePaneBordersForWindow(cfg.SessionName, firstWindow.Name, cfg)
		if err != nil {
			return fmt.Errorf("failed to enable pane borders for first window: %w", err)
		}
	}

	// Apply color to first window
	globalWindowIndex := 0
	color := firstWindow.Color
	if color == "" && shouldUseAutoColor(cfg) {
		color = getAutoColor(globalWindowIndex)
	}
	globalWindowIndex++
	if err := setWindowColor(cfg.SessionName, firstWindow.Name, color); err != nil {
		return fmt.Errorf("failed to set color for first window: %w", err)
	}

	// Create additional windows
	for i, window := range cfg.Windows[1:] {
		windowIndex := i + 1

		// Create new window with working directory (use first pane's dir if specified, otherwise session's dir)
		windowWorkingDir := cfg.WorkingDirectory
		if len(window.Panes) > 0 && window.Panes[0].WorkingDirectory != "" {
			windowWorkingDir = window.Panes[0].WorkingDirectory
		}

		// Use session: format to avoid ambiguity when session name matches a window name
		sessionTarget := cfg.SessionName + ":"
		if windowWorkingDir != "" {
			cmd = exec.Command("tmux", "new-window", "-t", sessionTarget, "-n", window.Name, "-c", windowWorkingDir, shellCmd)
		} else {
			cmd = exec.Command("tmux", "new-window", "-t", sessionTarget, "-n", window.Name, shellCmd)
		}

		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to create window '%s': %w (output: %s)", window.Name, err, string(output))
		}

		err = SetupWindowPanes(cfg.SessionName, window.Name, window.Panes, cfg.WorkingDirectory, cfg)
		if err != nil {
			return fmt.Errorf("failed to setup panes for window '%s': %w", window.Name, err)
		}

		// Enable pane borders for this window if labels are enabled
		if shouldShowPaneLabelsGlobal(cfg) {
			err := enablePaneBordersForWindow(cfg.SessionName, window.Name, cfg)
			if err != nil {
				return fmt.Errorf("failed to enable pane borders for window '%s': %w", window.Name, err)
			}
		}

		// Apply color to this window
		color := window.Color
		if color == "" && shouldUseAutoColor(cfg) {
			color = getAutoColor(globalWindowIndex)
		}
		globalWindowIndex++
		if err := setWindowColor(cfg.SessionName, window.Name, color); err != nil {
			return fmt.Errorf("failed to set color for window '%s': %w", window.Name, err)
		}

		// Select the first pane in the window
		cmd = exec.Command("tmux", "select-pane", "-t", fmt.Sprintf("%s:%d.0", cfg.SessionName, windowIndex+1))
		cmd.Run()
	}

	// Select first window
	cmd = exec.Command("tmux", "select-window", "-t", fmt.Sprintf("%s:%s", cfg.SessionName, firstWindow.Name))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to select first window '%s': %w", firstWindow.Name, err)
	}

	// Add shell alias if RC file is configured
	if cfg.RcFile != "" {
		aliasName, err := AddShellAlias(cfg.RcFile, cfg.SessionName)
		if err != nil {
			// Don't fail session creation, just warn
			fmt.Fprintf(os.Stderr, "Warning: Failed to add shell alias: %v\n", err)
		} else {
			if aliasName != cfg.SessionName {
				fmt.Printf("Note: Created alias '%s' (conflict with existing '%s')\n", aliasName, cfg.SessionName)
			}
			fmt.Printf("Shell alias '%s' added to %s\n", aliasName, cfg.RcFile)
			fmt.Printf("Run 'source %s' or restart your shell to use it\n", cfg.RcFile)
		}
	}

	return nil
}

func TerminateTmuxSession(sessionName string, rcFile string) error {
	// Remove shell alias if RC file is configured
	if rcFile != "" {
		if err := RemoveShellAlias(rcFile, sessionName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to remove shell alias: %v\n", err)
		} else {
			fmt.Printf("Shell alias removed from %s\n", rcFile)
			fmt.Printf("Run 'source %s' or restart your shell to refresh\n", rcFile)
		}
	}

	cmd := exec.Command("tmux", "kill-session", "-t", sessionName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to terminate tmux session '%s': %w", sessionName, err)
	}

	// Clean up session-scoped shortcuts file
	shortcuts.CleanupShellFile(sessionName)

	return nil
}
