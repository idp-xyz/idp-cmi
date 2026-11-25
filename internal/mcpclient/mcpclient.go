// Package mcpclient is the part of an MCP client that CMI needs to call the tools of the ingest sandbox through the platform's
// system gateway (ADR-0001 D62): protocol 2026-07-28, one POST per request with the headers MCP-Protocol-Version, Mcp-Method and
// Mcp-Name and the _meta envelope (protocol version, client info, capabilities); the answer is JSON or one server-sent
// message. No environment proxy and no redirects. It only calls tools; the catalog was pinned and reviewed when the gateway's
// release was made, so there is nothing to discover. Modelled on idp-host's client.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Protocol constants (MCP 2026-07-28).
const (
	ProtocolVersion        = "2026-07-28"
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	HeaderProtocolVersion  = "MCP-Protocol-Version"
	HeaderMethod           = "Mcp-Method"
	HeaderName             = "Mcp-Name"
)

// MaxResponse bounds a buffered answer.
const MaxResponse = 4 << 20

// Tokens is where the bearer credential comes from.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	Invalidate(bad string)
}

// ClientInfo is what CMI announces as clientInfo.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Client calls tools at one gateway endpoint.
type Client struct {
	Endpoint string
	HTTP     *http.Client
	Tokens   Tokens
	Info     ClientInfo
}

// NewHTTPClient returns the client for gateway calls: no environment proxy, no redirects. timeout bounds one call from the
// request to the end of the answer (the gateway ends a call after its own limit, 60 s by default).
func NewHTTPClient(timeout time.Duration, tr *http.Transport) *http.Client {
	if tr == nil {
		tr = &http.Transport{}
	}
	tr.Proxy = nil
	if tr.DialContext == nil {
		tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	tr.DisableCompression = true
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// CheckEndpoint validates a gateway MCP endpoint: an http(s) URL without credentials, query or fragment.
func CheckEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the gateway endpoint must be an http(s) URL without credentials, query or fragment")
	}
	return nil
}

// RPCError is a JSON-RPC error the server answered.
type RPCError struct {
	Code       int    `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"-"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

// HTTPError is a non-200 answer without a JSON-RPC body (401, 403, 429, 5xx).
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("gateway HTTP %d", e.Status) }

// NetworkError is a call that did not get an answer: the gateway was unreachable, or the call timed out.
type NetworkError struct {
	Err     error
	Timeout bool
}

func (e *NetworkError) Error() string { return "gateway: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

// Result is a tools/call result.
type Result struct {
	IsError    bool
	Text       string
	Structured json.RawMessage
}

// Decode reads the structured content into v.
func (r *Result) Decode(v any) error {
	if len(r.Structured) == 0 {
		return errors.New("the tool returned no structured content")
	}
	return json.Unmarshal(r.Structured, v)
}

func (c *Client) meta() map[string]any {
	return map[string]any{MetaProtocolVersion: ProtocolVersion, MetaClientInfo: c.Info, MetaClientCapabilities: map[string]any{}}
}

// CallTool calls a tool. A 401 renews the token once and repeats the call: the tools it is used for are safe to repeat in the
// way that matters (a create that was answered 401 was not run).
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*Result, error) {
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("no token for the gateway: %w", err)
	}
	res, err := c.call(ctx, token, name, args)
	if isUnauthorized(err) {
		c.Tokens.Invalidate(token)
		if token, err = c.Tokens.Token(ctx); err != nil {
			return nil, fmt.Errorf("no token for the gateway: %w", err)
		}
		res, err = c.call(ctx, token, name, args)
	}
	return res, err
}

func isUnauthorized(err error) bool {
	var he *HTTPError
	var re *RPCError
	return errors.As(err, &he) && he.Status == http.StatusUnauthorized || errors.As(err, &re) && re.HTTPStatus == http.StatusUnauthorized
}

// Unauthorized reports whether the gateway refused CMI's credential even after it was renewed.
func Unauthorized(err error) bool { return isUnauthorized(err) }

func (c *Client) call(ctx context.Context, bearer, name string, args map[string]any) (*Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args, "_meta": c.meta()}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(HeaderProtocolVersion, ProtocolVersion)
	req.Header.Set(HeaderMethod, "tools/call")
	req.Header.Set(HeaderName, name)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ne net.Error
		inner := errors.Unwrap(err)
		if inner == nil {
			inner = err
		}
		return nil, &NetworkError{Err: inner, Timeout: errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		var ne net.Error
		return nil, &NetworkError{Err: err, Timeout: errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()}
	}
	if len(raw) > MaxResponse {
		return nil, errors.New("the gateway's answer is too large")
	}
	msg, err := singleMessage(resp.Header.Get("Content-Type"), raw)
	if err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, &HTTPError{Status: resp.StatusCode}
		}
		return nil, fmt.Errorf("gateway answer: %w", err)
	}
	var out struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(msg, &out); err != nil || out.JSONRPC != "2.0" {
		if resp.StatusCode != http.StatusOK {
			return nil, &HTTPError{Status: resp.StatusCode}
		}
		return nil, errors.New("malformed JSON-RPC answer")
	}
	if out.Error != nil {
		out.Error.HTTPStatus = resp.StatusCode
		return nil, out.Error
	}
	if resp.StatusCode != http.StatusOK || len(out.Result) == 0 || out.Result[0] != '{' {
		return nil, &HTTPError{Status: resp.StatusCode}
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		return nil, errors.New("malformed tools/call result")
	}
	r := &Result{IsError: res.IsError, Structured: res.Structured}
	var text []string
	for _, b := range res.Content {
		if b.Type == "text" {
			text = append(text, b.Text)
		}
	}
	r.Text = strings.Join(text, "\n")
	return r, nil
}

// singleMessage returns the one JSON message of an answer that is application/json or an event stream with exactly one message
// event (other events are ignored).
func singleMessage(contentType string, body []byte) (json.RawMessage, error) {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch ct {
	case "application/json":
		if !json.Valid(body) {
			return nil, errors.New("invalid JSON body")
		}
		return json.RawMessage(bytes.TrimSpace(body)), nil
	case "text/event-stream":
		var msgs []json.RawMessage
		for _, block := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
			event, data := "", []string{}
			for _, line := range strings.Split(block, "\n") {
				switch {
				case strings.HasPrefix(line, "event:"):
					event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				case strings.HasPrefix(line, "data:"):
					data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				}
			}
			if len(data) == 0 || (event != "" && event != "message") {
				continue
			}
			joined := strings.Join(data, "\n")
			if !json.Valid([]byte(joined)) {
				return nil, errors.New("invalid JSON in the event stream")
			}
			msgs = append(msgs, json.RawMessage(joined))
		}
		if len(msgs) != 1 {
			return nil, errors.New("expected exactly one message in the event stream")
		}
		return msgs[0], nil
	}
	return nil, errors.New("unexpected content type " + contentType)
}
