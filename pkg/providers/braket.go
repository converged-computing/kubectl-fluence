// Package providers — Braket live provider.
//
// BraketLiveProvider retrieves DYNAMIC, credentialed attributes — queue depth
// and device status — by calling AWS Braket GetDevice in the user's context.
// It shells out to the AWS CLI rather than embedding the AWS SDK: the CLI is
// already how users authenticate to Braket, it inherits their profile/region
// config and credentials with no extra wiring, and it keeps this plugin's
// dependency surface small. Credentials therefore never leave the user's
// machine — only the resulting queue numbers are written into an annotation.
//
// This is the per-user, credentialed retrieval path. It only ADDS attributes
// (queue_size, status) to backends that already exist (typically from the file
// provider), keyed by device ARN, so it composes with static attribute sources.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

// BraketLiveProvider queries Braket GetDevice for live queue/status.
type BraketLiveProvider struct {
	// arnByName maps a backend name to its device ARN + region so we know what
	// to query. Populated from the static backends (file/graph) before use.
	arnByName map[string]arnRegion
	runner    func(region string, args ...string) ([]byte, error) // injectable for tests
}

type arnRegion struct {
	arn    string
	region string
}

// NewBraketLiveProvider builds a live provider from known static backends (it
// needs their ARNs/regions to query). Backends without an ARN are skipped.
func NewBraketLiveProvider(static []types.Backend) *BraketLiveProvider {
	m := map[string]arnRegion{}
	for _, b := range static {
		if b.DeviceARN != "" {
			m[b.Name] = arnRegion{arn: b.DeviceARN, region: b.Region}
		}
	}
	return &BraketLiveProvider{arnByName: m, runner: awsCLI}
}

func (p *BraketLiveProvider) Name() string { return "braket-live" }

// getDeviceResponse is the subset of Braket GetDevice we parse.
type getDeviceResponse struct {
	DeviceStatus    string `json:"deviceStatus"`
	DeviceQueueInfo []struct {
		Queue     string `json:"queue"`     // e.g. QUANTUM_TASKS_QUEUE
		QueueSize string `json:"queueSize"` // may be ">4000"
		Priority  string `json:"queuePriority"`
	} `json:"deviceQueueInfo"`
}

func (p *BraketLiveProvider) Candidates(_ context.Context, only []string, _ types.Request) ([]types.Backend, error) {
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	var out []types.Backend
	for name, ar := range p.arnByName {
		if len(want) > 0 && !want[name] {
			continue
		}
		b := types.Backend{Name: name, DeviceARN: ar.arn, Region: ar.region,
			Provider: "braket", Attributes: map[string]float64{}, Strings: map[string]string{}}
		raw, err := p.runner(ar.region, "braket", "get-device", "--device-arn", ar.arn,
			"--region", ar.region, "--output", "json")
		if err != nil {
			// A device we can't query (offline, no access) is recorded with a
			// status so online-only can drop it, but we don't fail the whole run.
			b.SetStr("status", "UNKNOWN")
			b.SetStr("query_error", strings.TrimSpace(err.Error()))
			out = append(out, b)
			continue
		}
		var resp getDeviceResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			b.SetStr("status", "UNKNOWN")
			out = append(out, b)
			continue
		}
		b.SetStr("status", resp.DeviceStatus)
		// Use the normal quantum-tasks queue depth as queue_size.
		for _, q := range resp.DeviceQueueInfo {
			if strings.Contains(strings.ToUpper(q.Queue), "QUANTUM_TASKS") &&
				!strings.EqualFold(q.Priority, "Priority") {
				b.SetAttr("queue_size", parseQueueSize(q.QueueSize))
			}
		}
		out = append(out, b)
	}
	return out, nil
}

// parseQueueSize turns Braket's queueSize string into a number. ">4000" becomes
// 4001 (a large sentinel so it ranks worst), unparseable becomes a large value.
func parseQueueSize(s string) float64 {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, ">") {
		if n, err := strconv.Atoi(strings.TrimPrefix(s, ">")); err == nil {
			return float64(n) + 1
		}
		return 1e9
	}
	if n, err := strconv.Atoi(s); err == nil {
		return float64(n)
	}
	return 1e9
}

// awsCLI runs the AWS CLI, returning stdout. Region is passed via --region in
// args already; we keep the param for clarity/testing.
func awsCLI(_ string, args ...string) ([]byte, error) {
	cmd := exec.Command("aws", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("aws %s: %s", strings.Join(args, " "),
				strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("aws %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
