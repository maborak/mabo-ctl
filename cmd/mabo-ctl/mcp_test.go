package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// mcpSession runs JSON-RPC lines through `mabo-ctl mcp` and returns the tool
// names it lists and the text of the call with id 2.
func mcpSession(t *testing.T, h *harness, call string) ([]string, string) {
	t.Helper()
	h.env.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" + call + "\n")
	if code := h.run(); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr)
	}
	var names []string
	text := ""
	for _, l := range strings.Split(strings.TrimSpace(h.stdout.String()), "\n") {
		var m struct {
			ID     int `json:"id"`
			Result struct {
				Tools   []struct{ Name string }
				Content []struct{ Text string }
			}
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout carried a non-protocol line %q: %v", l, err)
		}
		switch m.ID {
		case 1:
			for _, tool := range m.Result.Tools {
				names = append(names, tool.Name)
			}
		case 2:
			if len(m.Result.Content) > 0 {
				text = m.Result.Content[0].Text
			}
		}
	}
	return names, text
}

func TestMCPIsReadOnlyUnlessAllowedToMutate(t *testing.T) {
	names, _ := mcpSession(t, newHarness(t, "mcp"), "")
	if got := strings.Join(names, ","); got != "status,health,logs,config" {
		t.Errorf("read-only tools = %s", got)
	}
	names, _ = mcpSession(t, newHarness(t, "mcp", "--allow-mutate"), "")
	if got := strings.Join(names, ","); got != "status,health,logs,config,start,stop,restart" {
		t.Errorf("--allow-mutate tools = %s", got)
	}
}

// TestMCPStatusRedactsHealthCredentials: the server answers whatever process
// spawned it, so it withholds a health-URL credential as the web console does.
func TestMCPStatusRedactsHealthCredentials(t *testing.T) {
	h := newHarness(t, "mcp")
	h.sup.statuses[0].Health = "http://admin:hunter2@localhost:7100/health"
	h.sup.statuses[0].Detail = "HEAD http://admin:hunter2@localhost:7100/health: refused"
	_, text := mcpSession(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"status","arguments":{"services":["alpha"]}}}`)
	if !strings.Contains(text, `"service": "alpha"`) {
		t.Fatalf("status did not answer: %q", text)
	}
	if strings.Contains(text, "hunter2") {
		t.Errorf("MCP status leaked a credential:\n%s", text)
	}
}
