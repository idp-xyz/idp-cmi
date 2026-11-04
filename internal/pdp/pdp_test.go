package pdp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"idp-cmi/internal/cmi"
)

func TestDecide(t *testing.T) {
	var seen Input
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input Input `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = body.Input
		switch body.Input.Purpose {
		case "export":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"allow": false, "reason": "no exports"}})
		case "broken":
			_, _ = w.Write([]byte(`{"result": {"allow": true, "obligations": {"maxSensitivity": "secret"}}}`))
		case "down":
			http.Error(w, "x", 500)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"allow": true, "obligations": map[string]any{"maxSensitivity": "public", "excludeSources": []string{"assistant"}}}})
		}
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, HTTP: srv.Client()}
	in := Input{Tenant: "t", Principal: Principal{Subject: "carol", Roles: []string{"cmi-reader"}}, Action: "context.resolve", Resource: Resource{Kind: "context", Scope: &cmi.Scope{Kind: "thread", ID: "x"}}, Purpose: "answer"}
	d, err := c.Decide(context.Background(), in)
	if err != nil || !d.Allow || d.Obligations.MaxSensitivity != "public" || len(d.Obligations.ExcludeSources) != 1 {
		t.Fatalf("%+v %v", d, err)
	}
	if seen.Principal.Subject != "carol" || seen.Resource.Scope.ID != "x" {
		t.Fatalf("%+v", seen)
	}
	in.Purpose = "export"
	if d, err := c.Decide(context.Background(), in); err != nil || d.Allow || d.Reason != "no exports" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, p := range []string{"broken", "down"} {
		in.Purpose = p
		if _, err := c.Decide(context.Background(), in); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	c.URL = "http://127.0.0.1:1"
	if _, err := c.Decide(context.Background(), in); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
