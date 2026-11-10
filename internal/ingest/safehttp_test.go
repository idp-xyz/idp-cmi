package ingest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"idp-cmi/adapters/store/postgres"
)

func TestPublicAddress(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "93.184.216.34": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.9": false, "192.168.1.1": false,
		"169.254.169.254": false, "fe80::1": false, "fd00::1": false, "100.64.0.1": false, "0.0.0.0": false, "224.0.0.1": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "198.18.0.5": false, "240.0.0.1": false,
	} {
		if got := publicAddress(net.ParseIP(ip)); got != want {
			t.Errorf("%s: public=%v, want %v", ip, got, want)
		}
	}
}

func TestSafeClientRefusesNonPublicDestinations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("internal")) }))
	defer srv.Close()
	if resp, err := NewHTTPClient(false).Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("a loopback server must not be reachable")
	} else if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("%v", err)
	}
	resp, err := NewHTTPClient(true).Get(srv.URL)
	if err != nil {
		t.Fatalf("an operator who allows private destinations reaches it: %v", err)
	}
	resp.Body.Close()
}

func TestFetchStaysUnderTheBaseAndItsRedirectsToo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/docs/a.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	})
	mux.HandleFunc("/docs/inside", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/docs/a.txt", http.StatusFound) })
	mux.HandleFunc("/docs/out", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/secret.txt", http.StatusFound) })
	mux.HandleFunc("/docs/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/docs/loop", http.StatusFound) })
	mux.HandleFunc("/secret.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("SECRET")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := &Ingester{HTTP: NewHTTPClient(true)}
	c := &postgres.Connector{Kind: postgres.ConnectorHTTPManifest, Config: postgres.ConnectorConfig{BaseURL: srv.URL + "/docs/", URL: srv.URL + "/docs/manifest.json"}}
	if raw, _, err := g.fetch(context.Background(), c, srv.URL+"/docs/a.txt"); err != nil || string(raw) != "hello" {
		t.Fatalf("%q %v", raw, err)
	}
	if raw, _, err := g.fetch(context.Background(), c, srv.URL+"/docs/inside"); err != nil || string(raw) != "hello" {
		t.Fatalf("a redirect within the base is followed: %q %v", raw, err)
	}
	for name, u := range map[string]string{
		"redirect-out":   srv.URL + "/docs/out",
		"loop":           srv.URL + "/docs/loop",
		"outside":        srv.URL + "/secret.txt",
		"dot-dot":        srv.URL + "/docs/../secret.txt",
		"encoded-dotdot": srv.URL + "/docs/%2e%2e/secret.txt",
		"userinfo":       strings.Replace(srv.URL, "http://", "http://docs@", 1) + "/docs/a.txt",
	} {
		if raw, _, err := g.fetch(context.Background(), c, u); err == nil {
			t.Errorf("%s: fetched %q", name, raw)
		}
	}
}

func TestFilesystemConnectorsAreConfinedToTheOperatorsRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("ok"), 0o644)
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET"), 0o644)
	_ = os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "docs", "link.txt"))
	_ = os.Symlink(outside, filepath.Join(root, "escape"))

	g := &Ingester{}
	if _, err := g.FilesystemPath(filepath.Join(root, "docs")); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("without a root the connectors are disabled: %v", err)
	}
	g.FilesystemRoot = root
	if p, err := g.FilesystemPath(filepath.Join(root, "docs")); err != nil || !strings.HasSuffix(p, "docs") {
		t.Fatalf("%q %v", p, err)
	}
	for _, bad := range []string{outside, "/etc", filepath.Join(root, "escape"), filepath.Join(root, "docs", "..", "..")} {
		if _, err := g.FilesystemPath(bad); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
	c := &postgres.Connector{Kind: postgres.ConnectorFilesystem, Config: postgres.ConnectorConfig{Path: filepath.Join(root, "docs")}}
	if text, err := g.load(context.Background(), c, filepath.Join(root, "docs", "a.txt")); err != nil || text != "ok" {
		t.Fatalf("%q %v", text, err)
	}
	if text, err := g.load(context.Background(), c, filepath.Join(root, "docs", "link.txt")); err == nil {
		t.Fatalf("a symbolic link out of the root was read: %q", text)
	}
}
