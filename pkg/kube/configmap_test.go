package kube

import (
	"sort"
	"testing"
)

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	a, b = sorted(a), sorted(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A flat offering list, like the stand-in ConfigMap CI creates.
func TestParseFlatList(t *testing.T) {
	body := "\n- name: gpu-a100\n- name: gpu-l4\n"
	names, err := parseDeviceNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(names, []string{"gpu-a100", "gpu-l4"}) {
		t.Fatalf("got %v", names)
	}
}

// A nested resource-graph-ish mapping.
func TestParseNestedGraph(t *testing.T) {
	body := `
graph:
  name: cluster0
  resources:
    - name: rack0
      with:
        - name: sv1
        - name: rigetti_cepheus
`
	names, err := parseDeviceNames(body)
	if err != nil {
		t.Fatal(err)
	}
	// collects all name values; intersection later filters non-devices.
	for _, want := range []string{"sv1", "rigetti_cepheus"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected %q in %v", want, names)
		}
	}
}

// JSON body (sigs.k8s.io/yaml accepts JSON).
func TestParseJSONList(t *testing.T) {
	body := `[{"name":"a"},{"name":"b"}]`
	names, err := parseDeviceNames(body)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(names, []string{"a", "b"}) {
		t.Fatalf("got %v", names)
	}
}

// Quoted names and `- ` prefix via the regex fallback path.
func TestParseQuotedAndDashed(t *testing.T) {
	// Force the regex fallback by giving content the structured walk yields
	// nothing useful from but the regex can read.
	body := "items:\n  - name: \"gpu-t4\"\n  - name: gpu-l4\n"
	names, err := parseDeviceNames(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gpu-t4", "gpu-l4"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected %q in %v", want, names)
		}
	}
}

func TestParseEmptyErrors(t *testing.T) {
	if _, err := parseDeviceNames("# just a comment\n"); err == nil {
		t.Fatal("expected error for body with no names")
	}
}
