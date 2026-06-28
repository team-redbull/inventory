# Bare-metal Fleet Manager

Claim-based bare-metal allocation over Metal3 / MCE / HyperShift — air-gapped,
multi-MCE-per-site, multi-site. Request capacity by class for a HostedCluster,
move hosts between MCEs when needed, and see/forecast the whole region from one
store.

> **Status: early scaffold.** The data spine (CRDs + store) is built; the
> everyday allocation path is a working skeleton; the overflow/move machinery is
> designed and stubbed. See [`BUILD.md`](BUILD.md) for the component status table
> and task list.

## Start here

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — what it is, the three planes, the flows,
  the design decisions.
- [`BUILD.md`](BUILD.md) — every component, its status, and the creation tasks.
- [`docs/`](docs/) — architecture + flow diagrams (SVG):
  `architecture`, `flow-allocation`, `flow-move`, `flow-enrollment`.

## Layout

```
api/v1alpha1/        CRDs: HostClaim, InventoryRecord
pkg/store/           central store: lease CAS, inventory, lifecycle, capacity, holdings, forecast
pkg/binder/          NodePool binding seam (AgentBinder live; CAPM3 stub)
pkg/inventory/       collectors (bmh primary; ome/ucs/switch/redfish)
internal/controller/ claim reconciler (everyday allocation)
cmd/manager/         per-MCE manager entrypoint
db/schema.sql        store schema (Postgres)
workflows/           Argo WorkflowTemplates: host-install (PXE|Redfish), verify-teardown, verify-install
config/samples/      example HostClaim / InventoryRecord + store objects
docs/                diagrams
```

## Quickstart (dev)

```bash
make tidy                      # against your internal module proxy
make generate                  # DeepCopy + CRD manifests
make db DATABASE_URL=postgres://...   # apply the store schema
make run MCE=mce-1             # run the manager against the current kubecontext
```

Pin the OpenShift modules in `go.mod` to the versions your MCE runs — the
assisted-service and HyperShift API paths/fields drift across releases.

## How the pieces connect (one paragraph)

Git holds intent (claims, host records, platform); each MCE's ArgoCD pulls its
slice. The **claim reconciler** turns a `HostClaim` into a sized NodePool via the
**AgentBinder** and binds local Agents — the 90% path. The **store** (Postgres)
aggregates every MCE's inventory and holds the authoritative ownership lease;
its `region_headroom` view answers capacity/shortage. When a cluster needs hosts
its MCE doesn't have, the **fleet allocator** picks eligible donors and the
**move controller** runs a cross-MCE handoff gated by Argo Workflows, serialized
by a single CAS lease. Enrollment and moves are workflow-driven because the boot
methods (Redfish vs IPMI+PXE) and the teardown/verification differ per host.

## Contributing

Pick a `[ ]` or `[~]` item from [`BUILD.md`](BUILD.md). The everyday path
(reconciler write-back, binder version-pin, BMH collector + classifier) is the
highest-leverage area to make end-to-end allocation real.
