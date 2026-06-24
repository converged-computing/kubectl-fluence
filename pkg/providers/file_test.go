package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

// a made-up test attribute file with one simulator and two QPUs.
const testAttrs = `
version: 1
backends:
  - name: testsim
    provider: testq
    device_arn: arn:test:::sim/testsim
    region: us-test-1
    cost_per_minute: 0.10
    qubits: 30
  - name: testqpu_cheap
    provider: testq
    device_arn: arn:test:::qpu/cheap
    region: us-test-1
    cost_per_task: 0.30
    cost_per_shot: 0.001
    qubits: 16
  - name: testqpu_dear
    provider: testq
    device_arn: arn:test:::qpu/dear
    region: us-test-2
    cost_per_task: 0.30
    cost_per_shot: 0.005
    qubits: 64
    strings:
      gate_set: native
`

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "attrs.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadAll(t *testing.T, body string, req types.Request) map[string]types.Backend {
	t.Helper()
	fp, err := NewFileProvider(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	cands, err := fp.Candidates(context.Background(), nil, req)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]types.Backend{}
	for _, c := range cands {
		m[c.Name] = c
	}
	return m
}

func TestQPUCostFromComponents(t *testing.T) {
	// cost_usd = per_task + shots*per_shot. 1000 shots:
	//   cheap = 0.30 + 1000*0.001 = 1.30
	//   dear  = 0.30 + 1000*0.005 = 5.30
	m := loadAll(t, testAttrs, types.Request{Shots: 1000})
	if got := m["testqpu_cheap"].Attributes["cost_usd"]; got != 1.30 {
		t.Fatalf("cheap cost_usd = %v, want 1.30", got)
	}
	if got := m["testqpu_dear"].Attributes["cost_usd"]; got != 5.30 {
		t.Fatalf("dear cost_usd = %v, want 5.30", got)
	}
}

func TestCostScalesWithShots(t *testing.T) {
	// doubling shots changes only the per-shot term.
	m1 := loadAll(t, testAttrs, types.Request{Shots: 1000})
	m2 := loadAll(t, testAttrs, types.Request{Shots: 2000})
	c1 := m1["testqpu_cheap"].Attributes["cost_usd"] // 1.30
	c2 := m2["testqpu_cheap"].Attributes["cost_usd"] // 0.30 + 2.0 = 2.30
	if c2 != 2.30 {
		t.Fatalf("2000-shot cheap cost = %v, want 2.30", c2)
	}
	if c2 <= c1 {
		t.Fatal("cost should increase with shots")
	}
}

func TestSimulatorCheaperThanQPU(t *testing.T) {
	// simulator gets a small nominal cost so it ranks below QPUs.
	m := loadAll(t, testAttrs, types.Request{Shots: 1000})
	sim := m["testsim"].Attributes["cost_usd"]
	qpu := m["testqpu_cheap"].Attributes["cost_usd"]
	if sim >= qpu {
		t.Fatalf("sim cost %v should be < qpu cost %v", sim, qpu)
	}
}

func TestQubitsAndStringsParsed(t *testing.T) {
	m := loadAll(t, testAttrs, types.Request{Shots: 1000})
	if m["testqpu_dear"].Attributes["qubits"] != 64 {
		t.Fatalf("qubits not parsed: %v", m["testqpu_dear"].Attributes)
	}
	if gs := m["testqpu_dear"].Strings["gate_set"]; gs != "native" {
		t.Fatalf("string attr not parsed: %q", gs)
	}
}

func TestOnlyFilter(t *testing.T) {
	fp, err := NewFileProvider(writeTemp(t, testAttrs))
	if err != nil {
		t.Fatal(err)
	}
	cands, err := fp.Candidates(context.Background(), []string{"testqpu_cheap"}, types.Request{Shots: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Name != "testqpu_cheap" {
		t.Fatalf("only-filter returned %d backends, want just testqpu_cheap", len(cands))
	}
}

func TestEmptyFileErrors(t *testing.T) {
	if _, err := NewFileProvider(writeTemp(t, "version: 1\nbackends: []\n")); err == nil {
		t.Fatal("expected error for file with no backends")
	}
}

func TestMissingFileErrors(t *testing.T) {
	if _, err := NewFileProvider("/nonexistent/attrs.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestJSONAlsoParses(t *testing.T) {
	// sigs.k8s.io/yaml accepts JSON; confirm a JSON attribute file works.
	js := `{"version":1,"backends":[{"name":"j","cost_per_task":0.3,"cost_per_shot":0.002}]}`
	m := loadAll(t, js, types.Request{Shots: 1000})
	if got := m["j"].Attributes["cost_usd"]; got != 2.30 {
		t.Fatalf("json parse cost = %v, want 2.30", got)
	}
}
