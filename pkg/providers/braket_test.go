package providers

import (
	"context"
	"fmt"
	"testing"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

func TestParseQueueSize(t *testing.T) {
	cases := map[string]float64{
		"0":     0,
		"7":     7,
		">4000": 4001,
		"":      1e9,
		"weird": 1e9,
	}
	for in, want := range cases {
		if got := parseQueueSize(in); got != want {
			t.Errorf("parseQueueSize(%q) = %v, want %v", in, got, want)
		}
	}
}

// fakeRunner returns canned GetDevice JSON per ARN, simulating the AWS CLI.
func fakeRunner(byArn map[string]string) func(string, ...string) ([]byte, error) {
	return func(_ string, args ...string) ([]byte, error) {
		// find the --device-arn value in args
		var arn string
		for i, a := range args {
			if a == "--device-arn" && i+1 < len(args) {
				arn = args[i+1]
			}
		}
		body, ok := byArn[arn]
		if !ok {
			return nil, fmt.Errorf("no canned response for %s", arn)
		}
		return []byte(body), nil
	}
}

func TestBraketLiveQueueAndStatus(t *testing.T) {
	static := []types.Backend{
		{Name: "qpuA", DeviceARN: "arn:A", Region: "us-test-1", Provider: "braket"},
		{Name: "qpuB", DeviceARN: "arn:B", Region: "us-test-2", Provider: "braket"},
	}
	lp := NewBraketLiveProvider(static)
	lp.runner = fakeRunner(map[string]string{
		"arn:A": `{"deviceStatus":"ONLINE","deviceQueueInfo":[
			{"queue":"QUANTUM_TASKS_QUEUE","queueSize":"3","queuePriority":"Normal"},
			{"queue":"QUANTUM_TASKS_QUEUE","queueSize":"1","queuePriority":"Priority"}]}`,
		"arn:B": `{"deviceStatus":"OFFLINE","deviceQueueInfo":[
			{"queue":"QUANTUM_TASKS_QUEUE","queueSize":">4000","queuePriority":"Normal"}]}`,
	})
	cands, err := lp.Candidates(context.Background(), nil, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]types.Backend{}
	for _, c := range cands {
		m[c.Name] = c
	}
	// qpuA: normal queue 3, ONLINE
	if got := m["qpuA"].Attributes["queue_size"]; got != 3 {
		t.Fatalf("qpuA queue = %v, want 3 (normal, not priority)", got)
	}
	if m["qpuA"].Strings["status"] != "ONLINE" {
		t.Fatalf("qpuA status = %q", m["qpuA"].Strings["status"])
	}
	// qpuB: >4000 -> 4001, OFFLINE
	if got := m["qpuB"].Attributes["queue_size"]; got != 4001 {
		t.Fatalf("qpuB queue = %v, want 4001", got)
	}
	if m["qpuB"].Strings["status"] != "OFFLINE" {
		t.Fatalf("qpuB status = %q", m["qpuB"].Strings["status"])
	}
}

func TestBraketLiveQueryErrorIsSoft(t *testing.T) {
	// a device that fails to query should not fail the whole run; it gets
	// status UNKNOWN so online-only can drop it.
	static := []types.Backend{{Name: "q", DeviceARN: "arn:missing", Region: "r"}}
	lp := NewBraketLiveProvider(static)
	lp.runner = fakeRunner(map[string]string{}) // no canned response -> error
	cands, err := lp.Candidates(context.Background(), nil, types.Request{})
	if err != nil {
		t.Fatalf("query error should be soft, got hard error: %v", err)
	}
	if len(cands) != 1 || cands[0].Strings["status"] != "UNKNOWN" {
		t.Fatalf("expected one UNKNOWN-status backend, got %+v", cands)
	}
}

func TestBraketLiveSkipsBackendsWithoutARN(t *testing.T) {
	static := []types.Backend{{Name: "noarn"}} // no DeviceARN
	lp := NewBraketLiveProvider(static)
	cands, err := lp.Candidates(context.Background(), nil, types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("backend without ARN should be skipped, got %d", len(cands))
	}
}
