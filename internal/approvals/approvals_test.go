package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	answer := map[string]any{}
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in Request
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ApprovalRef == "" || in.RecordID == "" {
			http.Error(w, "bad", 400)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }}
	req := Request{ApprovalRef: "chg:1", Tenant: "t", RecordID: "r1", To: "active", Principal: "alice"}
	cases := []struct {
		name   string
		answer map[string]any
		status int
		want   error
	}{
		{"valid", map[string]any{"valid": true, "approvedBy": "erin", "expiresAt": now.Add(time.Hour), "recordId": "r1", "to": "active"}, 200, nil},
		{"rejected", map[string]any{"valid": false, "reason": "withdrawn"}, 200, ErrInvalid},
		{"expired", map[string]any{"valid": true, "approvedBy": "erin", "expiresAt": now.Add(-time.Second)}, 200, ErrInvalid},
		{"no-expiry", map[string]any{"valid": true, "approvedBy": "erin"}, 200, ErrInvalid},
		{"other-object", map[string]any{"valid": true, "approvedBy": "erin", "expiresAt": now.Add(time.Hour), "recordId": "r2"}, 200, ErrInvalid},
		{"other-transition", map[string]any{"valid": true, "approvedBy": "erin", "expiresAt": now.Add(time.Hour), "to": "revoked"}, 200, ErrInvalid},
		{"down", map[string]any{}, 503, ErrUnavailable},
	}
	for _, tc := range cases {
		answer, status = tc.answer, tc.status
		got, err := c.Verify(context.Background(), req)
		if tc.want == nil {
			if err != nil || got.ApprovedBy != "erin" {
				t.Fatalf("%s: %v %v", tc.name, got, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	c.URL = "http://127.0.0.1:1"
	if _, err := c.Verify(context.Background(), req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}
