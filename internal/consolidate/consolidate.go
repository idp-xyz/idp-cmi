package consolidate

import (
	"context"
	"errors"
	"log/slog"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
)

// Consolidator runs distillations: for every tenant (or one), every group large enough.
type Consolidator struct {
	Store  *postgres.Store
	Model  Model
	Source string // the registered agent source statements are attributed to
	Log    *slog.Logger
}

// Summary is what one run did.
type Summary struct {
	Groups    int `json:"groups"`
	Completed int `json:"completed"`
	Rejected  int `json:"rejected"`
	Failed    int `json:"failed"`
	Created   int `json:"created"`
	// Busy: another replica is consolidating this tenant right now; nothing was done here.
	Busy bool `json:"busy,omitempty"`
}

// ErrBusy is returned by RunTenant when another replica holds the tenant's consolidation lock.
var ErrBusy = errors.New("consolidation of this tenant is already running")

// RunTenant consolidates one tenant; scope narrows it to one scope. The configured source must be
// a registered agent source of the tenant, otherwise nothing is written and an error says so.
func (c *Consolidator) RunTenant(ctx context.Context, tenant string, scope *cmi.Scope) (*Summary, error) {
	// one replica at a time per tenant: two would distill the same episodic records twice
	release, ok, err := c.Store.TryWorkLock(ctx, "consolidate", tenant)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Summary{Busy: true}, ErrBusy
	}
	defer release()
	src, err := c.Store.GetSource(ctx, tenant, c.Source)
	if err != nil {
		return nil, err
	}
	if src.Kind != cmi.SourceAgent {
		return nil, &postgres.Error{Code: "CONFLICT", Message: "consolidation source " + c.Source + " must be an agent source"}
	}
	policy, err := c.Store.GetPolicy(ctx, tenant)
	if err != nil {
		return nil, err
	}
	groups, err := c.Store.ConsolidationGroups(ctx, tenant, policy.ConsolidateMinRecords, scope)
	if err != nil {
		return nil, err
	}
	sum := &Summary{Groups: len(groups)}
	for _, g := range groups {
		notes := make([]string, 0, len(g.Records))
		for _, r := range g.Records {
			notes = append(notes, r.Content)
		}
		statements, merr := c.Model.Distill(ctx, notes)
		res, err := c.Store.Consolidate(ctx, tenant, g, src.Name, c.Model.Name(), statements, merr)
		if err != nil {
			return sum, err
		}
		switch res.Status {
		case "completed":
			sum.Completed++
		case "rejected":
			sum.Rejected++
		default:
			sum.Failed++
		}
		sum.Created += len(res.OutputIDs)
	}
	return sum, nil
}

// RunAll consolidates every tenant; tenants without the source are skipped.
func (c *Consolidator) RunAll(ctx context.Context) (*Summary, error) {
	tenants, err := c.Store.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	total := &Summary{}
	for _, t := range tenants {
		s, err := c.RunTenant(ctx, t, nil)
		if err != nil {
			if c.Log != nil {
				c.Log.Debug("consolidation skipped", "tenant", t, "reason", err)
			}
			continue
		}
		total.Groups += s.Groups
		total.Completed += s.Completed
		total.Rejected += s.Rejected
		total.Failed += s.Failed
		total.Created += s.Created
	}
	return total, nil
}
