// Package pdp asks an external policy decision point before the CMI serves a protected request
// (ADR-0001 D23). The CMI's own roles, ACLs and sensitivity ceilings stay as the inner layer; the
// PDP decides on tenant, principal, delegation, action, resource and purpose, and may add
// obligations that only narrow what the inner layer allows. The wire format is OPA's data API
// ({"input": ...} in, {"result": ...} out) so OPA or any service speaking it can be the PDP.
package pdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"idp-cmi/internal/cmi"
)

// Input is the question.
type Input struct {
	Tenant    string    `json:"tenant"`
	Principal Principal `json:"principal"`
	Action    string    `json:"action"` // context.resolve | memory.write | memory.read | memory.revoke | memory.transition | memory.share | knowledge.write | knowledge.revoke | erasure.request
	Resource  Resource  `json:"resource"`
	Purpose   string    `json:"purpose,omitempty"`
}

// Principal is who asks: the subject, its roles, the OAuth client and the actor of a delegated token.
type Principal struct {
	Subject string   `json:"subject"`
	Roles   []string `json:"roles"`
	Client  string   `json:"client,omitempty"`
	Actor   string   `json:"actor,omitempty"`
}

// Resource is what is asked about.
type Resource struct {
	Kind        string       `json:"kind"` // context | memory | knowledge | erasure
	ID          string       `json:"id,omitempty"`
	Scope       *cmi.Scope   `json:"scope,omitempty"`
	Scopes      []cmi.Scope  `json:"scopes,omitempty"`
	Sensitivity string       `json:"sensitivity,omitempty"`
	Source      string       `json:"source,omitempty"`
	Entities    []cmi.Entity `json:"entities,omitempty"`
}

// Obligations narrow a permitted read.
type Obligations struct {
	MaxSensitivity string   `json:"maxSensitivity,omitempty"` // lowers the caller's ceiling
	ExcludeSources []string `json:"excludeSources,omitempty"` // never return content of these sources
}

// Decision is the answer.
type Decision struct {
	Allow       bool        `json:"allow"`
	Reason      string      `json:"reason,omitempty"`
	Obligations Obligations `json:"obligations"`
}

// ErrUnavailable marks a PDP that could not answer (unreachable, non-200, malformed).
var ErrUnavailable = errors.New("pdp unavailable")

// Client calls one PDP.
type Client struct {
	URL     string // the decision endpoint, e.g. https://opa:8181/v1/data/idp/cmi/decision
	HTTP    *http.Client
	Timeout time.Duration
}

// Decide asks the PDP. An unreachable or malformed PDP is ErrUnavailable; the caller decides what
// that means (deny by default).
func (c *Client) Decide(ctx context.Context, in Input) (*Decision, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"input": in})
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
	var out struct {
		Result *Decision `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Result == nil {
		return nil, fmt.Errorf("%w: no result in the answer", ErrUnavailable)
	}
	if ms := out.Result.Obligations.MaxSensitivity; ms != "" && !cmi.ValidSensitivity(ms) {
		return nil, fmt.Errorf("%w: obligation maxSensitivity %q is not a level", ErrUnavailable, ms)
	}
	return out.Result, nil
}
