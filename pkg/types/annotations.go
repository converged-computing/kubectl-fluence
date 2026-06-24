// Package types — manifest helpers (trigger detection + annotation contract).
//
// Kept in the types package so both the command and the selection layer can use
// the annotation keys without an import cycle.
package types

// Annotation keys: the contract between the user (intent), this plugin (result),
// and Fluence in-cluster (consumer of the pinned backend).
const (
	// Request annotations (user-authored intent):
	AnnSelectBackend     = "fluence.flux-framework.org/select-backend"     // provider registry, e.g. "braket"
	AnnSelectPolicy      = "fluence.flux-framework.org/select-policy"      // pipeline, e.g. "min-cost,min-queue"
	AnnSelectCandidates  = "fluence.flux-framework.org/select-candidates"  // optional CSV of backend names to restrict the pool
	AnnSelectConstraints = "fluence.flux-framework.org/select-constraints" // optional, e.g. "min_qubits=10"
	AnnSelectShots       = "fluence.flux-framework.org/select-shots"       // request size for cost math (else inferred)

	// Result annotations (plugin-authored):
	AnnBackend      = "fluence.flux-framework.org/backend"       // chosen device — the device-pin Fluence already honors
	AnnSelectResult = "fluence.flux-framework.org/select-result" // JSON audit trail

	// Detection:
	LabelGroup    = "fluence.flux-framework.org/group" // marks a Fluence gang member / PodGroup
	SchedulerName = "fluence"                          // spec.schedulerName value
)
