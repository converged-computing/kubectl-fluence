package selection

import (
	"context"
	"testing"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

// fakeProvider returns fixed backends for tests (no I/O).
type fakeProvider struct {
	name string
	bes  []types.Backend
}

func (f fakeProvider) Name() string { return f.name }
func (f fakeProvider) Candidates(_ context.Context, only []string, _ types.Request) ([]types.Backend, error) {
	if len(only) == 0 {
		return f.bes, nil
	}
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	var out []types.Backend
	for _, b := range f.bes {
		if want[b.Name] {
			out = append(out, b)
		}
	}
	return out, nil
}

func be(name string, attrs map[string]float64, strs map[string]string) types.Backend {
	return types.Backend{Name: name, DeviceARN: "arn:" + name, Region: "us-east-1",
		Attributes: attrs, Strings: strs}
}

func TestGenericNonQuantumSelection(t *testing.T) {
	// The engine has nothing quantum-specific: select a GPU type by a capability
	// constraint then cost, exactly as one would a QPU. require:min_memory_gb=24
	// drops the 16GB option; min-cost picks the cheaper of the survivors.
	prov := fakeProvider{name: "f", bes: []types.Backend{
		be("gpu-a100", map[string]float64{"cost_usd": 3.20, "memory_gb": 80}, nil),
		be("gpu-l4", map[string]float64{"cost_usd": 0.80, "memory_gb": 24}, nil),
		be("gpu-t4", map[string]float64{"cost_usd": 0.35, "memory_gb": 16}, nil),
	}}
	eng := &Engine{Providers: []types.Provider{prov}}
	res, err := eng.Select(context.Background(), "require:min_memory_gb=24,min-cost", nil, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chosen != "gpu-l4" {
		t.Fatalf("chose %q, want gpu-l4 (t4 too small, l4 cheaper than a100)", res.Chosen)
	}
}

func TestMinCost(t *testing.T) {
	prov := fakeProvider{name: "f", bes: []types.Backend{
		be("sv1", map[string]float64{"cost_usd": 0.1, "queue_size": 0}, nil),
		be("rigetti", map[string]float64{"cost_usd": 7.5, "queue_size": 3}, nil),
		be("iqm", map[string]float64{"cost_usd": 7.5, "queue_size": 12}, nil),
	}}
	eng := &Engine{Providers: []types.Provider{prov}}
	res, err := eng.Select(context.Background(), "min-cost", nil, types.Request{Shots: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chosen != "sv1" {
		t.Fatalf("min-cost chose %q, want sv1", res.Chosen)
	}
}

func TestMinCostThenMinQueue(t *testing.T) {
	// rigetti and iqm tie on cost (7.5); among them, min-queue should pick the
	// lower queue (rigetti=3 < iqm=12). sv1 is cheaper but we exclude it via
	// candidates to test the tie-break path.
	prov := fakeProvider{name: "f", bes: []types.Backend{
		be("rigetti", map[string]float64{"cost_usd": 7.5, "queue_size": 3}, nil),
		be("iqm", map[string]float64{"cost_usd": 7.5, "queue_size": 12}, nil),
	}}
	eng := &Engine{Providers: []types.Provider{prov}}
	res, err := eng.Select(context.Background(), "min-cost,min-queue", nil, types.Request{Shots: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chosen != "rigetti" {
		t.Fatalf("min-cost,min-queue chose %q, want rigetti", res.Chosen)
	}
}

func TestSchedulerIntersection(t *testing.T) {
	// file knows about a backend the scheduler doesn't offer -> it's dropped.
	prov := fakeProvider{name: "f", bes: []types.Backend{
		be("sv1", map[string]float64{"cost_usd": 0.1}, nil),
		be("unavailable_vendor", map[string]float64{"cost_usd": 0.0}, nil),
	}}
	eng := &Engine{Providers: []types.Provider{prov}, SchedulerOffers: []string{"sv1", "dm1"}}
	res, err := eng.Select(context.Background(), "min-cost", nil, types.Request{Shots: 1000})
	if err != nil {
		t.Fatal(err)
	}
	// unavailable_vendor is cheaper (0.0) but not offered, so sv1 wins.
	if res.Chosen != "sv1" {
		t.Fatalf("chose %q, want sv1 (unavailable_vendor should be dropped)", res.Chosen)
	}
	if _, ok := res.Dropped["unavailable_vendor"]; !ok {
		t.Fatalf("expected unavailable_vendor in dropped, got %v", res.Dropped)
	}
	// dm1 is offered but unpriced -> noted as dropped too.
	if _, ok := res.Dropped["dm1"]; !ok {
		t.Fatalf("expected dm1 (offered, unpriced) in dropped, got %v", res.Dropped)
	}
}

func TestOnlineOnlyFilter(t *testing.T) {
	prov := fakeProvider{name: "f", bes: []types.Backend{
		be("a", map[string]float64{"cost_usd": 1}, map[string]string{"status": "OFFLINE"}),
		be("b", map[string]float64{"cost_usd": 2}, map[string]string{"status": "ONLINE"}),
	}}
	eng := &Engine{Providers: []types.Provider{prov}}
	res, err := eng.Select(context.Background(), "online-only,min-cost", nil, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chosen != "b" {
		t.Fatalf("chose %q, want b (a is OFFLINE)", res.Chosen)
	}
}

func TestRequireConstraint(t *testing.T) {
	prov := fakeProvider{name: "f", bes: []types.Backend{
		be("small", map[string]float64{"cost_usd": 1, "qubits": 5}, nil),
		be("big", map[string]float64{"cost_usd": 9, "qubits": 100}, nil),
	}}
	eng := &Engine{Providers: []types.Provider{prov}}
	res, err := eng.Select(context.Background(), "require:min_qubits=10,min-cost", nil, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chosen != "big" {
		t.Fatalf("chose %q, want big (small has too few qubits)", res.Chosen)
	}
}

func TestMergeAttributesAcrossProviders(t *testing.T) {
	// one provider supplies cost, another supplies queue; pipeline uses both.
	costProv := fakeProvider{name: "cost", bes: []types.Backend{
		be("x", map[string]float64{"cost_usd": 7.5}, nil),
		be("y", map[string]float64{"cost_usd": 7.5}, nil),
	}}
	queueProv := fakeProvider{name: "queue", bes: []types.Backend{
		{Name: "x", Attributes: map[string]float64{"queue_size": 50}},
		{Name: "y", Attributes: map[string]float64{"queue_size": 2}},
	}}
	eng := &Engine{Providers: []types.Provider{costProv, queueProv}}
	res, err := eng.Select(context.Background(), "min-cost,min-queue", nil, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chosen != "y" {
		t.Fatalf("chose %q, want y (tie on cost, y has lower queue)", res.Chosen)
	}
}
