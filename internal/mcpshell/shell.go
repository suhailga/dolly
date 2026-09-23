// Package mcpshell provides an interactive, psql-style playground for an MCP
// stdio server. It launches the server as a subprocess, performs the MCP
// handshake, and offers a REPL where you can list tools, inspect their schemas,
// and call them — seeing the raw/pretty responses. By default it drives this
// dolly binary's own "mcp" server, but it can point at any stdio MCP server.
package mcpshell

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// ── JSON-RPC client over the server's stdio ─────────────────────────────────────

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type client struct {
	cmd *exec.Cmd
	w   io.WriteCloser
	r   *bufio.Reader
	id  int
}

func dial(args []string) (*client, error) {
	cmd := exec.Command(args[0], args[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("wiring stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("wiring stdout: %w", err)
	}
	cmd.Stderr = os.Stderr // surface server crashes/logs directly
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting server %q: %w", strings.Join(args, " "), err)
	}
	// 16 MiB read buffer for large pane captures.
	r := bufio.NewReaderSize(stdout, 64*1024)
	return &client{cmd: cmd, w: stdin, r: r}, nil
}

// request sends a JSON-RPC request and returns the response matching its id.
func (c *client) request(method string, params interface{}) (json.RawMessage, *rpcError, error) {
	c.id++
	id := c.id
	msg := map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.send(msg); err != nil {
		return nil, nil, err
	}
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			return nil, nil, fmt.Errorf("reading response: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if json.Unmarshal(line, &resp) != nil || len(resp.ID) == 0 {
			continue // skip notifications / non-JSON noise
		}
		var got int
		json.Unmarshal(resp.ID, &got)
		if got != id {
			continue
		}
		return resp.Result, resp.Error, nil
	}
}

func (c *client) notify(method string, params interface{}) error {
	msg := map[string]interface{}{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.send(msg)
}

func (c *client) send(msg interface{}) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("writing to server: %w", err)
	}
	return nil
}

func (c *client) close() {
	c.w.Close()
	c.cmd.Wait()
}

// ── tool metadata ───────────────────────────────────────────────────────────────

type toolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type schema struct {
	Required   []string `json:"required"`
	Properties map[string]struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"properties"`
}

// ── shell ───────────────────────────────────────────────────────────────────────

// Options configures a shell session.
type Options struct {
	In          io.Reader
	Out         io.Writer
	Color       bool // colorize output
	Interactive bool // print prompts/banner (false when scripting via a pipe)
}

type shell struct {
	c     *client
	out   *bufio.Writer
	opt   Options
	tools map[string]toolInfo
	names []string
	srv   struct{ name, version, protocol string }
}

// Run launches the MCP server given by serverArgs and starts the REPL.
// clientVersion is reported to the server during initialize.
func Run(serverArgs []string, clientVersion string, opt Options) error {
	c, err := dial(serverArgs)
	if err != nil {
		return err
	}
	s := &shell{c: c, out: bufio.NewWriter(opt.Out), opt: opt, tools: map[string]toolInfo{}}
	defer func() { s.out.Flush(); c.close() }()

	if err := s.handshake(clientVersion); err != nil {
		return err
	}
	if err := s.loadTools(); err != nil {
		s.printf(s.red("warning: could not list tools: %v")+"\n", err)
	}
	if opt.Interactive {
		s.banner(serverArgs)
	}
	s.out.Flush() // flush banner before the REPL may swap the output writer
	return s.repl()
}

func (s *shell) handshake(clientVersion string) error {
	res, rerr, err := s.c.request("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "dolly-mcp-shell", "version": clientVersion},
	})
	if err != nil {
		return fmt.Errorf("initialize failed: %w", err)
	}
	if rerr != nil {
		return fmt.Errorf("initialize error %d: %s", rerr.Code, rerr.Message)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	json.Unmarshal(res, &init)
	s.srv.name, s.srv.version, s.srv.protocol = init.ServerInfo.Name, init.ServerInfo.Version, init.ProtocolVersion
	return s.c.notify("notifications/initialized", nil)
}

func (s *shell) loadTools() error {
	res, rerr, err := s.c.request("tools/list", nil)
	if err != nil {
		return err
	}
	if rerr != nil {
		return fmt.Errorf("%d: %s", rerr.Code, rerr.Message)
	}
	var out struct {
		Tools []toolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return err
	}
	s.tools = map[string]toolInfo{}
	s.names = s.names[:0]
	for _, t := range out.Tools {
		s.tools[t.Name] = t
		s.names = append(s.names, t.Name)
	}
	sort.Strings(s.names)
	return nil
}

func (s *shell) banner(serverArgs []string) {
	s.printf("%s\n", s.bold("dolly MCP playground"))
	s.printf("  server   : %s\n", strings.Join(serverArgs, " "))
	s.printf("  connected: %s v%s (protocol %s)\n", s.srv.name, s.srv.version, s.srv.protocol)
	s.printf("  tools    : %s\n", strings.Join(s.names, ", "))
	s.printf("\nType %s for commands, or just %s. %s to quit.\n\n",
		s.bold("\\help"), s.bold("<tool> [args]"), s.bold("\\quit"))
}

func (s *shell) repl() error {
	if s.opt.Interactive {
		if handled, err := s.replInteractive(); handled {
			return err
		}
		// Terminal could not be set up (e.g. not a real tty) — fall back.
	}
	return s.replLines()
}

// replInteractive drives the REPL with golang.org/x/term, giving line editing,
// history (up/down arrows), and a managed prompt. handled is false if the
// terminal could not be initialised, so the caller can fall back to line mode.
func (s *shell) replInteractive() (handled bool, err error) {
	inFile, ok1 := s.opt.In.(*os.File)
	outFile, ok2 := s.opt.Out.(*os.File)
	if !ok1 || !ok2 {
		return false, nil
	}
	oldState, merr := term.MakeRaw(int(inFile.Fd()))
	if merr != nil {
		return false, nil
	}
	defer term.Restore(int(inFile.Fd()), oldState)

	rw := struct {
		io.Reader
		io.Writer
	}{inFile, outFile}
	t := term.NewTerminal(rw, "")

	prompt := "dolly-mcp> "
	if s.opt.Color {
		prompt = string(t.Escape.Cyan) + prompt + string(t.Escape.Reset)
	}
	t.SetPrompt(prompt)

	// Route all shell output through the terminal so the prompt is redrawn
	// correctly; raw mode requires \n to be written as \r\n.
	s.out = bufio.NewWriter(crlfWriter{t})

	for {
		line, rerr := t.ReadLine()
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			if quit := s.handle(line); quit {
				return true, nil
			}
		}
		s.out.Flush()
		if rerr != nil {
			return true, nil // io.EOF on Ctrl-D
		}
	}
}

// replLines is a minimal line-based REPL for non-interactive input (pipes,
// scripts), where terminal editing is neither available nor desired.
func (s *shell) replLines() error {
	in := bufio.NewReader(s.opt.In)
	for {
		line, err := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			if quit := s.handle(line); quit {
				return nil
			}
		}
		s.out.Flush()
		if err != nil {
			return nil // EOF
		}
	}
}

// crlfWriter translates \n to \r\n, which terminals in raw mode require.
type crlfWriter struct{ w io.Writer }

func (c crlfWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// handle processes one input line; returns true to quit.
func (s *shell) handle(line string) bool {
	if strings.HasPrefix(line, "\\") {
		return s.meta(line)
	}
	// Direct tool call: "<tool> [args]"
	name, args := cut(line)
	if _, ok := s.tools[name]; !ok {
		s.printf(s.red("unknown tool %q")+" — try %s to list tools\n", name, s.bold("\\tools"))
		return false
	}
	s.callTool(name, args)
	return false
}

func (s *shell) meta(line string) bool {
	cmd, rest := cut(line)
	switch cmd {
	case "\\quit", "\\q":
		return true
	case "\\help", "\\?":
		s.help()
	case "\\tools":
		s.listTools()
	case "\\describe":
		s.describe(strings.TrimSpace(rest))
	case "\\call":
		name, args := cut(rest)
		if name == "" {
			s.printf(s.red("usage: \\call <tool> [json-or-positional-args]") + "\n")
			break
		}
		s.callTool(name, args)
	case "\\raw":
		s.raw(rest)
	case "\\reload":
		if err := s.loadTools(); err != nil {
			s.printf(s.red("reload failed: %v")+"\n", err)
		} else {
			s.printf("reloaded %d tools\n", len(s.names))
		}
	default:
		s.printf(s.red("unknown command %q")+" — try %s\n", cmd, s.bold("\\help"))
	}
	return false
}

func (s *shell) help() {
	s.printf(`Commands:
  <tool> [args]            Call a tool (e.g. list_panes distill)
  \call <tool> [args]      Same, explicit form
  \tools                   List available tools
  \describe <tool>         Show a tool's full description and schema
  \raw <method> [json]     Send a raw JSON-RPC method (e.g. \raw tools/list)
  \reload                  Re-fetch the tool list from the server
  \help                    Show this help
  \quit                    Quit

Args may be JSON ({"session":"distill"}) or positional. Positional tokens fill
the tool's fields in order (required first, then optional); quote values that
contain spaces. Examples:
  list_panes distill
  read_pane %s 200
  search_panes distill "error|warn" 500
`, "%3")
}

func (s *shell) listTools() {
	if len(s.names) == 0 {
		s.printf("(no tools)\n")
		return
	}
	for _, n := range s.names {
		s.printf("  %s\t%s\n", s.bold(n), firstLine(s.tools[n].Description))
	}
}

func (s *shell) describe(name string) {
	t, ok := s.tools[name]
	if !ok {
		s.printf(s.red("unknown tool %q")+"\n", name)
		return
	}
	s.printf("%s\n%s\n\n%s\n%s\n", s.bold(t.Name), t.Description, s.bold("input schema:"), indentJSON(t.InputSchema))
}

func (s *shell) callTool(name, argStr string) {
	args, err := s.buildArgs(name, argStr)
	if err != nil {
		s.printf(s.red("bad args: %v")+"\n", err)
		return
	}
	start := time.Now()
	res, rerr, err := s.c.request("tools/call", map[string]interface{}{"name": name, "arguments": args})
	elapsed := time.Since(start)
	if err != nil {
		s.printf(s.red("transport error: %v")+"\n", err)
		return
	}
	if rerr != nil {
		s.printf(s.red("RPC error %d: %s")+"\n", rerr.Code, rerr.Message)
		return
	}
	s.printToolResult(res)
	if s.opt.Interactive {
		s.printf(s.dim("Time: %s")+"\n", elapsed.Round(time.Millisecond))
	}
}

func (s *shell) raw(rest string) {
	method, params := cut(rest)
	if method == "" {
		s.printf(s.red("usage: \\raw <method> [json-params]") + "\n")
		return
	}
	var p interface{}
	if strings.TrimSpace(params) != "" {
		if err := json.Unmarshal([]byte(params), &p); err != nil {
			s.printf(s.red("invalid JSON params: %v")+"\n", err)
			return
		}
	}
	res, rerr, err := s.c.request(method, p)
	if err != nil {
		s.printf(s.red("transport error: %v")+"\n", err)
		return
	}
	if rerr != nil {
		s.printf(s.red("RPC error %d: %s")+"\n", rerr.Code, rerr.Message)
		return
	}
	s.printf("%s\n", indentJSON(res))
}

// buildArgs turns an argument string into an arguments object. It accepts a JSON
// object, or positional tokens mapped one-to-one onto the tool's fields — required
// fields first (in order), then optional fields in declared order. Tokens are
// quote-aware, so values with spaces use quotes (e.g. "error|warn"). Empty input
// yields {}.
func (s *shell) buildArgs(name, argStr string) (interface{}, error) {
	argStr = strings.TrimSpace(argStr)
	if argStr == "" {
		return map[string]interface{}{}, nil
	}
	if strings.HasPrefix(argStr, "{") {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(argStr), &m); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return m, nil
	}

	var sc schema
	json.Unmarshal(s.tools[name].InputSchema, &sc)
	targets := positionalTargets(s.tools[name].InputSchema, sc.Required)
	if len(targets) == 0 {
		return nil, fmt.Errorf("tool %q takes no positional args; pass JSON", name)
	}
	toks := tokenize(argStr)
	if len(toks) > len(targets) {
		return nil, fmt.Errorf("too many args (%d) for %q; fields: %s — or pass JSON",
			len(toks), name, strings.Join(targets, ", "))
	}
	m := map[string]interface{}{}
	for i, tok := range toks {
		key := targets[i]
		switch sc.Properties[key].Type {
		case "integer", "number":
			n, err := strconv.Atoi(tok)
			if err != nil {
				return nil, fmt.Errorf("field %q expects a number, got %q", key, tok)
			}
			m[key] = n
		default:
			m[key] = tok
		}
	}
	return m, nil
}

// positionalTargets returns the order in which positional args fill fields:
// required fields first (in their listed order), then the remaining properties
// in their declared order in the schema.
func positionalTargets(rawSchema json.RawMessage, required []string) []string {
	inReq := make(map[string]bool, len(required))
	targets := make([]string, 0, len(required))
	for _, r := range required {
		targets = append(targets, r)
		inReq[r] = true
	}
	for _, p := range propertyOrder(rawSchema) {
		if !inReq[p] {
			targets = append(targets, p)
		}
	}
	return targets
}

// propertyOrder returns the property names of a JSON schema in declared order
// (a plain map would lose ordering, which positional args depend on).
func propertyOrder(rawSchema json.RawMessage) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(rawSchema, &top) != nil {
		return nil
	}
	props, ok := top["properties"]
	if !ok {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(props))
	if t, err := dec.Token(); err != nil {
		return nil
	} else if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			break
		}
		if key, ok := t.(string); ok {
			keys = append(keys, key)
		}
		var skip json.RawMessage
		dec.Decode(&skip) // consume the value
	}
	return keys
}

// tokenize splits on whitespace but keeps "double"- and 'single'-quoted spans
// together, so positional values may contain spaces.
func tokenize(s string) []string {
	var toks []string
	var cur strings.Builder
	var quote rune
	inTok := false
	flush := func() {
		if inTok {
			toks = append(toks, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inTok = true
		case r == '"' || r == '\'':
			quote = r
			inTok = true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	flush()
	return toks
}

func (s *shell) printToolResult(res json.RawMessage) {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.Content == nil {
		s.printf("%s\n", indentJSON(res)) // not the standard shape — show raw
		return
	}
	if r.IsError {
		s.printf("%s ", s.red("[isError]"))
	}
	for _, c := range r.Content {
		if c.Type == "text" {
			s.printf("%s\n", maybePretty(c.Text))
		} else {
			s.printf("(%s content)\n", c.Type)
		}
	}
}

// ── small helpers ───────────────────────────────────────────────────────────────

func (s *shell) printf(format string, a ...interface{}) { fmt.Fprintf(s.out, format, a...) }

// cut splits a line into its first whitespace-delimited token and the remainder.
func cut(line string) (head, rest string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 && i < 80 {
		return s[:i+1]
	}
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func indentJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return buf.String()
	}
	return string(raw)
}

// maybePretty re-indents text that is itself JSON; otherwise returns it as-is.
func maybePretty(text string) string {
	t := strings.TrimSpace(text)
	if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
		return indentJSON(json.RawMessage(t))
	}
	return text
}

// ANSI color helpers — no-ops when color is disabled.
func (s *shell) color(code, text string) string {
	if !s.opt.Color {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}
func (s *shell) bold(t string) string { return s.color("1", t) }
func (s *shell) dim(t string) string  { return s.color("2", t) }
func (s *shell) red(t string) string  { return s.color("31", t) }
func (s *shell) cyan(t string) string { return s.color("36", t) }
