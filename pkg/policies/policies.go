// Package policies implements the Policy interface from pkg/types. Each policy
// is a small, single-purpose, PURE function over backend attributes. Rankers
// (min/max over a named attribute) share one generic core, so adding "rank by
// X" is a one-liner. Filters reduce the candidate set without reordering.
//
// Policies are looked up by name and composed into a left-to-right pipeline by
// the selection engine (e.g. "min-cost,min-queue" = cheapest set, then ordered
// by queue depth).
package policies

import (
	"fmt"
	"sort"
	"strings"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

// CostPerRequest is the attribute key the cost policies rank on. It is computed
// (not stored) from cost_per_task + shots*cost_per_shot (QPU) or
// cost_per_minute*est (sim) and injected as this key before policies run; see
// pkg/selection. Policies themselves stay pure and just read it.
const (
	AttrCost   = "cost_usd"   // estimated USD for THIS request
	AttrQueue  = "queue_size" // device queue depth (normal queue)
	AttrQubits = "qubits"     // device qubit count
	StrStatus  = "status"     // "ONLINE"/"OFFLINE"/...
)

// ranker ranks by a numeric attribute in a direction, and optionally narrows to
// the best-equivalent group (those within `tol` of the best value) so a
// downstream policy can break ties. This single type implements all min-*/max-*
// policies.
type ranker struct {
	name   string
	attr   string
	ascend bool    // true = smaller is better (min); false = larger is better (max)
	tol    float64 // keep candidates within this absolute tolerance of the best
	narrow bool    // if true, drop all but the best-equivalent group
}

func (r ranker) Name() string                 { return r.name }
func (r ranker) RequiredAttributes() []string { return []string{r.attr} }

func (r ranker) Apply(in []types.Backend, _ types.Request) ([]types.Backend, error) {
	// keep only backends that have the attribute
	var have []types.Backend
	var missing []string
	for _, b := range in {
		if _, ok := b.Attr(r.attr); ok {
			have = append(have, b)
		} else {
			missing = append(missing, b.Name)
		}
	}
	if len(have) == 0 {
		return nil, fmt.Errorf("policy %q: no candidate has attribute %q (missing: %s)",
			r.name, r.attr, strings.Join(missing, ","))
	}
	sort.SliceStable(have, func(i, j int) bool {
		vi := have[i].Attributes[r.attr]
		vj := have[j].Attributes[r.attr]
		if r.ascend {
			return vi < vj
		}
		return vi > vj
	})
	if r.narrow {
		best := have[0].Attributes[r.attr]
		var keep []types.Backend
		for _, b := range have {
			v := b.Attributes[r.attr]
			d := v - best
			if d < 0 {
				d = -d
			}
			if d <= r.tol {
				keep = append(keep, b)
			}
		}
		return keep, nil
	}
	return have, nil
}

// filter keeps backends satisfying a predicate, preserving order.
type filter struct {
	name string
	attr string
	pred func(types.Backend) bool
}

func (f filter) Name() string { return f.name }
func (f filter) RequiredAttributes() []string {
	if f.attr == "" {
		return nil
	}
	return []string{f.attr}
}
func (f filter) Apply(in []types.Backend, _ types.Request) ([]types.Backend, error) {
	var out []types.Backend
	for _, b := range in {
		if f.pred(b) {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("policy %q removed all candidates", f.name)
	}
	return out, nil
}

// preferPolicy chooses a named backend if present, else passes input through
// unchanged (so the next policy decides). "prefer:rigetti_cepheus".
type preferPolicy struct {
	name   string
	target string
}

func (p preferPolicy) Name() string                 { return p.name }
func (p preferPolicy) RequiredAttributes() []string { return nil }
func (p preferPolicy) Apply(in []types.Backend, _ types.Request) ([]types.Backend, error) {
	for _, b := range in {
		if b.Name == p.target {
			return []types.Backend{b}, nil
		}
	}
	return in, nil // not available; defer to the rest of the pipeline
}

// Registry maps a policy token to a constructed Policy. Tokens may carry a
// parameter after ':' (e.g. "prefer:iqm_garnet", "require:min_qubits=10").
func Lookup(token string, tol float64) (types.Policy, error) {
	tok := strings.TrimSpace(token)
	switch {
	case tok == "min-cost":
		return ranker{name: "min-cost", attr: AttrCost, ascend: true, tol: tol, narrow: true}, nil
	case tok == "max-cost":
		return ranker{name: "max-cost", attr: AttrCost, ascend: false, tol: tol, narrow: true}, nil
	case tok == "min-queue":
		return ranker{name: "min-queue", attr: AttrQueue, ascend: true, tol: tol, narrow: true}, nil
	case tok == "max-queue":
		return ranker{name: "max-queue", attr: AttrQueue, ascend: false, tol: tol, narrow: true}, nil
	case tok == "min-qubits":
		return ranker{name: "min-qubits", attr: AttrQubits, ascend: true, tol: 0, narrow: true}, nil
	case tok == "max-qubits":
		return ranker{name: "max-qubits", attr: AttrQubits, ascend: false, tol: 0, narrow: true}, nil
	case tok == "online-only":
		return filter{name: "online-only", attr: "", pred: func(b types.Backend) bool {
			s, ok := b.Str(StrStatus)
			return !ok || strings.EqualFold(s, "ONLINE")
		}}, nil
	case strings.HasPrefix(tok, "prefer:"):
		return preferPolicy{name: tok, target: strings.TrimPrefix(tok, "prefer:")}, nil
	case strings.HasPrefix(tok, "require:"):
		return parseRequire(tok)
	default:
		return nil, fmt.Errorf("unknown policy %q", tok)
	}
}

// canonAttr maps user-friendly attribute names to the internal keys policies
// rank/filter on, so a user can write require:max_cost=8 instead of needing to
// know the internal cost_usd key.
func canonAttr(a string) string {
	switch a {
	case "cost":
		return AttrCost // cost_usd
	case "queue":
		return AttrQueue // queue_size
	default:
		return a
	}
}

// parseRequire builds a numeric-threshold filter, e.g. "require:min_qubits=10"
// keeps backends with qubits>=10; "require:max_cost=8" keeps cost<=8.
func parseRequire(tok string) (types.Policy, error) {
	body := strings.TrimPrefix(tok, "require:")
	parts := strings.SplitN(body, "=", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed require %q (want require:min_X=N or require:max_X=N)", tok)
	}
	key, valStr := parts[0], parts[1]
	var val float64
	if _, err := fmt.Sscanf(valStr, "%g", &val); err != nil {
		return nil, fmt.Errorf("require %q: bad number %q", tok, valStr)
	}
	var attr string
	var lowerBound bool
	switch {
	case strings.HasPrefix(key, "min_"):
		attr, lowerBound = strings.TrimPrefix(key, "min_"), true
	case strings.HasPrefix(key, "max_"):
		attr, lowerBound = strings.TrimPrefix(key, "max_"), false
	default:
		return nil, fmt.Errorf("require %q: key must start with min_ or max_", tok)
	}
	attr = canonAttr(attr)
	return filter{name: tok, attr: attr, pred: func(b types.Backend) bool {
		v, ok := b.Attr(attr)
		if !ok {
			return false
		}
		if lowerBound {
			return v >= val
		}
		return v <= val
	}}, nil
}
