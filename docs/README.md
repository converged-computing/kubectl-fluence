# Design: `kubectl-fluence` — client-side backend-selection plugin

Repo: `https://github.com/converged-computing/kubectl-fluence`

## Purpose

A `kubectl` plugin that wraps `apply`/`create`, and — when the workload targets
Fluence and declares a backend-selection policy — resolves *which* quantum
backend the job should use **client-side, in the user's credentialed context**,
then stamps the choice as an annotation before the manifest reaches the cluster.
Fluence in the cluster honors the pinned backend and does the actual allocation,
gating, and arbitration.

Why client-side: the inputs to some selection policies (live queue depth, the
user's account-specific pricing/credits, device entitlements) require the user's
cloud credentials. Those must never enter the central scheduler. So the decision
that needs credentials runs where the credentials already are — the user's
`kubectl` context — and only the *result* (an annotation) crosses into the
cluster. Static/public inputs (list price, capability) could also be resolved
in-cluster later; this plugin owns the credentialed path.

## Invocation: wrapping kubectl

The plugin installs as `kubectl-fluence` (krew-discoverable as `kubectl fluence
...`). It wraps the mutating verbs:

```
kubectl fluence apply  -f gang.yaml            # like kubectl apply -f, with selection
kubectl fluence create -f gang.yaml
kubectl fluence apply  -f gang.yaml --dry-run  # print the mutated manifest, do not apply
kubectl fluence select -f gang.yaml            # ONLY resolve + print choice, never apply
```

Behavior of `apply`/`create`:
1. Read the manifest(s) from `-f` (file, dir, or stdin), exactly as kubectl does.
2. For each object, decide whether selection applies (see "Trigger" below).
3. If it applies, run the selection pipeline, mutate the object's annotations.
4. Hand the (possibly mutated) manifest to real `kubectl apply/create` (shell out
   or client-go), preserving all passthrough flags (`-n`, `--context`,
   `--dry-run`, `-o`, etc.). Unmutated objects pass through untouched.

The plugin is *additive and safe*: anything it doesn't recognize is passed
straight through, so it is a drop-in replacement for `kubectl apply`.

## Trigger: when does selection run?

Two gates, both must hold:

1. **Targets Fluence.** The object schedules with Fluence — detected by
   `spec.schedulerName: fluence` (pods/templates) or the Fluence group label
   `fluence.flux-framework.org/group` (PodGroups / labeled pods). If not Fluence,
   pass through.
2. **Declares a selection policy.** The object carries a selection-policy
   annotation (below). No policy annotation → nothing to resolve → pass through.

This means a plain Fluence gang with a hard-pinned device is untouched; selection
only runs when the user explicitly asks for it.

## The annotation contract (the interface)

The user expresses *intent* with annotations; the plugin writes the *result* into
a separate annotation that Fluence reads. Keeping request and result separate
makes the operation idempotent and auditable.

Request (user writes these):
```
fluence.flux-framework.org/select-backend: "braket"            # provider/registry to query
fluence.flux-framework.org/select-policy:  "min-cost"          # policy name (see registry)
fluence.flux-framework.org/select-candidates: "sv1,dm1,rigetti_cepheus,iqm_garnet"  # optional: restrict the pool
fluence.flux-framework.org/select-constraints: "min_qubits=10" # optional: filter before ranking
fluence.flux-framework.org/select-shots: "1000"                # request size, for cost math (else read from spec/env)
```

Composability (your "lowest cost then order by queue depth" requirement) is a
*pipeline* of policies, comma-separated, applied left to right as filter→rank
stages:
```
fluence.flux-framework.org/select-policy: "min-cost,min-queue"
# meaning: compute the cheapest set (ties / within-tolerance group), then break
# ties / reorder by queue depth. See "Policy pipeline semantics".
```

Result (plugin writes this; Fluence reads it):
```
fluence.flux-framework.org/backend: "rigetti_cepheus"          # the chosen device — the existing device-pin annotation
fluence.flux-framework.org/select-result: >                    # audit trail (JSON): what was considered and why
  {"policy":"min-cost,min-queue","chosen":"rigetti_cepheus",
   "candidates":[{"name":"rigetti_cepheus","cost_usd":7.5,"queue":3},
                 {"name":"iqm_garnet","cost_usd":7.5,"queue":12}],
   "resolved_at":"2026-06-23T..."}
```

`fluence.flux-framework.org/backend` is the EXISTING device-pin annotation Fluence
already honors, so the cluster side needs no change to consume the result. The
`select-result` annotation is purely informational (reproducibility/debugging).

Precedence: if the user has ALREADY hard-set `.../backend` AND a `select-policy`,
the plugin refuses by default (ambiguous) and requires `--force-select` to
override, so an explicit pin is never silently replaced.

## Architecture: retrieval separated from operation

Two orthogonal abstractions, per your requirement that "retrieval of information
is separate from the operation":

### A. Providers (retrieval) — "where do attributes come from?"

A `Provider` knows how to produce a set of candidate backends, each annotated with
attributes (cost, queue depth, capability, status). Providers are the only thing
that touches credentials or external APIs.

```go
type Backend struct {
    Name       string                 // "rigetti_cepheus"
    DeviceARN  string
    Region     string
    Attributes map[string]float64     // "cost_usd", "queue_size", "qubits", ...
    Strings    map[string]string      // "status", "provider", "gate_set", ...
}

type Provider interface {
    Name() string                                  // "braket"
    // Candidates returns the backends this provider knows about, optionally
    // restricted to `only` (names) and the request context (shots, etc.).
    Candidates(ctx context.Context, only []string, req Request) ([]Backend, error)
}
```

Concrete providers for Braket:

- **`braket-graph`** (default attribute source): reads the candidate set and their
  STATIC attributes (cost table, qubits, gate set, region) from the Fluence
  resource graph — specifically the `fluence-resources` **ConfigMap in the
  cluster**, NOT a local file. The user never needs the YAML; the plugin does
  `kubectl get configmap -n kube-system fluence-resources -o ...` in their
  context. (Requires read RBAC on that ConfigMap, which is reasonable.)
- **`braket-live`** (credentialed, dynamic): uses the user's AWS creds + boto3 /
  AWS SDK to call `GetDevice` → `deviceQueueInfo` for live **queue depth** and
  `deviceStatus` (online/offline). This is the per-user credentialed retrieval.
- **`braket-pricing`** (optional, credentialed-ish): refreshes per-shot/per-task
  prices from the **AWS Price List API** so cost isn't stale relative to the
  graph's configured table. Falls back to the graph's static prices if
  unavailable. (Braket has no per-device price API; pricing is published +
  Price List API only — so the graph table is the primary source and this is a
  refresh.)

Providers COMPOSE: the selection pipeline can merge attributes from several
providers into each Backend (e.g. cost from `braket-graph`, queue from
`braket-live`), so "lowest cost then order by queue depth" pulls cost from one
retriever and queue from another. Retrieval is unioned before policies run.

### B. Policies (operation) — "how do we choose, given attributes?"

A `Policy` is a pure function over already-retrieved Backends. It never does I/O.
This is the standard interface all policies share; each concrete policy (min, max,
filter) implements it.

```go
type Request struct {
    Shots      int
    NWorkers   int
    Constraints map[string]float64   // parsed from select-constraints
}

type Policy interface {
    Name() string                                       // "min-cost"
    // Apply filters/ranks the input and returns an ORDERED result (best first).
    // It must be pure: depends only on the Backends' attributes + req.
    Apply(in []Backend, req Request) ([]Backend, error)
    // RequiredAttributes lets the engine know which attributes to retrieve
    // (so we only call the providers we need). e.g. ["cost_usd"].
    RequiredAttributes() []string
}
```

Concrete policies (each a small, single-purpose implementation):

- `min-cost` / `max-cost` — rank by `cost_usd` (computed per request: see below).
- `min-queue` / `max-queue` — rank by `queue_size`.
- `min-qubits` / `max-qubits`, or `require:min_qubits=N` — capability filter.
- `online-only` — filter `status == ONLINE` (a pure filter, no ranking).
- `prefer:<name>` — pin-with-fallback: choose <name> if it satisfies, else defer
  to the next policy.

A **generic comparator core** implements min/max once over a named attribute, so
`min-cost` and `min-queue` are the same code parameterized by attribute name and
direction — only the attribute differs. New "rank by X" policies are one line.

### Policy pipeline semantics (composition)

`select-policy: "min-cost,min-queue"` runs as a left-to-right pipeline:

1. Start with all candidates (after constraints/`online-only` filters).
2. `min-cost` ranks by cost and keeps the **best-equivalent group** — those
   within a tolerance band of the minimum (default exact ties; `--cost-tol=0.1`
   widens it). This is the "lowest cost set" you described.
3. `min-queue` reorders that surviving set by queue depth.
4. The head of the final ordered list is chosen → written to `.../backend`.

Filters (`online-only`, `require:`) reduce the set; rankers (`min-*`) order it and
optionally narrow to a best-equivalent band. The engine validates the pipeline is
non-empty at the end (else error: "no backend satisfies the policy").

## Cost model (what "cost" means)

Cost is per-request, computed from attributes + request size:

- **QPU:** `cost_usd = per_task + shots * per_shot`  (per-task is currently uniform
  across Braket QPUs; per-shot is device-specific). Attributes on the graph node:
  `cost_per_task`, `cost_per_shot`.
- **Simulator:** `cost_usd = per_minute * est_minutes` where est_minutes is hard
  to know a priori; for selection we can use a configured `cost_per_minute` and a
  coarse duration estimate, or treat simulators as ~free relative to QPUs (the
  realistic case). Attribute: `cost_per_minute`. Note SV1/DM1 cost is shot-
  independent; TN1 and QPUs scale with shots.

So the unit is **estimated USD for THIS request** (given its shot count), which is
the decision-relevant quantity — exactly "cost for the work being submitted."

## Graph attribute additions (prerequisite, cluster side)

`fluence-resources.yaml` QPU/simulator vertices gain optional numeric attributes:
```yaml
- name: rigetti_cepheus
  attributes:
    cost_per_task: 0.30
    cost_per_shot: 0.00090
    qubits: 108
    region: us-west-1
```
The plugin reads these from the in-cluster ConfigMap. (Verify the Fluence graph
builder ingests arbitrary numeric attributes — flagged as the open prerequisite.)

## Multi-provider / extensibility

`select-backend` names the provider registry to use; `braket` is first. The
`Provider`/`Policy` split means a future `qiskit-ibm` or `azure-quantum` provider
implements `Candidates()` against its own API, and ALL existing policies
(`min-cost`, `min-queue`, ...) work unchanged because they operate only on the
abstract `Backend.Attributes`. New providers add retrieval; new policies add
operations; the two never entangle.

## Open prerequisites / risks

- **Graph numeric attributes**: confirm the Fluence resource-graph builder and
  `fluence-resources` ConfigMap can carry/read arbitrary per-device numeric
  attributes. Gate for `braket-graph`.
- **ConfigMap RBAC**: the user's context needs read on
  `kube-system/fluence-resources`. Confirm acceptable, or expose the table via a
  user-namespace ConfigMap / CRD instead.
- **Snapshot, not dispatch-time**: queue depth is read at submit time; the job may
  sit gated before it runs, so the queue value can be stale by dispatch. This is a
  client-side hint, not a scheduling-time decision — state it plainly.
- **Pricing freshness**: no per-device price API; graph table is primary, Price
  List API is a best-effort refresh. Document that configured prices may lag.
- **`select-result` size**: keep the audit JSON small (annotations have a size
  limit); cap the candidate list or truncate.

## What lives where (summary)

| Concern | Where | Credentials? |
|---|---|---|
| List candidates + static attrs (cost table, qubits) | `braket-graph` provider, from ConfigMap | no |
| Live queue depth, device status | `braket-live` provider, user AWS creds | YES (client-side) |
| Price refresh | `braket-pricing` provider, Price List API | maybe |
| Choosing (min/max/filter/pipeline) | Policies (pure functions) | no |
| Pinning the result | `.../backend` annotation | n/a |
| Allocation, gating, arbitration | Fluence in-cluster (unchanged) | no |
