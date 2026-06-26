# Dolly — Tmux Session Manager

[![Release](https://github.com/suhailgupta03/dolly/workflows/Release/badge.svg)](https://github.com/suhailgupta03/dolly/actions)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

YAML-based tmux session manager. Define your dev environment once, spin it up anywhere.

## Install

**Pre-built binary** (recommended): [Releases](https://github.com/suhailgupta03/dolly/releases)

**From source:**
```bash
git clone https://github.com/suhailgupta03/dolly.git
cd dolly && make install
```

---

## Usage

### YAML mode — repeatable environments

```bash
dolly my-project.yml       # create session
dolly -t my-project.yml    # terminate session
```

Minimal config:
```yaml
session_name: "my-session"
terminal: "zsh"
working_directory: "/path/to/project"
windows:
  - name: "dev"
    panes:
      - command: "npm run dev"
        split: "none"
      - command: "npm test"
        split: "vertical"
```

### Throwaway mode — disposable sessions

No config needed. Opens empty shells in the current directory.

```bash
dolly throwaway                          # 2 windows, 2 panes each, auto-named
dolly throwaway -windows 3 -panes 2     # custom layout
dolly throwaway -name my-debug          # named session
dolly throwaway -dir /path/to/project   # custom directory
```

Output:
```
Throwaway session 'tw-0401-143022' created (2 windows, 2 panes each)
Attach:  tmux attach -t tw-0401-143022
Kill:    dolly throwaway -kill tw-0401-143022
```

Manage throwaway sessions:
```bash
dolly throwaway -list                    # list sessions with status
dolly throwaway -kill SESSION            # kill + remove from registry
dolly throwaway -cleanup                 # remove dead sessions (default: 7 days)
dolly throwaway -cleanup -days 14        # custom threshold
```

### Exec mode — quick one-off sessions

```bash
dolly -e "npm run dev, npm test" -n my-session
```

Runs each comma-separated command in its own pane. Prompts to save as YAML.

### Attach mode — adopt existing tmux sessions

Already have tmux sessions running? Bring them under dolly management:

```bash
dolly attach SESSION_NAME                # adopt one session
dolly attach -all                        # adopt all unmanaged sessions
dolly attach -list                       # discover unmanaged sessions
```

Attaching is idempotent — running it again on the same session warns and updates the entry without creating a duplicate.

```
$ dolly attach -list
Unmanaged tmux sessions (not in dolly registry):
NAME      WINDOWS  DIR
work      3        /Users/x/work
personal  1        /Users/x

Run "dolly attach -all" to attach all, or "dolly attach NAME" for one.
```

### Pane shortcuts

Every dolly pane gets built-in shortcuts organised by root command (grep, find, tmux). See **[docs/shortcuts.md](docs/shortcuts.md)** for the full reference with descriptions and examples.

Add your own shortcuts:

```bash
dolly shortcuts                              # list all (with GROUP column)
dolly shortcuts add deploy "./deploy.sh"     # add global shortcut
dolly shortcuts remove deploy                # remove it
dolly shortcuts sync                         # rewrite shortcuts file for all live sessions
```

Per-session shortcuts can be defined in YAML via the `shortcuts:` key. Run `echo $DOLLY_SHORTCUTS_FILE` in any pane to see what's active.

**Adding a shortcut mid-flight:** `dolly shortcuts add` writes to `~/.dolly/shortcuts.yml` only. Running sessions won't see it until they re-source the file:

```bash
dolly shortcuts sync          # rewrites the .sh file for every live session
source $DOLLY_SHORTCUTS_FILE  # run this inside each pane to apply
```

### Terminate without a YAML file

Any session — throwaway, attached, exec — can be terminated by name:

```bash
dolly -t my-project.yml    # terminate via YAML (reads session name from file)
dolly -t SESSION_NAME      # terminate by session name directly
```

### Session registry

All dolly sessions are tracked in `~/.dolly/registry.json`. View them:

```bash
dolly sessions                           # all sessions (yaml, exec, throwaway, attached)
dolly sessions -type yaml                # filter by type
dolly sessions -type attached            # show adopted sessions
dolly sessions -format json              # output as JSON
```

Registry is updated on every create, terminate, attach, and cleanup. Sessions show as `alive` or `dead` based on live tmux status. Shortcuts files are cleaned up automatically when sessions are terminated.

### Crash reporting

Dolly logs internal errors and panics to `~/.dolly/logs/crashes.jsonl` automatically. User input errors (nonexistent files, invalid flags) are not logged.

```bash
dolly report                       # show recent crashes (last 5)
dolly report -last 10              # show last 10 entries (-last 0 for all)
dolly report preview               # review issue body before submitting
dolly report submit                # open pre-filled GitHub issue; confirm filing before marking
dolly report url                   # print issue URL only (useful on headless/SSH)
dolly report mark-submitted        # mark unsubmitted entries as reported after filing manually
dolly report -format json          # JSON output with submitted status
dolly report clear                 # clear crash log (prompts for confirmation)
```

Logs are stored with user-only permissions (`0600`). A `submitted.json` sidecar prevents duplicate issue reports.

### MCP server — let an LLM read your panes

`dolly mcp` starts a [Model Context Protocol](https://modelcontextprotocol.io) server on stdio giving an LLM **read-only** access to your tmux sessions — no keystrokes, no mutations.

| Tool | What it does |
|------|--------------|
| `list_sessions` | All tmux sessions — dolly-managed **and** hand-created (`managed: false`), with live window counts and alive status |
| `list_panes` | Every window/pane in a session: pane id, name (title), cwd, running command, `start_command`. `compact:true` trims to id/title/command/cwd; `filter:"web/ai"` returns only matching panes |
| `read_pane` | Read a pane's output/logs (full, untruncated lines) by pane id (`%3`) or `session:window.pane`; `history_lines` pulls scrollback, or `start`/`end` read a specific scrollback range |
| `tail_pane` | Live-tail a pane: first call returns the current screen, each later call returns only new output (lossless, via `tmux pipe-pane`). Poll in a loop; `{"stop":true}` ends it |
| `search_panes` | Regex-grep every pane; returns **full** matching lines with pane id/window/line. `context:N` adds surrounding lines (grep -C), `tail:N` scopes to recent output, `max_matches` caps results |
| `wait_for_pattern` | Block until a pane's output matches a regex (or `timeout_seconds`), then return the matching line + context. Replaces sleep/poll loops when waiting for a signal like `status=ok` |

Everything is **queried live from tmux on every call** — there is no config/snapshot cache — so panes, windows, and whole sessions added by hand (including in throwaway sessions) are always reflected. To spot a manually-added pane: pane ids are creation-ordered (higher `%N` = newer), and dolly config panes carry a meaningful title with `start_command` like `zsh -l`, while hand-split panes show the hostname as title and a `start_command` like `exec /bin/zsh`.

Point your MCP client at `dolly mcp` as a stdio server:

```json
{ "mcpServers": { "dolly": { "command": "dolly", "args": ["mcp"] } } }
```

Typical flow: `list_sessions` → `list_panes` → `read_pane` (read logs) or `search_panes` (hunt errors across the session).

**Live tailing.** MCP is request/response (no server push), so "tailing" means the LLM *polls* `tail_pane` in a loop. Unlike repeated `read_pane` calls, `tail_pane` returns only the new output since the previous call — it's backed by `tmux pipe-pane`, so nothing is missed between polls. Pipes and their buffers are cleaned up automatically when the client disconnects, or when you pass `{"stop":true}`.

#### Playground (`dolly mcp-shell`)

A psql-style REPL for poking at the server live — it launches `dolly mcp` as a subprocess and talks MCP to it, so you see real responses. Line editing and history come from `golang.org/x/term`.

```bash
dolly mcp-shell
```

```
dolly-mcp> \tools                       # list tools
dolly-mcp> \describe read_pane          # show a tool's full schema
dolly-mcp> list_sessions                # call a tool
dolly-mcp> list_panes distill           # positional args
dolly-mcp> read_pane %58 200            # pane + history_lines
dolly-mcp> tail_pane %58                 # start tailing; call again for new output
dolly-mcp> search_panes distill "error|warn" 500
dolly-mcp> wait_for_pattern %58 "status=ok"   # block until it appears
dolly-mcp> read_pane {"pane":"%58","history_lines":50}   # JSON args also work
dolly-mcp> \raw tools/list              # raw JSON-RPC
dolly-mcp> \quit
```

Args are JSON (`{...}`) or positional, mapped to the tool's fields in order (quote values with spaces). It also scripts: `echo 'list_sessions' | dolly mcp-shell`. Use `-cmd "<server>"` to drive any other stdio MCP server.

---

## YAML Configuration Reference

```yaml
session_name: "my-session"           # required
working_directory: "/path/to/project" # default for all panes
terminal: "zsh"                      # bash | zsh | fish (default: bash)
auto_color: true                     # auto-assign window tab colors
show_pane_labels: true               # show pane ID as label in border
default_label_color: "blue"          # label background color

windows:
  - name: "frontend"
    color: "green"                   # window tab color (overrides auto)
    panes:
      - id: "dev-server"             # becomes the pane label
        command: "npm run dev"
        split: "none"                # first pane is always none
        working_directory: "./web"   # overrides session default
        label_color: "brightblue"    # overrides default_label_color
        pre_hooks:
          - "nvm use 18"
      - id: "tests"
        command: "npm test"
        split: "vertical"            # side-by-side
        split_from: "dev-server"     # split from specific pane (optional)
```

**Split types:** `none` (first pane), `vertical` (side by side), `horizontal` (stacked)

**Colors:** `red`, `green`, `blue`, `yellow`, `cyan`, `magenta`, `white`, `black` — prefix with `bright` for bright variants

**Advanced layouts** use `split_from` to split from any named pane, not just the previous one:
```yaml
panes:
  - id: "main"   split: "none"
  - id: "right"  split: "vertical"    split_from: "main"
  - id: "bl"     split: "horizontal"  split_from: "main"
  - id: "br"     split: "horizontal"  split_from: "right"
```
Result: `main | right` on top, `bl | br` on bottom.

---

## Tmux config

Copy the included `.tmux.conf` for mouse support, 1-based window numbering, and sensible keybindings:
```bash
cp .tmux.conf ~/.tmux.conf
```

---

## Build

```bash
make build     # build binary
make install   # install to /usr/local/bin
make test      # run tests
```

## License

MIT — see [LICENSE](LICENSE)
