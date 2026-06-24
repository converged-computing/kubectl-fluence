// Package providers implements the Provider interface from pkg/types. Providers
// are the only components that touch external data sources (files, the cluster
// ConfigMap, or credentialed cloud APIs).
//
// FileProvider reads STATIC backend attributes (cost components, qubit count,
// region, gate set) from a user-supplied metadata file. This is deliberately
// the primary cost source: a user may have pricing/capability metadata the
// cluster scheduler does not know (or cannot know), so cost selection must not
// depend on the resource graph. The selection engine intersects file-provided
// backends with the candidate set the scheduler actually offers.
package providers

import (
	"context"
	"fmt"
	"os"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
	"sigs.k8s.io/yaml"
)

// FileBackend is the on-disk schema for one backend's static attributes. Cost is
// expressed as components so the engine can compute per-request cost from the
// shot count rather than baking in a request size.
type FileBackend struct {
	Name        string             `json:"name"`
	DeviceARN   string             `json:"device_arn,omitempty"`
	Region      string             `json:"region,omitempty"`
	Provider    string             `json:"provider,omitempty"`
	CostPerTask *float64           `json:"cost_per_task,omitempty"`   // QPU per-task fee (USD)
	CostPerShot *float64           `json:"cost_per_shot,omitempty"`   // QPU per-shot fee (USD)
	CostPerMin  *float64           `json:"cost_per_minute,omitempty"` // simulator per-minute fee (USD)
	Qubits      *float64           `json:"qubits,omitempty"`
	Attributes  map[string]float64 `json:"attributes,omitempty"` // any extra numeric attrs
	Strings     map[string]string  `json:"strings,omitempty"`    // any extra string attrs (gate_set, ...)
}

// AttributeFile is the top-level metadata file schema.
//
//	version: 1
//	backends:
//	  - name: rigetti_cepheus
//	    provider: braket
//	    region: us-west-1
//	    cost_per_task: 0.30
//	    cost_per_shot: 0.00090
//	    qubits: 108
type AttributeFile struct {
	Version  int           `json:"version"`
	Backends []FileBackend `json:"backends"`
}

// FileProvider supplies static attributes from an AttributeFile (YAML or JSON).
type FileProvider struct {
	path     string
	backends []FileBackend
}

// NewFileProvider loads and parses the attribute file.
func NewFileProvider(path string) (*FileProvider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read attribute file %s: %w", path, err)
	}
	var af AttributeFile
	// yaml.Unmarshal (sigs.k8s.io/yaml) accepts both YAML and JSON.
	if err := yaml.Unmarshal(raw, &af); err != nil {
		return nil, fmt.Errorf("parse attribute file %s: %w", path, err)
	}
	if len(af.Backends) == 0 {
		return nil, fmt.Errorf("attribute file %s has no backends", path)
	}
	return &FileProvider{path: path, backends: af.Backends}, nil
}

func (p *FileProvider) Name() string { return "file" }

func (p *FileProvider) Candidates(_ context.Context, only []string, req types.Request) ([]types.Backend, error) {
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	var out []types.Backend
	for _, fb := range p.backends {
		if len(want) > 0 && !want[fb.Name] {
			continue
		}
		b := types.Backend{
			Name:       fb.Name,
			DeviceARN:  fb.DeviceARN,
			Region:     fb.Region,
			Provider:   fb.Provider,
			Attributes: map[string]float64{},
			Strings:    map[string]string{},
		}
		for k, v := range fb.Attributes {
			b.Attributes[k] = v
		}
		for k, v := range fb.Strings {
			b.Strings[k] = v
		}
		if fb.Qubits != nil {
			b.Attributes["qubits"] = *fb.Qubits
		}
		// Compute per-request cost from components + request shot count. QPU:
		// per_task + shots*per_shot. Simulator: per_minute is duration-dependent
		// and not known a priori, so we record it but cannot finalize cost; we
		// expose cost_per_minute as an attribute for policies that want it, and
		// compute cost_usd only when QPU components are present.
		if fb.CostPerTask != nil || fb.CostPerShot != nil {
			task := 0.0
			if fb.CostPerTask != nil {
				task = *fb.CostPerTask
			}
			shot := 0.0
			if fb.CostPerShot != nil {
				shot = *fb.CostPerShot
			}
			b.Attributes["cost_usd"] = task + float64(req.Shots)*shot
			b.Attributes["cost_per_task"] = task
			b.Attributes["cost_per_shot"] = shot
		}
		if fb.CostPerMin != nil {
			b.Attributes["cost_per_minute"] = *fb.CostPerMin
			// Simulators are effectively free relative to QPUs; if no QPU cost
			// components were given, record a nominal cost_usd of ~0 so cost
			// policies can still rank simulators below QPUs.
			if _, ok := b.Attributes["cost_usd"]; !ok {
				b.Attributes["cost_usd"] = *fb.CostPerMin * 0.05 // ~3s min billing, coarse
			}
		}
		out = append(out, b)
	}
	return out, nil
}
