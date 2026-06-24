// Command kubectl-fluence is a kubectl plugin (invoked as `kubectl fluence ...`)
// that resolves quantum backend selection CLIENT-SIDE before submitting work to
// a Fluence-scheduled cluster.
//
// It wraps the mutating verbs:
//
//	kubectl fluence apply  -f gang.yaml [kubectl flags...]
//	kubectl fluence create -f gang.yaml [kubectl flags...]
//	kubectl fluence select -f gang.yaml      # resolve + print only, never apply
//
// For each object that (a) targets Fluence and (b) carries a select-policy
// annotation, it runs the selection engine (cost from a user attribute file,
// queue depth live from Braket), intersects the priced backends with what the
// scheduler offers, picks a winner, and stamps the fluence backend annotation —
// the same device-pin Fluence already honors. Everything else passes through
// untouched, so this is a safe drop-in for kubectl apply.
//
// Credentials (AWS) are only used by the live provider and never leave the
// user's machine; only the chosen backend name enters the cluster.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/converged-computing/kubectl-fluence/pkg/kube"
	"github.com/converged-computing/kubectl-fluence/pkg/providers"
	"github.com/converged-computing/kubectl-fluence/pkg/selection"
	"github.com/converged-computing/kubectl-fluence/pkg/types"
	"sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	verb := os.Args[1]
	switch verb {
	case "apply", "create", "select":
		if err := run(verb, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	case "version":
		fmt.Println("kubectl-fluence", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", verb)
		usage()
		os.Exit(2)
	}
}

var version = "0.1.0"

// options parsed out of args; everything else is passed through to kubectl.
type options struct {
	files       []string
	attrFile    string
	namespace   string // fluence ConfigMap namespace (default kube-system)
	costTol     float64
	forceSelect bool
	dryRun      bool
	confirm     bool     // prompt for y/n approval before applying
	passthrough []string // remaining args forwarded to kubectl
}

func run(verb string, args []string) error {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}
	if len(opts.files) == 0 {
		return fmt.Errorf("no -f/--filename given")
	}

	// Read scheduler offerings once (best-effort).
	offers, offErr := kube.SchedulerOfferings(opts.namespace, kubectlContextArgs(opts.passthrough))
	if offErr != nil {
		fmt.Fprintf(os.Stderr, "note: could not read scheduler offerings (%v); "+
			"selecting without intersection\n", offErr)
	}

	// Load + process each file's documents.
	var mutatedDocs []string
	for _, f := range opts.files {
		raw, err := readFileOrStdin(f)
		if err != nil {
			return err
		}
		for _, doc := range splitYAMLDocs(raw) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			out, changed, err := processDoc(doc, opts, offers)
			if err != nil {
				return err
			}
			if changed && verb == "select" {
				// select verb: just report, don't apply
			}
			mutatedDocs = append(mutatedDocs, out)
		}
	}
	combined := strings.Join(mutatedDocs, "\n---\n")

	if verb == "select" || opts.dryRun {
		fmt.Println(combined)
		if verb == "select" {
			return nil
		}
	}

	// Optional interactive confirmation before applying. The selection results
	// were already printed to stderr (one "selected backend ..." line per
	// resolved object); --confirm lets the user inspect and approve, edit, or
	// cancel. Off by default so automated/experiment runs proceed unattended.
	if opts.confirm && !opts.dryRun {
		fmt.Fprint(os.Stderr, "\nApply with the selections above? [y]es / [n]o (cancel): ")
		var resp string
		fmt.Fscanln(os.Stdin, &resp)
		resp = strings.ToLower(strings.TrimSpace(resp))
		if resp != "y" && resp != "yes" {
			fmt.Fprintln(os.Stderr, "cancelled; nothing applied")
			return nil
		}
	}

	// Pass the mutated manifest to real kubectl via stdin.
	return applyWithKubectl(verb, combined, opts.passthrough)
}

