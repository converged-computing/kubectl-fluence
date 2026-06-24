// Package selection ties providers and policies together: it merges attributes
// from multiple providers onto a single candidate set, intersects that set with
// what the cluster scheduler actually offers, then runs the policy pipeline to
// pick a winner.
//
// The intersection is important: a user's attribute file may describe backends
// the scheduler does not offer (a provider/vendor the cluster's resource graph
// doesn't expose), and the scheduler may offer backends the file doesn't price.
// We can only select among backends that are BOTH schedulable and have the
// attributes the chosen policies require, so the engine intersects and reports
// what it dropped and why.
package selection

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/converged-computing/kubectl-fluence/pkg/policies"
	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

// Result is the outcome of a selection, including an audit trail.
type Result struct {
	Chosen     string            `json:"chosen"`
	Policy     string            `json:"policy"`
	Candidates []types.Backend   `json:"candidates"`
	Dropped    map[string]string `json:"dropped,omitempty"` // name -> reason
	Notes      []string          `json:"notes,omitempty"`
}

// Engine runs a selection.
type Engine struct {
	Providers []types.Provider
	// SchedulerOffers is the set of backend names the cluster scheduler exposes
	// (from the fluence-resources ConfigMap). If empty, no intersection is done
	// (all file/provider backends are eligible) — used when the ConfigMap is
	// unavailable; a note is added so the user knows the intersection was skipped.
	SchedulerOffers []string
	CostTol         float64
}

// mergeCandidates collects backends from all providers and merges their
// attributes by backend name (so cost from one provider and queue from another
// land on the same Backend).
func (e *Engine) mergeCandidates(ctx context.Context, only []string, req types.Request) (map[string]*types.Backend, error) {
	merged := map[string]*types.Backend{}
	for _, p := range e.Providers {
		cands, err := p.Candidates(ctx, only, req)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", p.Name(), err)
		}
		for _, c := range cands {
			b, ok := merged[c.Name]
			if !ok {
				cc := c
				merged[c.Name] = &cc
				continue
			}
			// merge attributes/strings; later providers fill gaps + override
			for k, v := range c.Attributes {
				b.SetAttr(k, v)
			}
			for k, v := range c.Strings {
				b.SetStr(k, v)
			}
			if b.DeviceARN == "" {
				b.DeviceARN = c.DeviceARN
			}
			if b.Region == "" {
				b.Region = c.Region
			}
		}
	}
	return merged, nil
}

// Select runs the full pipeline and returns the chosen backend + audit.
//
// policySpec is a comma-separated pipeline, e.g. "online-only,min-cost,min-queue".
func (e *Engine) Select(ctx context.Context, policySpec string, only []string, req types.Request) (*Result, error) {
	res := &Result{Policy: policySpec, Dropped: map[string]string{}}

	merged, err := e.mergeCandidates(ctx, only, req)
	if err != nil {
		return nil, err
	}

	// Intersect with what the scheduler offers.
	offers := map[string]bool{}
	for _, n := range e.SchedulerOffers {
		offers[n] = true
	}
	var pool []types.Backend
	for name, b := range merged {
		if len(offers) > 0 && !offers[name] {
			res.Dropped[name] = "not offered by the cluster scheduler"
			continue
		}
		pool = append(pool, *b)
	}
	// Backends the scheduler offers but we have no attributes for are noted too.
	for _, n := range e.SchedulerOffers {
		if _, ok := merged[n]; !ok {
			res.Dropped[n] = "offered by scheduler but no attributes available (not in file/providers)"
		}
	}
	if len(e.SchedulerOffers) == 0 {
		res.Notes = append(res.Notes,
			"scheduler offerings unknown (ConfigMap unavailable); selecting over all provided backends")
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("no candidate backends after intersecting providers with scheduler offerings")
	}
	// stable order for determinism before policies run
	sort.Slice(pool, func(i, j int) bool { return pool[i].Name < pool[j].Name })

	// Build and run the policy pipeline.
	tokens := splitPipeline(policySpec)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty selection policy")
	}
	cur := pool
	for _, tok := range tokens {
		pol, err := policies.Lookup(tok, e.CostTol)
		if err != nil {
			return nil, err
		}
		next, err := pol.Apply(cur, req)
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", tok, err)
		}
		cur = next
		if len(cur) == 0 {
			return nil, fmt.Errorf("policy %q eliminated all candidates", tok)
		}
	}

	res.Candidates = cur
	res.Chosen = cur[0].Name
	return res, nil
}

func splitPipeline(spec string) []string {
	var out []string
	for _, t := range strings.Split(spec, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
