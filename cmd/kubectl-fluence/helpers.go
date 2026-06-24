package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/converged-computing/kubectl-fluence/pkg/types"
)

// parseArgs pulls out the flags this plugin owns; everything else is forwarded
// to kubectl as passthrough.
func parseArgs(args []string) (options, error) {
	o := options{namespace: "kube-system"}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-f" || a == "--filename":
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s requires a value", a)
			}
			o.files = append(o.files, args[i+1])
			i += 2
		case strings.HasPrefix(a, "-f="):
			o.files = append(o.files, strings.TrimPrefix(a, "-f="))
			i++
		case strings.HasPrefix(a, "--filename="):
			o.files = append(o.files, strings.TrimPrefix(a, "--filename="))
			i++
		case a == "--attributes":
			o.attrFile = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--attributes="):
			o.attrFile = strings.TrimPrefix(a, "--attributes=")
			i++
		case a == "--fluence-ns":
			o.namespace = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--fluence-ns="):
			o.namespace = strings.TrimPrefix(a, "--fluence-ns=")
			i++
		case a == "--cost-tol":
			v, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil {
				return o, fmt.Errorf("--cost-tol: %w", err)
			}
			o.costTol = v
			i += 2
		case a == "--force-select":
			o.forceSelect = true
			i++
		case a == "--dry-run":
			o.dryRun = true
			// also forward to kubectl so its dry-run semantics apply on apply
			o.passthrough = append(o.passthrough, "--dry-run=client")
			i++
		default:
			// forward everything else to kubectl (e.g. -n, --context, -o)
			o.passthrough = append(o.passthrough, a)
			i++
		}
	}
	return o, nil
}

// kubectlContextArgs extracts only the context/namespace-ish flags from
// passthrough so the ConfigMap read happens in the same context as the apply.
func kubectlContextArgs(passthrough []string) []string {
	var out []string
	for i := 0; i < len(passthrough); i++ {
		a := passthrough[i]
		if a == "--context" || a == "--kubeconfig" {
			if i+1 < len(passthrough) {
				out = append(out, a, passthrough[i+1])
				i++
			}
		} else if strings.HasPrefix(a, "--context=") || strings.HasPrefix(a, "--kubeconfig=") {
			out = append(out, a)
		}
	}
	return out
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// splitYAMLDocs splits a multi-doc YAML stream on `---` separators.
func splitYAMLDocs(raw []byte) []string {
	text := string(raw)
	parts := strings.Split(text, "\n---")
	var out []string
	for _, p := range parts {
		p = strings.TrimPrefix(p, "---")
		out = append(out, p)
	}
	return out
}

// metadata accessors -------------------------------------------------------

func metadata(obj map[string]interface{}) map[string]interface{} {
	m, _ := obj["metadata"].(map[string]interface{})
	return m
}

func name(obj map[string]interface{}) string {
	if m := metadata(obj); m != nil {
		if n, ok := m["name"].(string); ok {
			kind, _ := obj["kind"].(string)
			return fmt.Sprintf("%s/%s", kind, n)
		}
	}
	k, _ := obj["kind"].(string)
	return k
}

func annotations(obj map[string]interface{}) map[string]string {
	out := map[string]string{}
	if m := metadata(obj); m != nil {
		if a, ok := m["annotations"].(map[string]interface{}); ok {
			for k, v := range a {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
	}
	return out
}

func labels(obj map[string]interface{}) map[string]string {
	out := map[string]string{}
	if m := metadata(obj); m != nil {
		if a, ok := m["labels"].(map[string]interface{}); ok {
			for k, v := range a {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
	}
	return out
}

func setAnnotation(obj map[string]interface{}, key, val string) {
	m, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		m = map[string]interface{}{}
		obj["metadata"] = m
	}
	a, ok := m["annotations"].(map[string]interface{})
	if !ok {
		a = map[string]interface{}{}
		m["annotations"] = a
	}
	a[key] = val
}

// shotsFromAnnotationsOrSpec resolves the request shot count for cost math:
// prefer the explicit select-shots annotation, else look for an N_SHOTS env in
// the pod spec, else default to 1000 (Braket QPU default).
func shotsFromAnnotationsOrSpec(anns map[string]string, obj map[string]interface{}) int {
	if s, ok := anns[types.AnnSelectShots]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n
		}
	}
	if n := findEnvInt(obj, "N_SHOTS"); n > 0 {
		return n
	}
	return 1000
}

// findEnvInt walks spec.containers[].env and spec.template.spec.containers[].env
// for an integer env var.
func findEnvInt(obj map[string]interface{}, key string) int {
	spec, _ := obj["spec"].(map[string]interface{})
	if spec == nil {
		return 0
	}
	if n := envFromSpec(spec, key); n > 0 {
		return n
	}
	if tmpl, ok := spec["template"].(map[string]interface{}); ok {
		if ts, ok := tmpl["spec"].(map[string]interface{}); ok {
			if n := envFromSpec(ts, key); n > 0 {
				return n
			}
		}
	}
	return 0
}

func envFromSpec(spec map[string]interface{}, key string) int {
	conts, _ := spec["containers"].([]interface{})
	for _, c := range conts {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		envs, _ := cm["env"].([]interface{})
		for _, e := range envs {
			em, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			if n, _ := em["name"].(string); n == key {
				if v, ok := em["value"].(string); ok {
					if iv, err := strconv.Atoi(v); err == nil {
						return iv
					}
				}
			}
		}
	}
	return 0
}

func parseConstraints(s string) map[string]float64 {
	out := map[string]float64{}
	for _, kv := range splitCSV(s) {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
				out[strings.TrimSpace(parts[0])] = v
			}
		}
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// applyWithKubectl pipes the mutated manifest to `kubectl <verb> -f -`.
func applyWithKubectl(verb, manifest string, passthrough []string) error {
	args := append([]string{verb, "-f", "-"}, passthrough...)
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