// processDoc inspects one YAML document; if it triggers selection, mutates its
// annotations. Returns (possibly-mutated doc, changed?, error).
func processDoc(doc string, opts options, offers []string) (string, bool, error) {
	var obj map[string]interface{}
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		// not parseable as a k8s object; pass through unchanged
		return doc, false, nil
	}
	if !triggersSelection(obj) {
		return doc, false, nil
	}
	anns := annotations(obj)
	policy := anns[types.AnnSelectPolicy]

	// Honor an explicit pre-existing pin unless --force-select.
	if existing, ok := anns[types.AnnRequireBackend]; ok && existing != "" && !opts.forceSelect {
		fmt.Fprintf(os.Stderr, "note: %s already pinned to %q; skipping selection "+
			"(use --force-select to override)\n", name(obj), existing)
		return doc, false, nil
	}

	req := types.Request{
		Shots:       shotsFromAnnotationsOrSpec(anns, obj),
		Constraints: parseConstraints(anns[types.AnnSelectConstraints]),
	}
	only := splitCSV(anns[types.AnnSelectCandidates])

	// Build providers: file (static cost/capability) + braket-live (queue/status).
	var provs []types.Provider
	if opts.attrFile == "" {
		return doc, false, fmt.Errorf("%s requests selection but no --attributes file was given", name(obj))
	}
	fp, err := providers.NewFileProvider(opts.attrFile)
	if err != nil {
		return doc, false, err
	}
	provs = append(provs, fp)

	// The live provider needs ARNs; get them from the file provider's candidates.
	staticCands, _ := fp.Candidates(context.Background(), only, req)
	if needsLive(policy) {
		provs = append(provs, providers.NewBraketLiveProvider(staticCands))
	}

	eng := &selection.Engine{Providers: provs, SchedulerOffers: offers, CostTol: opts.costTol}
	res, err := eng.Select(context.Background(), policy, only, req)
	if err != nil {
		return doc, false, fmt.Errorf("%s: selection failed: %w", name(obj), err)
	}

	// Stamp the result: the device-pin annotation + an audit annotation.
	setAnnotation(obj, types.AnnRequireBackend, res.Chosen)
	auditJSON, _ := json.Marshal(res)
	setAnnotation(obj, types.AnnSelectResult, string(auditJSON))

	fmt.Fprintf(os.Stderr, "selected backend %q for %s (policy %q)\n",
		res.Chosen, name(obj), policy)
	for n, why := range res.Dropped {
		fmt.Fprintf(os.Stderr, "  dropped %s: %s\n", n, why)
	}

	mut, err := yaml.Marshal(obj)
	if err != nil {
		return doc, false, err
	}
	return string(mut), true, nil
}

// triggersSelection: targets Fluence AND declares a select-policy.
func triggersSelection(obj map[string]interface{}) bool {
	anns := annotations(obj)
	if anns[types.AnnSelectPolicy] == "" {
		return false
	}
	return targetsFluence(obj)
}

func targetsFluence(obj map[string]interface{}) bool {
	// spec.schedulerName == fluence (pods/templates), or the group label present.
	if spec, ok := obj["spec"].(map[string]interface{}); ok {
		if sn, ok := spec["schedulerName"].(string); ok && sn == types.SchedulerName {
			return true
		}
		// PodTemplate nested under spec.template.spec
		if tmpl, ok := spec["template"].(map[string]interface{}); ok {
			if ts, ok := tmpl["spec"].(map[string]interface{}); ok {
				if sn, ok := ts["schedulerName"].(string); ok && sn == types.SchedulerName {
					return true
				}
			}
		}
	}
	if lbls := labels(obj); lbls != nil {
		if _, ok := lbls[types.LabelGroup]; ok {
			return true
		}
	}
	return false
}

func needsLive(policy string) bool {
	return strings.Contains(policy, "queue") || strings.Contains(policy, "online")
}

func usage() {
	fmt.Fprint(os.Stderr, `kubectl-fluence — client-side quantum backend selection for Fluence

USAGE:
  kubectl fluence apply  -f FILE [--attributes attrs.yaml] [kubectl flags]
  kubectl fluence create -f FILE [--attributes attrs.yaml] [kubectl flags]
  kubectl fluence select -f FILE [--attributes attrs.yaml]   # resolve + print only

FLAGS:
  -f, --filename       manifest file/dir/'-' (repeatable)
      --attributes     YAML/JSON file of backend attributes (cost, qubits, ...)
      --fluence-ns     namespace of the fluence-resources ConfigMap (default kube-system)
      --cost-tol       tolerance band for the cheapest-equivalent set (default 0)
      --force-select   override an existing backend pin
      --confirm        show selections and prompt for y/n approval before applying
      --dry-run        print the mutated manifest (still applies unless 'select')

Objects that target Fluence and carry the annotation
  fluence.flux-framework.org/select-policy: "<pipeline>"
are resolved; the chosen device is written to
  fluence.flux-framework.org/backend
which Fluence honors. All other objects pass through unchanged.
`)
}
