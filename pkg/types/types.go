// Package types defines the core abstractions shared across kubectl-fluence:
// the Backend (a candidate quantum device with attributes), the Request (the
// work being submitted, e.g. shot count), and the two orthogonal interfaces
// that keep retrieval separate from operation:
//
//   - Provider: retrieves candidate backends and their attributes (the only
//     thing that touches credentials or external APIs).
//   - Policy:   a pure function that filters/ranks backends by their attributes
//     (never does I/O).
//
// This separation lets a user compose, e.g., "cheapest set, then ordered by
// queue depth" by pulling cost from one provider and queue from another, then
// running a pipeline of policies over the merged attributes.
package types

import "context"

// Backend is a candidate quantum device with numeric and string attributes.
// Attributes are merged across providers (one provider may supply cost, another
// queue depth), so a Backend accumulates everything known about it before
// policies run.
type Backend struct {
	Name       string             `json:"name"`
	DeviceARN  string             `json:"device_arn,omitempty"`
	Region     string             `json:"region,omitempty"`
	Provider   string             `json:"provider,omitempty"` // e.g. "braket"
	Attributes map[string]float64 `json:"attributes,omitempty"`
	Strings    map[string]string  `json:"strings,omitempty"`
}

// Attr returns a numeric attribute and whether it was present.
func (b Backend) Attr(key string) (float64, bool) {
	v, ok := b.Attributes[key]
	return v, ok
}

// Str returns a string attribute and whether it was present.
func (b Backend) Str(key string) (string, bool) {
	v, ok := b.Strings[key]
	return v, ok
}

// SetAttr sets a numeric attribute, allocating the map if needed.
func (b *Backend) SetAttr(key string, val float64) {
	if b.Attributes == nil {
		b.Attributes = map[string]float64{}
	}
	b.Attributes[key] = val
}

// SetStr sets a string attribute, allocating the map if needed.
func (b *Backend) SetStr(key, val string) {
	if b.Strings == nil {
		b.Strings = map[string]string{}
	}
	b.Strings[key] = val
}

// Request describes the work being submitted, used by policies that need the
// request size (e.g. cost = per_task + shots*per_shot).
type Request struct {
	Shots       int                // number of shots in the quantum task
	NWorkers    int                // gang worker count (for reporting)
	Constraints map[string]float64 // parsed from select-constraints, e.g. min_qubits=10
}

// Provider retrieves candidate backends and their attributes. Providers are the
// only components permitted to touch credentials or external APIs.
type Provider interface {
	// Name identifies the provider, e.g. "braket-graph", "braket-live".
	Name() string
	// Candidates returns backends this provider knows about. If `only` is
	// non-empty, the provider should restrict to those names. The Request is
	// supplied for providers whose attributes depend on request size.
	Candidates(ctx context.Context, only []string, req Request) ([]Backend, error)
}

// Policy filters and/or ranks backends. Policies MUST be pure: they depend only
// on the supplied backends and request, never on I/O. Apply returns an ordered
// slice, best first; an empty result means "nothing satisfied this policy".
type Policy interface {
	Name() string
	Apply(in []Backend, req Request) ([]Backend, error)
	// RequiredAttributes lists the attribute keys this policy reads, so the
	// engine can retrieve only what is needed (and warn if a backend lacks it).
	RequiredAttributes() []string
}
