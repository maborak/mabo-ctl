// Package mcp is a minimal Model Context Protocol server over stdio: JSON-RPC
// 2.0 messages, one per line, on stdin and stdout. It implements exactly what a
// tool server needs — initialize, ping, tools/list and tools/call — with the
// standard library alone.
//
// It knows nothing about mabo-ctl. cmd/mabo-ctl builds the tools and hands
// them over; this package only speaks the protocol. Nothing here listens on a
// network: the client spawns the server and owns both pipes.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the MCP revision this server implements. A client asking
// for another revision gets this one back, which is how the protocol
// negotiates: the client then decides whether it can continue.
const ProtocolVersion = "2025-06-18"

// maxMessage bounds one inbound line. Tool arguments here are a few names and
// a number; a megabyte is generous, and a line past it ends the session rather
// than growing memory without limit.
const maxMessage = 1 << 20

// JSON-RPC error codes the server returns.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Tool is one callable tool.
type Tool struct {
	// Name is the tool's identifier, e.g. "status".
	Name string
	// Description tells the model what the tool does and when to use it.
	Description string
	// InputSchema is the JSON Schema of the arguments object.
	InputSchema map[string]any
	// Call runs the tool. A returned error is reported to the model as a tool
	// error (isError), not as a protocol error: the call was valid, the work
	// failed, and the model should see why.
	Call func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server serves tools to one client.
type Server struct {
	// Name and Version identify the server in the initialize reply.
	Name, Version string
	// Instructions, when set, is sent in the initialize reply as guidance for
	// the model.
	Instructions string
	// Tools are the tools offered, in the order tools/list reports them.
	Tools []Tool
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from r and writes responses to w until r ends or ctx
// is done. Requests run concurrently — a start that waits for readiness does
// not hold up a ping — and their responses are written whole, one per line.
// When the input ends — which is how an MCP client asks a stdio server to
// exit — calls already in flight finish and are answered; cancelling ctx (a
// signal) is what abandons them.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	send := func(resp response) {
		mu.Lock()
		defer mu.Unlock()
		resp.JSONRPC = "2.0"
		if resp.ID == nil {
			resp.ID = json.RawMessage("null")
		}
		_ = enc.Encode(resp) // a broken stdout ends the session at the next read
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessage)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			send(response{Error: &rpcError{Code: codeParse, Message: "parse error: " + err.Error()}})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			if req.ID != nil {
				send(response{ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "not a JSON-RPC 2.0 request"}})
			}
			continue
		}
		if req.ID == nil {
			// A notification: initialized, cancelled. Nothing to answer.
			continue
		}
		wg.Add(1)
		go func(req request) {
			defer wg.Done()
			result, rerr := s.handle(ctx, req)
			if rerr != nil {
				send(response{ID: req.ID, Error: rerr})
				return
			}
			send(response{ID: req.ID, Result: result})
		}(req)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcp: read: %w", err)
	}
	return nil
}

// handle answers one request.
func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		result := map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}
		if s.Instructions != "" {
			result["instructions"] = s.Instructions
		}
		return result, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		return map[string]any{"tools": tools}, nil

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs {\"name\": …, \"arguments\": {…}}"}
		}
		for _, t := range s.Tools {
			if t.Name != p.Name {
				continue
			}
			args := p.Arguments
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			text, err := t.Call(ctx, args)
			isError := err != nil
			if isError {
				var ue *UsageError
				if errors.As(err, &ue) || text == "" {
					text = err.Error()
				} else {
					text += "\n" + err.Error()
				}
			}
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isError,
			}, nil
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// UsageError is a tool error caused by the arguments, reported to the model
// alone rather than appended to partial output.
type UsageError struct{ Msg string }

// Error returns the message.
func (e *UsageError) Error() string { return e.Msg }

// Usagef returns a [UsageError].
func Usagef(format string, a ...any) error { return &UsageError{Msg: fmt.Sprintf(format, a...)} }
