package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// session runs lines through a server and returns the responses by id.
func session(t *testing.T, s *Server, lines ...string) map[string]map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("response %q is not JSON: %v", l, err)
		}
		if m["jsonrpc"] != "2.0" {
			t.Errorf("response %q lacks jsonrpc 2.0", l)
		}
		b, _ := json.Marshal(m["id"])
		got[string(b)] = m
	}
	return got
}

func testServer() *Server {
	return &Server{Name: "t", Version: "1", Tools: []Tool{
		{Name: "echo", Description: "d", InputSchema: map[string]any{"type": "object"},
			Call: func(_ context.Context, a json.RawMessage) (string, error) { return string(a), nil }},
		{Name: "fail", Description: "d", InputSchema: map[string]any{"type": "object"},
			Call: func(context.Context, json.RawMessage) (string, error) { return "partial", errors.New("boom") }},
	}}
}

func TestServerSpeaksTheProtocol(t *testing.T) {
	t.Parallel()
	got := session(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"missing"}}`,
		`not json`,
		`{"jsonrpc":"2.0","id":"p","method":"ping"}`,
	)
	if len(got) != 8 {
		t.Fatalf("got %d responses, want 8 (the notification gets none): %v", len(got), got)
	}
	init := got["1"]["result"].(map[string]any)
	if init["protocolVersion"] != ProtocolVersion {
		t.Errorf("initialize offered %v, want %s", init["protocolVersion"], ProtocolVersion)
	}
	if tools := got["2"]["result"].(map[string]any)["tools"].([]any); len(tools) != 2 {
		t.Errorf("tools/list = %v", tools)
	}
	text := func(id string) (string, bool) {
		r := got[id]["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
	}
	if s, isErr := text("3"); s != `{"x":1}` || isErr {
		t.Errorf("echo = %q, isError %v", s, isErr)
	}
	if s, isErr := text("4"); s != "partial\nboom" || !isErr {
		t.Errorf("fail = %q, isError %v; want the partial output and the error", s, isErr)
	}
	for id, code := range map[string]float64{"5": -32601, "6": -32602, "null": -32700} {
		if e, ok := got[id]["error"].(map[string]any); !ok || e["code"] != code {
			t.Errorf("id %s error = %v, want code %v", id, got[id]["error"], code)
		}
	}
	if _, ok := got[`"p"`]["result"]; !ok {
		t.Error("ping with a string id was not answered")
	}
}

func TestServeFinishesInFlightCallsWhenInputEnds(t *testing.T) {
	t.Parallel()
	s := &Server{Tools: []Tool{{Name: "slow", InputSchema: map[string]any{},
		Call: func(ctx context.Context, _ json.RawMessage) (string, error) { return "done", ctx.Err() }}}}
	got := session(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`)
	r := got["1"]["result"].(map[string]any)
	if r["isError"].(bool) {
		t.Errorf("an in-flight call was cancelled by end of input: %v", r)
	}
}

func TestServeRefusesAnOversizedMessage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := testServer().Serve(context.Background(), strings.NewReader(strings.Repeat("x", maxMessage+10)+"\n"), &out)
	if err == nil {
		t.Error("a message past the limit did not end the session")
	}
}
