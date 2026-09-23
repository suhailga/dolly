// Package mcpserver implements a minimal Model Context Protocol (MCP) server
// that exposes dolly's tmux sessions, windows, and panes to an LLM over stdio.
//
// The server speaks JSON-RPC 2.0 framed as newline-delimited JSON, which is the
// MCP stdio transport. It is intentionally read-only: an LLM can discover
// sessions, inspect what is running in each pane, read a pane's output/logs,
// and search across panes — but it cannot send keystrokes or mutate state.
package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// protocolVersion is the MCP revision we default to when a client does not
// request a specific one. When the client does, we echo theirs back.
const defaultProtocolVersion = "2024-11-05"

const serverName = "dolly"

// rpcRequest is an incoming JSON-RPC 2.0 message. A request with no ID is a
// notification and must not be answered.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server holds the wiring for one stdio session.
type Server struct {
	version string
	in      *bufio.Scanner
	out     *bufio.Writer
	tails   *tailManager
}

// New returns a server reading requests from r and writing responses to w.
// version is dolly's build version, surfaced in the initialize handshake.
func New(r io.Reader, w io.Writer, version string) *Server {
	sc := bufio.NewScanner(r)
	// Pane captures can be large; allow generous line sizes for JSON messages.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &Server{
		version: version,
		in:      sc,
		out:     bufio.NewWriter(w),
		tails:   newTailManager(),
	}
}

// Serve runs the read/dispatch/respond loop until stdin closes.
func (s *Server) Serve() error {
	defer s.tails.closeAll() // stop pipe-pane tails and remove temp buffers
	for s.in.Scan() {
		line := strings.TrimSpace(s.in.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			// -32700: parse error. No id is known, so send a null-id error.
			s.write(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}

		s.dispatch(req)
	}
	if err := s.in.Err(); err != nil {
		return fmt.Errorf("mcp: reading stdin: %w", err)
	}
	return nil
}

// dispatch routes a single request to its handler. Notifications (no ID) never
// produce a response.
func (s *Server) dispatch(req rpcRequest) {
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		s.reply(req, s.handleInitialize(req.Params))
	case "notifications/initialized", "initialized":
		// Acknowledgement notification — nothing to return.
	case "ping":
		s.reply(req, struct{}{})
	case "tools/list":
		s.reply(req, map[string]interface{}{"tools": toolDefinitions()})
	case "tools/call":
		result, rerr := s.handleToolCall(req.Params)
		if rerr != nil {
			s.replyError(req, rerr.Code, rerr.Message)
			return
		}
		s.reply(req, result)
	default:
		if isNotification {
			return // ignore unknown notifications
		}
		s.replyError(req, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(params json.RawMessage) interface{} {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)

	version := p.ProtocolVersion
	if version == "" {
		version = defaultProtocolVersion
	}

	return map[string]interface{}{
		"protocolVersion": version,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    serverName,
			"version": s.version,
		},
		"instructions": "Read-only access to dolly-managed tmux sessions. " +
			"Call list_sessions to find sessions, list_panes to see each window/pane " +
			"and what command is running, read_pane to read a pane's output or logs, " +
			"and search_panes to grep across a session's panes.",
	}
}

// reply writes a successful response, unless req is a notification.
func (s *Server) reply(req rpcRequest, result interface{}) {
	if len(req.ID) == 0 {
		return
	}
	s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

// replyError writes an error response, unless req is a notification.
func (s *Server) replyError(req rpcRequest, code int, message string) {
	if len(req.ID) == 0 {
		return
	}
	s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: message}})
}

func (s *Server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}
