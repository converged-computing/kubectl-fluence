// Package kube reads cluster state the plugin needs, using the user's kubectl
// context (so no in-cluster credentials or kubeconfig wiring is required beyond
// what the user already has). Specifically it reads the set of backends the
// Fluence scheduler offers from the fluence-resources ConfigMap, so selection
// can intersect the user's priced backends with what is actually schedulable.
package kube

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// SchedulerOfferings returns the backend (device) names the Fluence scheduler
// exposes, by reading the fluence-resources ConfigMap. Returns (names, nil) on
// success; if the ConfigMap can't be read, returns (nil, err) and the caller
// decides whether to proceed without intersection.
//
// kubectlArgs lets callers pass through context/namespace flags (e.g.
// ["--context", "foo"]). The ConfigMap is expected in kube-system with key
// resources.yaml, matching the Fluence deployment.
func SchedulerOfferings(namespace string, kubectlArgs []string) ([]string, error) {
	if namespace == "" {
		namespace = "kube-system"
	}
	args := append([]string{"get", "configmap", "fluence-resources",
		"-n", namespace, "-o", "json"}, kubectlArgs...)
	out, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("kubectl get configmap fluence-resources: %s",
				strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl get configmap fluence-resources: %w", err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &cm); err != nil {
		return nil, fmt.Errorf("parse configmap json: %w", err)
	}
	body, ok := cm.Data["resources.yaml"]
	if !ok {
		// fall back to the first data key
		for _, v := range cm.Data {
			body = v
			break
		}
	}
	if body == "" {
		return nil, fmt.Errorf("fluence-resources ConfigMap has no resources.yaml data")
	}
	return parseDeviceNames(body)
}

// parseDeviceNames extracts device names from the resource-graph YAML. The graph
// lists devices as entries with a `name:` under the quantum resource hierarchy.
// We parse generically rather than binding to the full graph schema: collect
// every `name:` value that looks like a backend (sv1/tn1/dm1/<vendor>_<device>).
// This is intentionally permissive; the intersection later restricts to what
// the user also has attributes for.
func parseDeviceNames(body string) ([]string, error) {
	// Try structured parse first.
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(body), &doc); err == nil {
		if names := walkForNames(doc); len(names) > 0 {
			return dedupe(names), nil
		}
	}
	// Fallback: regex for `name: <token>` lines.
	re := regexp.MustCompile(`(?m)^\s*name:\s*([A-Za-z0-9_\-]+)\s*$`)
	var names []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no device names found in resource graph")
	}
	return dedupe(names), nil
}

// walkForNames recursively collects values of "name" keys whose sibling context
// suggests a quantum device. To stay schema-agnostic we collect all "name"
// values and let the intersection with the attribute file filter out non-devices
// (cluster/rack/node names won't have cost attributes, so they drop out).
func walkForNames(v interface{}) []string {
	var out []string
	switch t := v.(type) {
	case map[string]interface{}:
		if n, ok := t["name"].(string); ok {
			out = append(out, n)
		}
		for _, vv := range t {
			out = append(out, walkForNames(vv)...)
		}
	case []interface{}:
		for _, vv := range t {
			out = append(out, walkForNames(vv)...)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
