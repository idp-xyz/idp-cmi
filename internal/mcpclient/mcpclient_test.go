package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type tokens struct {
	mu      sync.Mutex
	current string
	n       int
	dropped []string
}

func (t *tokens) Token(context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == "" {
		t.n++
		t.current = "token-" + string(rune('0'+t.n))
	}
	return t.current, nil
}

func (t *tokens) Invalidate(bad string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropped = append(t.dropped, bad)
	if t.current == bad {
		t.current = ""
	}
}

func rpc(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func client(srv *httptest.Server, tk Tokens) *Client {
	return &Client{Endpoint: srv.URL, HTTP: NewHTTPClient(5*time.Second, nil), Tokens: tk, Info: ClientInfo{Name: "idp-cmi", Version: "test"}}
}

func TestACallCarriesTheHeadersAndTheMetaEnvelope(t *testing.T) {
	var got struct {
		hdr    http.Header
		params map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params map[string]any
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got.hdr, got.params = r.Header, req.Params
		if req.Method != "tools/call" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, req.Method)
		}
		rpc(w, map[string]any{"content": []map[string]any{{"type": "text", "text": "created"}}, "structuredContent": map[string]any{"workspaceId": "ws-1"}})
	}))
	defer srv.Close()
	res, err := client(srv, &tokens{}).CallTool(context.Background(), "ingest__sandbox_create", nil)
	if err != nil || res.IsError || res.Text != "created" {
		t.Fatalf("%+v %v", res, err)
	}
	var out struct{ WorkspaceID string }
	if err := res.Decode(&out); err != nil || out.WorkspaceID != "ws-1" {
		t.Fatalf("%+v %v", out, err)
	}
	h := got.hdr
	if h.Get("Mcp-Protocol-Version") != "2026-07-28" || h.Get("Mcp-Method") != "tools/call" || h.Get("Mcp-Name") != "ingest__sandbox_create" || h.Get("Authorization") != "Bearer token-1" {
		t.Fatalf("headers %v", h)
	}
	meta, _ := got.params["_meta"].(map[string]any)
	info, _ := meta[MetaClientInfo].(map[string]any)
	if meta[MetaProtocolVersion] != "2026-07-28" || info["name"] != "idp-cmi" || meta[MetaClientCapabilities] == nil || got.params["name"] != "ingest__sandbox_create" {
		t.Fatalf("params %v", got.params)
	}
}

func TestASingleMessageEventStreamIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: ping\ndata: {}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"isError\":true,\"content\":[{\"type\":\"text\",\"text\":\"the sandbox is at capacity; try again later\"}]}}\n\n"))
	}))
	defer srv.Close()
	res, err := client(srv, &tokens{}).CallTool(context.Background(), "x", nil)
	if err != nil || !res.IsError || res.Text != "the sandbox is at capacity; try again later" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestTwoMessagesAreRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		m := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
		_, _ = w.Write([]byte(m + m))
	}))
	defer srv.Close()
	if _, err := client(srv, &tokens{}).CallTool(context.Background(), "x", nil); err == nil {
		t.Fatal("two messages")
	}
}

func TestA401RenewsTheTokenOnceAndRepeatsTheCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rpc(w, map[string]any{"content": []any{}})
	}))
	defer srv.Close()
	tk := &tokens{}
	if _, err := client(srv, tk).CallTool(context.Background(), "x", nil); err != nil || calls.Load() != 2 || len(tk.dropped) != 1 || tk.dropped[0] != "token-1" {
		t.Fatalf("%d calls, dropped %v, %v", calls.Load(), tk.dropped, err)
	}
	// a gateway that keeps refusing is not asked a third time
	calls.Store(0)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
	defer bad.Close()
	_, err := client(bad, &tokens{}).CallTool(context.Background(), "x", nil)
	if !Unauthorized(err) || calls.Load() != 2 {
		t.Fatalf("%d calls, %v", calls.Load(), err)
	}
}

func TestErrorsAreTypedForTheCallersClassification(t *testing.T) {
	status := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := status.Load(); s == 502 {
			w.WriteHeader(502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"forbidden"}}`))
	}))
	defer srv.Close()
	c := client(srv, &tokens{})
	var re *RPCError
	if _, err := c.CallTool(context.Background(), "x", nil); !errors.As(err, &re) || re.HTTPStatus != 403 || re.Code != -32001 {
		t.Fatalf("%v", err)
	}
	status.Store(502)
	var he *HTTPError
	if _, err := c.CallTool(context.Background(), "x", nil); !errors.As(err, &he) || he.Status != 502 {
		t.Fatalf("%v", err)
	}
	srv.Close()
	var ne *NetworkError
	if _, err := c.CallTool(context.Background(), "x", nil); !errors.As(err, &ne) {
		t.Fatalf("%v", err)
	}
}

func TestATimeoutIsANetworkErrorMarkedAsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer srv.Close()
	c := client(srv, &tokens{})
	c.HTTP = NewHTTPClient(50*time.Millisecond, nil)
	var ne *NetworkError
	if _, err := c.CallTool(context.Background(), "x", nil); !errors.As(err, &ne) || !ne.Timeout {
		t.Fatalf("%v", err)
	}
}

func TestNoProxyAndNoRedirects(t *testing.T) {
	c := NewHTTPClient(time.Second, nil)
	if c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the environment proxy is used")
	}
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); rpc(w, map[string]any{}) }))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()
	cl := &Client{Endpoint: redirect.URL, HTTP: c, Tokens: &tokens{}}
	if _, err := cl.CallTool(context.Background(), "x", nil); err == nil || hits.Load() != 0 {
		t.Fatalf("a redirect was followed: %v", err)
	}
}

func TestCheckEndpoint(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "http://u:p@x", "http://x?a=1", "http://x#f", "/path"} {
		if CheckEndpoint(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := CheckEndpoint("https://system-gateway:8443/mcp"); err != nil {
		t.Fatal(err)
	}
}
