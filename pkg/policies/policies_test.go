package policies

import (
	"testing"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

func be(name string, attrs map[string]float64, strs map[string]string) types.Backend {
	return types.Backend{Name: name, Attributes: attrs, Strings: strs}
}

func names(bs []types.Backend) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}

func TestRankerMinNarrowsToBest(t *testing.T) {
	p, err := Lookup("min-cost", 0)
	if err != nil {
		t.Fatal(err)
	}
	in := []types.Backend{
		be("a", map[string]float64{AttrCost: 5}, nil),
		be("b", map[string]float64{AttrCost: 1}, nil),
		be("c", map[string]float64{AttrCost: 1}, nil),
	}
	out, err := p.Apply(in, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	// min-cost narrows to the best-equivalent group (cost==1): b and c, a dropped.
	if len(out) != 2 {
		t.Fatalf("expected 2 best-equivalent, got %v", names(out))
	}
	for _, b := range out {
		if b.Attributes[AttrCost] != 1 {
			t.Fatalf("non-best survived: %v", names(out))
		}
	}
}

func TestRankerTolerance(t *testing.T) {
	// with tol=0.5, costs 1.0 and 1.4 are "equivalent"; 2.0 is not.
	p, _ := Lookup("min-cost", 0.5)
	in := []types.Backend{
		be("a", map[string]float64{AttrCost: 1.0}, nil),
		be("b", map[string]float64{AttrCost: 1.4}, nil),
		be("c", map[string]float64{AttrCost: 2.0}, nil),
	}
	out, _ := p.Apply(in, types.Request{})
	if len(out) != 2 {
		t.Fatalf("tol=0.5 should keep a,b; got %v", names(out))
	}
}

func TestRankerMaxDirection(t *testing.T) {
	p, _ := Lookup("max-queue", 0)
	in := []types.Backend{
		be("a", map[string]float64{AttrQueue: 3}, nil),
		be("b", map[string]float64{AttrQueue: 50}, nil),
	}
	out, _ := p.Apply(in, types.Request{})
	if out[0].Name != "b" {
		t.Fatalf("max-queue head = %q, want b", out[0].Name)
	}
}

func TestRankerMissingAttributeErrors(t *testing.T) {
	p, _ := Lookup("min-queue", 0)
	in := []types.Backend{be("a", map[string]float64{AttrCost: 1}, nil)} // no queue_size
	if _, err := p.Apply(in, types.Request{}); err == nil {
		t.Fatal("expected error when no candidate has the ranked attribute")
	}
}

func TestOnlineOnly(t *testing.T) {
	p, _ := Lookup("online-only", 0)
	in := []types.Backend{
		be("on", nil, map[string]string{StrStatus: "ONLINE"}),
		be("off", nil, map[string]string{StrStatus: "OFFLINE"}),
		be("unknown", nil, nil), // no status -> kept (permissive)
	}
	out, _ := p.Apply(in, types.Request{})
	got := map[string]bool{}
	for _, b := range out {
		got[b.Name] = true
	}
	if got["off"] {
		t.Fatal("OFFLINE device should be filtered")
	}
	if !got["on"] || !got["unknown"] {
		t.Fatalf("ONLINE and unknown-status should survive, got %v", names(out))
	}
}

func TestRequireMinAndMax(t *testing.T) {
	in := []types.Backend{
		be("small", map[string]float64{"qubits": 5, AttrCost: 1}, nil),
		be("big", map[string]float64{"qubits": 100, AttrCost: 9}, nil),
	}
	pmin, err := Lookup("require:min_qubits=10", 0)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := pmin.Apply(in, types.Request{})
	if len(out) != 1 || out[0].Name != "big" {
		t.Fatalf("require:min_qubits=10 -> %v, want [big]", names(out))
	}
	pmax, _ := Lookup("require:max_cost=5", 0)
	out2, _ := pmax.Apply(in, types.Request{})
	if len(out2) != 1 || out2[0].Name != "small" {
		t.Fatalf("require:max_cost=5 -> %v, want [small]", names(out2))
	}
}

func TestRequireMalformed(t *testing.T) {
	for _, bad := range []string{"require:qubits=10", "require:min_qubits", "require:min_qubits=abc"} {
		if _, err := Lookup(bad, 0); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestPrefer(t *testing.T) {
	in := []types.Backend{
		be("a", map[string]float64{AttrCost: 1}, nil),
		be("b", map[string]float64{AttrCost: 2}, nil),
	}
	// prefer present -> picks it
	p, _ := Lookup("prefer:b", 0)
	out, _ := p.Apply(in, types.Request{})
	if len(out) != 1 || out[0].Name != "b" {
		t.Fatalf("prefer:b -> %v, want [b]", names(out))
	}
	// prefer absent -> passes input through for the next policy
	p2, _ := Lookup("prefer:zzz", 0)
	out2, _ := p2.Apply(in, types.Request{})
	if len(out2) != 2 {
		t.Fatalf("prefer:absent should pass through all, got %v", names(out2))
	}
}

func TestUnknownPolicy(t *testing.T) {
	if _, err := Lookup("frobnicate", 0); err == nil {
		t.Fatal("expected error for unknown policy")
	}
}
