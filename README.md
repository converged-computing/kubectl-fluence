# kubectl-fluence

A `kubectl` plugin for **client-side backend selection** with the
[Fluence](https://github.com/converged-computing/fluence) scheduler.

When you submit work to a Fluence-scheduled cluster, you often want to choose
*which* backend resource it should target by some policy — the cheapest option
that meets a constraint, the one with the shortest queue, the one with enough
memory, and so on. Some of the inputs to those decisions (live queue depth,
account-specific pricing, entitlements) require **your** credentials, which
should never live in the central scheduler.

`kubectl-fluence` resolves the choice **on your machine, in your own context**,
before the manifest reaches the cluster. It picks a backend and stamps it as the
`fluence.flux-framework.org/backend` annotation — the selection Fluence honors at
schedule time. Credentials never leave your machine; only the chosen backend
name enters the cluster.

The selection engine is **generic**: a "backend" is anything with attributes you
want to choose among — a GPU type, a storage tier, a region/cluster, or a quantum
device. Quantum (AWS Braket) is one *provider*; the cost/queue/capability
policies are provider-agnostic. For constraints that can live in the scheduler,
the filter and match will be supported there.

## Install

```bash
go build -o kubectl-fluence ./cmd/kubectl-fluence
mv kubectl-fluence /usr/local/bin/        # kubectl discovers it as a plugin
kubectl fluence version
```

## Use

Annotate the workload with a selection policy, then submit through the plugin:

```yaml
metadata:
  labels:
    fluence.flux-framework.org/group: my-group
  annotations:
    fluence.flux-framework.org/select-policy: "require:min_memory_gb=24,min-cost"
spec:
  schedulerName: fluence
```

```sh
# resolve + apply (drop-in for kubectl apply):
kubectl fluence apply -f job.yaml --attributes attrs.yaml

# just resolve and print the mutated manifest, don't apply:
kubectl fluence select -f job.yaml --attributes attrs.yaml
```

Any object that does **not** target Fluence (via `schedulerName: fluence` or the
`fluence.flux-framework.org/group` label) or does **not** carry a `select-policy`
annotation passes through unchanged, so the plugin is a safe replacement for
`kubectl apply`.

## How selection works

Selection has two orthogonal halves (see [docs/DESIGN.md](docs/DESIGN.md)):

- **Providers** retrieve candidate backends and their attributes. They are the
  only part that touches files, the cluster, or credentials.
  - `file` — static attributes (cost, capability, region, anything numeric or
    string) from your `--attributes` file. Fully generic; works for any kind of
    resource. **This is the cost source** — you may have pricing or metadata the
    scheduler doesn't know.
  - `braket-live` — live queue depth and device status from AWS Braket
    (`braket get-device`), using your credentials. Quantum-specific; only used
    when a policy needs it (`*-queue`, `online-only`).
- **Policies** are pure functions that filter/rank backends by their attributes,
  composed left-to-right into a pipeline. They are provider-agnostic — `min-cost`
  works the same whether the backend is a GPU or a QPU.

The engine merges attributes from all providers, **intersects** the result with
the backends the scheduler actually offers (read from the `fluence-resources`
ConfigMap), then runs the policy pipeline. Backends you described but the
scheduler doesn't offer — or that it offers but you didn't describe — are dropped
with a reported reason.

## Policies

| Policy | Effect |
|---|---|
| `min-cost` / `max-cost` | rank by `cost_usd` (or, for quantum, computed `per_task + shots*per_shot`) |
| `min-queue` / `max-queue` | rank by `queue_size` (needs a provider that supplies it, e.g. `braket-live`) |
| `min-<attr>` / `max-<attr>` | rank by any numeric attribute, e.g. `min-cost`, `max-throughput` |
| `require:min_X=N` / `require:max_X=N` | keep backends with attribute X ≥ / ≤ N (e.g. `require:min_memory_gb=24`) |
| `online-only` | drop devices not `ONLINE` (needs a status-providing provider) |
| `prefer:<name>` | choose `<name>` if available, else defer to the rest |

Compose them. `cost` and `queue` are accepted as friendly aliases for `cost_usd`
and `queue_size`. Example pipeline:

```console
require:min_memory_gb=24,min-cost,max-throughput
```

*keep backends with ≥24 GB, take the cheapest-equivalent set, break ties by throughput.*

## Attribute file

Generic — any numeric attribute under `attributes:`, any string under `strings:`:

```yaml
version: 1
backends:
  - name: gpu-l4
    provider: generic
    region: us-east-1
    attributes:
      cost_usd: 0.80
      memory_gb: 24
      throughput: 121
```

For quantum/Braket you can instead give cost as **components**, and the plugin
computes per-request cost from the shot count:

```yaml
  - name: rigetti_cepheus
    provider: braket
    device_arn: arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q
    region: us-west-1
    cost_per_task: 0.30
    cost_per_shot: 0.00090
    qubits: 108
```

You can include backends the scheduler doesn't offer — they're intersected out.

## Examples

- [`examples/generic-gpu.yaml`](examples/generic-gpu.yaml) +
  [`examples/generic-attributes.yaml`](examples/generic-attributes.yaml) — select
  a GPU type by memory + cost. No credentials, no cluster needed; this is the
  case CI exercises and the one to try with a [kind](https://kind.sigs.k8s.io)
  cluster (see [docs/KIND.md](docs/KIND.md)).
- [`examples/gang-select.yaml`](examples/gang-select.yaml) +
  [`examples/attributes.yaml`](examples/attributes.yaml) — quantum (Braket)
  backend selection by qubits + cost.

## Caveats

- **Submit-time snapshot.** Dynamic attributes (queue depth) are read when you
  submit; the job may sit gated before it runs, so the value can be stale by
  dispatch. This is a client-side hint, not a dispatch-time decision.
- **Pricing is configured, not fetched** (for Braket there is no per-device price
  API). Keep your attribute file current.
- **ConfigMap RBAC.** Reading scheduler offerings needs read access to the
  `fluence-resources` ConfigMap. If unavailable, selection proceeds over all
  provided backends and says so.


## License

Distributed under the MIT license; all contributions must be made under it. 

See [LICENSE](LICENSE), [COPYRIGHT](COPYRIGHT), and [NOTICE](NOTICE).

SPDX-License-Identifier: MIT

LLNL-CODE-842614

