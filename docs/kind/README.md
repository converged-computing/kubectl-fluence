# Trying kubectl-fluence with a kind cluster

[kind](https://kind.sigs.k8s.io) runs a local Kubernetes cluster in Docker. You
do **not** need Fluence installed to try the selection logic — the plugin
resolves the backend client-side and stamps an annotation; the cluster only needs
to accept the (annotated) manifest. This walkthrough uses the generic GPU example
(no quantum, no AWS).

## 1. Create a cluster

```sh
kind create cluster --name fluence-demo
```

## 2. (Optional) provide scheduler offerings

The plugin intersects your attribute file with the backends the scheduler
"offers", read from the `fluence-resources` ConfigMap. Without Fluence installed
there is no such ConfigMap, and the plugin simply selects over all backends in
your file (it prints a note saying so). To exercise the intersection locally, you
can create a stand-in ConfigMap that lists the offered backend names:

```sh
kubectl create configmap fluence-resources -n kube-system \
  --from-literal=resources.yaml='
- name: gpu-a100
- name: gpu-l4
'
```

With this, `gpu-t4` (in the attribute file but not offered) is dropped by the
intersection — try it and watch the "dropped" note.

## 3. Resolve a selection (no apply)

```sh
kubectl fluence select \
  -f examples/generic-gpu.yaml \
  --attributes examples/generic-attributes.yaml
```

You should see `gpu-l4` chosen (cheapest backend with >=24 GB) and the mutated
manifest printed with `fluence.flux-framework.org/backend: gpu-l4`.

## 4. Apply it

```sh
kubectl fluence apply \
  -f examples/generic-gpu.yaml \
  --attributes examples/generic-attributes.yaml
```

The pod is created with the backend annotation set. (Without Fluence the default
scheduler runs it; the annotation is inert but present — which is all the
client-side contract requires. With Fluence installed, Fluence honors the pin.)

## 5. Clean up

```sh
kind delete cluster --name fluence-demo
```
