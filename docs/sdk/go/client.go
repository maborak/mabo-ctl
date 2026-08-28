// Package maboclient provides a Go client for the mabo-ctl web console HTTP API.
//
// The client communicates with a running `mabo-ctl serve` instance over its
// loopback HTTP interface. Every response is decoded into a typed struct;
// SSE streams are returned as channels.
//
// Usage:
//
//	c, err := maboclient.New("http://127.0.0.1:7999", "YOUR_TOKEN")
//	if err != nil { log.Fatal(err) }
//
//	status, err := c.Status(ctx)
//	if err != nil { log.Fatal(err) }
//	for _, s := range status {
//	    fmt.Printf("%s: %s\n", s.Service, s.Phase)
//	}
package maboclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is an HTTP client for the mabo-ctl web console.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// Option configures the client.
type Option func(*Client)

// WithHTTPClient sets a custom http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// New returns a client for the given console URL and token.
func New(baseURL, token string, opts ...Option) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("maboclient: baseURL is required")
	}
	if token == "" {
		return nil, fmt.Errorf("maboclient: token is required")
	}
	c := &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// do executes a request and decodes the JSON response into v.
func (c *Client) do(ctx context.Context, method, path string, body any, v any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("maboclient: marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("maboclient: create request: %w", err)
	}
	req.Header.Set("X-Mabo-Ctl-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("maboclient: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string   `json:"error"`
			Valid []string `json:"valid,omitempty"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			return fmt.Errorf("maboclient: HTTP %d", resp.StatusCode)
		}
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    errResp.Error,
			Valid:      errResp.Valid,
		}
	}

	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			return fmt.Errorf("maboclient: decode response: %w", err)
		}
	}
	return nil
}

// APIError is a non-2xx response from the console.
type APIError struct {
	StatusCode int
	Message    string
	Valid      []string // valid service names, when applicable
}

func (e *APIError) Error() string {
	if len(e.Valid) > 0 {
		return fmt.Sprintf("maboclient: HTTP %d: %s (valid: %v)", e.StatusCode, e.Message, e.Valid)
	}
	return fmt.Sprintf("maboclient: HTTP %d: %s", e.StatusCode, e.Message)
}

// --- Data types ---

// ServiceInfo is one declared service as resolved.
type ServiceInfo struct {
	Name      string   `json:"name"`
	Dir       string   `json:"dir"`
	Port      int      `json:"port"`
	Health    string   `json:"health"`
	Runtime   string   `json:"runtime"`
	Cmd       []string `json:"cmd"`
	CmdLine   string   `json:"cmd_line"`
	CmdError  string   `json:"cmd_error,omitempty"`
	Env       []EnvVar `json:"env"`
	DependsOn []string `json:"depends_on"`
	Color     string   `json:"color"`
}

// EnvVar is one key-value pair from the declared environment.
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// StatusRecord is one service's status — the stable machine contract.
type StatusRecord struct {
	Service    string `json:"service"`
	Phase      string `json:"phase"`
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
	Health     string `json:"health"`
	HTTP       int    `json:"http_status"`
	Detail     string `json:"detail"`
	LogPath    string `json:"log_path"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	StartedAt  string `json:"started_at"`
	UptimeMS   int64  `json:"uptime_ms"`
	ExitCode   int    `json:"exit_code"`
	ExitSignal string `json:"exit_signal"`
	ExitedAt   string `json:"exited_at"`
}

// LogsResponse is the body of GET /api/logs/{svc}.
type LogsResponse struct {
	Service string   `json:"service"`
	Lines   []string `json:"lines"`
	Count   int      `json:"count"`
}

// EventJSON is one lifecycle event on the wire.
type EventJSON struct {
	Service string `json:"service"`
	Phase   string `json:"phase"`
	Msg     string `json:"msg"`
	Error   string `json:"error,omitempty"`
}

// MutationResponse is the body of a start, stop or restart.
type MutationResponse struct {
	Operation string      `json:"operation"`
	Services  []string    `json:"services"`
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	Events    []EventJSON `json:"events"`
}

// OriginsResponse is the body of GET /api/origins.
type OriginsResponse struct {
	Trusted  []string `json:"trusted"`
	Implicit string   `json:"implicit"`
	Origin   string   `json:"origin,omitempty"`
	Max      int      `json:"max"`
}

// HistoryResponse is the body of GET /api/history.
type HistoryResponse struct {
	Events []EventJSON `json:"events"`
}

// --- API methods ---

// Services returns every declared service as resolved.
func (c *Client) Services(ctx context.Context) ([]ServiceInfo, error) {
	var out []ServiceInfo
	if err := c.do(ctx, http.MethodGet, "/api/services", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Status returns one status per service (stable machine contract).
func (c *Client) Status(ctx context.Context) ([]StatusRecord, error) {
	var out []StatusRecord
	if err := c.do(ctx, http.MethodGet, "/api/status", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Logs returns the last n lines of a service's log.
func (c *Client) Logs(ctx context.Context, svc string, n int) (*LogsResponse, error) {
	u := "/api/logs/" + url.PathEscape(svc)
	if n > 0 {
		u += fmt.Sprintf("?tail=%d", n)
	}
	var out LogsResponse
	if err := c.do(ctx, http.MethodGet, u, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// History returns the most recent lifecycle events (oldest first, max 50).
func (c *Client) History(ctx context.Context) (*HistoryResponse, error) {
	var out HistoryResponse
	if err := c.do(ctx, http.MethodGet, "/api/history", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Origins returns the trusted browser origins.
func (c *Client) Origins(ctx context.Context) (*OriginsResponse, error) {
	var out OriginsResponse
	if err := c.do(ctx, http.MethodGet, "/api/origins", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetOrigins replaces the trusted-origin list.
func (c *Client) SetOrigins(ctx context.Context, trusted []string) (*OriginsResponse, error) {
	body := struct {
		Trusted []string `json:"trusted"`
	}{Trusted: trusted}
	var out OriginsResponse
	if err := c.do(ctx, http.MethodPost, "/api/origins", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Start starts one or more named services.
func (c *Client) Start(ctx context.Context, services ...string) (*MutationResponse, error) {
	return c.mutate(ctx, "/api/start-all", "/api/%s/start", services)
}

// Stop stops one or more named services.
func (c *Client) Stop(ctx context.Context, services ...string) (*MutationResponse, error) {
	return c.mutate(ctx, "/api/stop-all", "/api/%s/stop", services)
}

// Restart restarts one or more named services.
func (c *Client) Restart(ctx context.Context, services ...string) (*MutationResponse, error) {
	return c.mutate(ctx, "", "/api/%s/restart", services)
}

// mutate is the shared implementation for start/stop/restart.
func (c *Client) mutate(ctx context.Context, allPath, perPath string, services []string) (*MutationResponse, error) {
	path := allPath
	if len(services) == 1 {
		path = fmt.Sprintf(perPath, url.PathEscape(services[0]))
	} else if len(services) == 0 && allPath == "" {
		// restart-all doesn't have a dedicated endpoint; restart each
		return nil, fmt.Errorf("maboclient: restart requires at least one service name")
	}

	var out MutationResponse
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamEvents returns a channel of lifecycle events from GET /api/events.
// The channel is closed when the context is cancelled or the server disconnects.
func (c *Client) StreamEvents(ctx context.Context) (<-chan EventJSON, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/events", nil)
	if err != nil {
		return nil, fmt.Errorf("maboclient: create request: %w", err)
	}
	req.Header.Set("X-Mabo-Ctl-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("maboclient: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("maboclient: HTTP %d", resp.StatusCode)
	}

	ch := make(chan EventJSON, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		decodeSSE(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// StreamLogs returns a channel of log lines from GET /api/stream/{svc}.
// LogLine is one line of a service's log output on the wire.
type LogLine struct {
	Service string `json:"service"`
	Line    string `json:"line"`
}

// StreamLogs returns a channel of log lines from GET /api/stream/{svc}.
func (c *Client) StreamLogs(ctx context.Context, svc string) (<-chan LogLine, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/stream/"+url.PathEscape(svc), nil)
	if err != nil {
		return nil, fmt.Errorf("maboclient: create request: %w", err)
	}
	req.Header.Set("X-Mabo-Ctl-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("maboclient: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("maboclient: HTTP %d", resp.StatusCode)
	}

	ch := make(chan LogLine, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		decodeSSE(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// decodeSSE reads an SSE stream and sends decoded JSON events into ch.
// It exits when the context is cancelled or the body is closed.
func decodeSSE[T any](ctx context.Context, body io.Reader, ch chan<- T) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	var leftover []byte

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf[:0], leftover...)
			buf = append(buf, tmp[:n]...)

			// Split on double newlines (SSE event boundary)
			for {
				idx := bytes.Index(buf, []byte("\n\n"))
				if idx < 0 {
					leftover = make([]byte, len(buf))
					copy(leftover, buf)
					break
				}

				event := buf[:idx]
				buf = buf[idx+2:]

				// Extract data lines
				for _, line := range bytes.Split(event, []byte("\n")) {
					if bytes.HasPrefix(line, []byte("data: ")) {
						var v T
						if err := json.Unmarshal(line[6:], &v); err == nil {
							select {
							case ch <- v:
							case <-ctx.Done():
								return
							}
						}
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}
