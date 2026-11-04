// Package approvals verifies an approval reference with the approval system before an
// administrator's status transition takes effect (ADR-0001 D30). The CMI sends what is being
// approved and expects a decision bound to that object and a validity window; it never admits a
// transition on a cached or assumed answer.
package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Request is what the CMI asks the approval system to confirm.
type Request struct {
	ApprovalRef string `json:"approvalRef"`
	Tenant      string `json:"tenant"`
	RecordID    string `json:"recordId"`
	To          string `json:"to"`
	Principal   string `json:"principal"`
}

// Approval is the approval system's answer. RecordID and To, when present, must echo the request.
type Approval struct {
	Valid      bool       `json:"valid"`
	ApprovedBy string     `json:"approvedBy"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	RecordID   string     `json:"recordId,omitempty"`
	To         string     `json:"to,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

var (
	// ErrInvalid marks an approval that is not valid, has expired or is bound to another object.
	ErrInvalid = errors.New("approval invalid")
	// ErrUnavailable marks an approval system that could not answer.
	ErrUnavailable = errors.New("approval system unavailable")
)

// Client calls one approval system.
type Client struct {
	URL     string
	HTTP    *http.Client
	Timeout time.Duration
	Now     func() time.Time
}

// Verify asks the approval system and checks the answer against the request.
func (c *Client) Verify(ctx context.Context, in Request) (*Approval, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	var out Approval
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: malformed answer", ErrUnavailable)
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	switch {
	case !out.Valid:
		reason := out.Reason
		if reason == "" {
			reason = "not valid"
		}
		return nil, fmt.Errorf("%w: %s", ErrInvalid, reason)
	case out.ApprovedBy == "":
		return nil, fmt.Errorf("%w: no approver", ErrInvalid)
	case out.ExpiresAt == nil || !out.ExpiresAt.After(now()):
		return nil, fmt.Errorf("%w: expired", ErrInvalid)
	case out.RecordID != "" && out.RecordID != in.RecordID, out.To != "" && out.To != in.To:
		return nil, fmt.Errorf("%w: bound to another object", ErrInvalid)
	}
	return &out, nil
}
