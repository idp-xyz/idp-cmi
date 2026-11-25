// Package convtest is a stand-in for the system gateway and the ingest sandbox behind it, for the tests of the conversion
// worker. It speaks MCP 2026-07-28 (one POST per call, JSON answers) and behaves like idp-sandbox where CMI depends on it:
//
//   - a ticket is read, not verified: its tenant_id, sub and kind must be the caller's and the tool's (files.For), then the whole
//     ticket is presented to the file endpoint as a Bearer token (files.Client.Get and Put);
//   - the 9th concurrent run, and a workspace over the caller's limit, are refused outright with the sandbox's words;
//   - a workspace holds files; sandbox_run runs the converter named in the script on in/original.* and writes out/extract.md.
package convtest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Output is what a converter does with a file.
type Output struct {
	Markdown string
	Exit     int
	Stderr   string
	TimedOut bool
	OOM      bool
	Delay    time.Duration
}

// Sandbox is the fake.
type Sandbox struct {
	// Token is the bearer credential the gateway accepts; Tenant and Subject are the caller it asserts to the sandbox.
	Token, Tenant, Subject string
	FilesURL               string // CMI's internal file endpoint (what IDP_SANDBOX_FILES_URL would be)
	MaxRuns                int    // concurrent runs (8 as the real instance's default; 0 = 8)
	MaxWorkspaces          int    // per caller (8; 0 = 8)
	// Convert is the converter: it is given the command line's name (pdftext or ooxml2md), the file's name and bytes.
	Convert func(cmd, name string, data []byte) Output
	// Before is called at the start of every tools/call, with the tool's name; a non-nil answer replaces the call's result
	// (status > 0: an HTTP status with no tool result).
	Before func(tool string, args map[string]any) *Reply

	Srv *httptest.Server

	mu         sync.Mutex
	workspaces map[string]map[string][]byte
	runs       int
	maxRuns    int
	calls      []string
	scripts    []string
	imported   []int
	tickets    []string
}

// Reply replaces a call's answer.
type Reply struct {
	Status  int    // an HTTP status without a JSON-RPC result (502, 429, ...)
	IsError bool   // a tool error with Text
	Text    string //
	Hang    time.Duration
}

// New starts the fake; Close stops it. The caller sets FilesURL once the file endpoint is up.
func New(token, tenant, subject string) *Sandbox {
	s := &Sandbox{Token: token, Tenant: tenant, Subject: subject, workspaces: map[string]map[string][]byte{}}
	s.Srv = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Close stops the server.
func (s *Sandbox) Close() { s.Srv.Close() }

// URL is the gateway endpoint.
func (s *Sandbox) URL() string { return s.Srv.URL }

// Calls are the names of the tools called, in order.
func (s *Sandbox) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// Scripts are the programs sandbox_run was given.
func (s *Sandbox) Scripts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.scripts...)
}

// Tickets are the tickets the tools were given, in order.
func (s *Sandbox) Tickets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tickets...)
}

// Workspaces is how many workspaces exist now.
func (s *Sandbox) Workspaces() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.workspaces) }

// MaxConcurrentRuns is the most runs that were in progress at once.
func (s *Sandbox) MaxConcurrentRuns() int { s.mu.Lock(); defer s.mu.Unlock(); return s.maxRuns }

// Count is how many times the tool was called.
func (s *Sandbox) Count(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == tool {
			n++
		}
	}
	return n
}

func rpc(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func toolError(w http.ResponseWriter, text string) {
	rpc(w, map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": text}}})
}

func toolOK(w http.ResponseWriter, text string, structured any) {
	rpc(w, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "structuredContent": structured})
}

func (s *Sandbox) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Mcp-Protocol-Version") != "2026-07-28" || r.Header.Get("Mcp-Method") != "tools/call" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Method != "tools/call" || r.Header.Get("Mcp-Name") != req.Params.Name || req.Params.Meta == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	name := strings.TrimPrefix(req.Params.Name, "ingest__")
	if name == req.Params.Name {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool"}}`))
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, name)
	s.mu.Unlock()
	args := req.Params.Arguments
	if s.Before != nil {
		if rep := s.Before(name, args); rep != nil {
			if rep.Hang > 0 {
				time.Sleep(rep.Hang)
			}
			switch {
			case rep.Status > 0:
				w.WriteHeader(rep.Status)
			case rep.IsError:
				toolError(w, rep.Text)
			}
			if rep.Status > 0 || rep.IsError {
				return
			}
		}
	}
	str := func(k string) string { v, _ := args[k].(string); return v }
	switch name {
	case "sandbox_create":
		s.mu.Lock()
		limit := s.MaxWorkspaces
		if limit == 0 {
			limit = 8
		}
		if len(s.workspaces) >= limit {
			s.mu.Unlock()
			toolError(w, "you already hold the maximum number of workspaces; delete one first")
			return
		}
		id := fmt.Sprintf("ws-%d", len(s.calls)*1000+len(s.workspaces))
		s.workspaces[id] = map[string][]byte{}
		s.mu.Unlock()
		toolOK(w, "workspace "+id+" created", map[string]any{"workspaceId": id})
	case "sandbox_delete":
		s.mu.Lock()
		delete(s.workspaces, str("workspaceId"))
		s.mu.Unlock()
		toolOK(w, "deleted", nil)
	case "sandbox_import_file":
		s.importFile(w, str("workspaceId"), str("source"), str("path"))
	case "sandbox_run":
		s.run(w, str("workspaceId"), str("language"), str("code"))
	case "sandbox_export_file":
		s.exportFile(w, str("workspaceId"), str("path"), str("target"), str("name"))
	default:
		toolError(w, "unknown tool")
	}
}

// peek is files.Peek: three parts, the middle one JSON with tenant_id, sub and kind; the signature is not looked at.
func peek(tok string) (tenant, sub, kind string, ok bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || len(tok) > 8192 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var c struct {
		Tenant string `json:"tenant_id"`
		Sub    string `json:"sub"`
		Kind   string `json:"kind"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Tenant == "" || c.Sub == "" || (c.Kind != "read" && c.Kind != "write") {
		return
	}
	return c.Tenant, c.Sub, c.Kind, true
}

func (s *Sandbox) forTicket(tok, kind string) string {
	t, sub, k, ok := peek(tok)
	switch {
	case !ok || k != kind:
		return "the file reference is not valid for this run (use the attachment or output reference you were given)"
	case t+"/"+sub != s.Tenant+"/"+s.Subject:
		return "the file reference belongs to someone else"
	}
	return ""
}

func (s *Sandbox) importFile(w http.ResponseWriter, ws, source, path string) {
	s.mu.Lock()
	s.tickets = append(s.tickets, source)
	s.mu.Unlock()
	if msg := s.forTicket(source, "read"); msg != "" {
		toolError(w, msg)
		return
	}
	req, _ := http.NewRequest(http.MethodGet, s.FilesURL+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+source)
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		toolError(w, "the file could not be moved: the Workspace did not answer: "+err.Error())
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		toolError(w, "the file reference is not valid for this run (use the attachment or output reference you were given)")
		return
	case resp.StatusCode >= 300:
		toolError(w, fmt.Sprintf("the file could not be moved: the Workspace refused the file (HTTP %d)", resp.StatusCode))
		return
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20+1))
	if len(data) > 50<<20 {
		toolError(w, "the file is larger than the transfer limit")
		return
	}
	s.mu.Lock()
	files, ok := s.workspaces[ws]
	if ok {
		files[path] = data
		s.imported = append(s.imported, len(data))
	}
	s.mu.Unlock()
	if !ok {
		toolError(w, "workspace not found")
		return
	}
	toolOK(w, fmt.Sprintf("imported %s (%d bytes) to %s", resp.Header.Get("Content-Disposition"), len(data), path), map[string]any{"path": path, "size": len(data)})
}

var command = regexp.MustCompile(`(?m)\b(pdftext|ooxml2md)\s+(\S+)(.*)$`)

func (s *Sandbox) run(w http.ResponseWriter, ws, language, code string) {
	s.mu.Lock()
	s.scripts = append(s.scripts, code)
	limit := s.MaxRuns
	if limit == 0 {
		limit = 8
	}
	if s.runs >= limit {
		s.mu.Unlock()
		toolError(w, "the sandbox is at capacity; try again later")
		return
	}
	s.runs++
	s.maxRuns = max(s.maxRuns, s.runs)
	files, ok := s.workspaces[ws]
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.runs--; s.mu.Unlock() }()
	if !ok {
		toolError(w, "workspace not found")
		return
	}
	m := command.FindStringSubmatch(code)
	if language != "office" || m == nil {
		toolError(w, "the fake runs the converters only")
		return
	}
	s.mu.Lock()
	data := files[m[2]]
	s.mu.Unlock()
	out := Output{Exit: 4, Stderr: m[1] + ": error: corrupt: no such file " + m[2]}
	if data != nil && s.Convert != nil {
		out = s.Convert(m[1], m[2], data)
	}
	if out.Delay > 0 {
		time.Sleep(out.Delay)
	}
	if out.Exit == 0 && !out.TimedOut && !out.OOM {
		s.mu.Lock()
		files["out/extract.md"] = []byte(out.Markdown)
		s.mu.Unlock()
	}
	toolOK(w, fmt.Sprintf("exit code %d", out.Exit), map[string]any{"exitCode": out.Exit, "stdout": "", "stderr": out.Stderr, "timedOut": out.TimedOut, "outOfMemory": out.OOM, "durationMs": 1})
}

func (s *Sandbox) exportFile(w http.ResponseWriter, ws, path, target, name string) {
	s.mu.Lock()
	s.tickets = append(s.tickets, target)
	s.mu.Unlock()
	if msg := s.forTicket(target, "write"); msg != "" {
		toolError(w, msg)
		return
	}
	s.mu.Lock()
	data, ok := s.workspaces[ws][path]
	s.mu.Unlock()
	if !ok {
		toolError(w, "no such file in the workspace")
		return
	}
	req, _ := http.NewRequest(http.MethodPost, s.FilesURL, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+target)
	req.Header.Set("Content-Type", "text/markdown")
	req.Header.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		toolError(w, "the file could not be moved: the Workspace did not answer: "+err.Error())
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		toolError(w, "the file reference is not valid for this run (use the attachment or output reference you were given)")
		return
	case resp.StatusCode == 413:
		toolError(w, "the file is larger than the transfer limit")
		return
	case resp.StatusCode >= 300:
		toolError(w, fmt.Sprintf("the file could not be moved: the Workspace refused the file (HTTP %d)", resp.StatusCode))
		return
	}
	var f struct {
		Ref, Name string
		Size      int64
		SHA256    string
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&f) != nil || f.Ref == "" {
		toolError(w, "the file could not be moved: the Workspace's answer has no file reference")
		return
	}
	toolOK(w, fmt.Sprintf("delivered %s (%d bytes) to the member", f.Name, f.Size), map[string]any{"deliveredFile": f})
}
