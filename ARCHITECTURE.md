# Bare-metal Fleet Manager — Architecture

A claim-based bare-metal allocation system over Metal3 / MCE / HyperShift,
air-gapped, spanning multiple MCEs per site and multiple sites in a region.

---

## 1. What it does

Two goals:

1. **Allocate and move bare metal automatically** — request capacity by class for
   a HostedCluster ("3 of type A in workload-blue"), and relocate hosts between
   MCEs when needed, without hand-managing individual servers.
2. **One regional control surface** — know every server across every site: what it
   is, who owns it, whether it's allocated, spare, in maintenance, or newly
   discovered — and forecast shortages before they happen.

The interface is **declarative capacity claims**, not per-host placement. You say
*how much of what, where*; the system decides *which* hosts satisfy it. Pinning a
specific host is the degenerate case of the same mechanism (a selector that
matches one host).

---

## 2. Three planes

The whole design is one principle: **read globally from the store, write globally
through Git, act locally in each MCE.**

**Desired state — Git.** `fleet-config` holds the declared intent: `HostClaim`s,
`InventoryRecord`s (enrollment facts), and the platform manifests. Each MCE's
standalone ArgoCD pulls only its own slice (ApplicationSet scoped by MCE label),
destination always in-cluster. No hub pushes; no ManifestWork. Adding an MCE costs
the control plane nothing.

**Runtime truth — the store (Postgres).** The single fleet-wide component, and the
only shared write point — deliberately **off the data path** (it carries small
facts and lease transitions, never provisioning bandwidth). It holds the
authoritative ownership lease, the aggregated inventory, allocations, holdings,
lifecycle state, and the capacity/headroom views. It is *not* a Kubernetes hub.

**Execution — each MCE.** Holds only its own slice of objects (BMHs, Agents,
NodePools, InventoryRecords) plus the controllers that act on them. The data path
— ISO serving, PXE, disk writes, ignition — never leaves the MCE.

> Key consequence: an MCE only has Kubernetes objects for hosts it currently owns.
> The store is the only thing with the whole fleet. "Which MCE owns host X right
> now" is answered by the **lease row in the store**, never by searching MCEs.

---

## 3. Data model — declared vs discovered

`InventoryRecord` (a.k.a. Host) splits cleanly:

- **spec** (declared, GitOps, authored at enrollment): serviceTag, BMC address +
  method + credentialsRef, `network.segment`, physical placement. The cluster
  binding is deliberately **not** here.
- **status** (discovered/runtime, controller-written): hardware from introspection,
  reflected ownership, and the **allocation outcome** (which HostedCluster/NodePool
  consumes it).

`HostClaim` is the interface: a label `selector` + `count` + `targetHostedCluster`
(+ optional `nodePool`, `allowSpill`). One selector spans "any N of class A" through
"this exact host." Status carries a `phase` and, critically, an
`unsatisfiableReason` so a claim never sits silently pending when it's impossible.

The **class label** (`inventory.example.io/class`) is the hinge: assigned at
enrollment from the hardware profile, it's what NodePool selectors match and what
the capacity view groups by.

---

## 4. The lifecycle of a host

A host is always in exactly one phase, and — except for a brief mid-move window —
always has a **home MCE that holds its BMH** (the operational grip):

- **discovered** — found in a site (by the switch or an aggregator), not yet
  enrolled in any MCE. Visible regionally, not yet gripped or introspected.
- **in_service / spare** — enrolled, BMH in its home MCE, inspected, available to
  claim. Idle but fully manageable.
- **in_service / allocated** — bound to a HostedCluster.
- **maintenance** — BMH held in the home MCE, powered off / Metal3 maintenance,
  excluded from the claimable pool. Still gripped for repair.
- **decommissioning** — being wiped/removed.

The only BMH-less moment is the transient `Free` lease window during a cross-MCE
move; even then the lease in the store knows where the host is.

---

## 5. The flows

**Enrollment & discovery.** The switch-topology collector (or OME/Intersight)
notices new hardware in a site and registers it as a `discovered` host with its
segment. To bring it into service you pick an MCE from `EligibleMCEs` (computed
from segment reach), then the **`host-install` Argo WorkflowTemplate** runs —
branching on boot method (Redfish virtual-media vs IPMI+PXE) before converging on
create-BMH → Ironic inspect → classify → register. The class is stamped on the
BMH/Agent and facts pushed to the store. Phase → spare.

**Inventory & capacity.** Every MCE's collectors push discovered facts up (CQRS
write side; nothing polls down). The store aggregates into `host_capacity` (per
MCE) and `region_headroom` (per class, region-wide: total / allocated /
maintenance / discovered / spare / reserved / free_headroom / shortage). The
Capacity API + UI reads this — the regional control surface.

**Everyday allocation (the 90% path).** A `HostClaim` lands in Git → ArgoCD
delivers it to the MCE hosting the target cluster → the **claim reconciler**
resolves it local-first: the **AgentBinder** sizes the HostedCluster's NodePool
and writes the class into its `agentLabelSelector`; the HyperShift agent provider
binds matching Agents; nodes join. No lease, no move — stock Metal3/HyperShift plus
one reconciler.

**Holdings & shortage.** A region-scoped `host_reservation` earmarks capacity not
yet bound to a cluster. `region_headroom` subtracts active holdings from spare:
`free_headroom = spare − reserved`, and `shortage` trips when holdings over-commit
the spare pool — answering "will there be a shortage of type A" before you claim.

**Overflow (the rare path).** If the local pool is short and `allowSpill` is true,
the reconciler signals the **fleet allocator** (the only component that reads the
whole region). It filters donors by `available ∧ class ∧ segment-reachable`, picks
some, and emits moves. The **move controller** runs the handoff: drain →
deprovision → **Argo Workflows teardown gate** (disks wiped, no orphans) → lease
`Owned→Releasing→Free` (CAS) → target MCE claims `Free→Owned` → BMH recreated →
inspect → bind → **install gate** (node Ready, config applied) → complete. A failed
gate holds the lease and quarantines the host.

---

## 6. Key design decisions

- **No ManifestWork / no central push.** Per-MCE standalone ArgoCD in pull mode.
  The hub-shaped role shrinks to a small transactional store off the data path.
- **Single-writer via lease CAS.** A physical BMC may be managed by one MCE at a
  time. One conditional `UPDATE ... WHERE state=? AND generation=?` is the entire
  guarantee; on conflict, callers re-read — never force.
- **Store off the data path.** Load is O(state changes), not provisioning
  bandwidth, so it never bottlenecks. Its outage stalls *new* moves only; running
  clusters and in-flight installs are unaffected.
- **Claims over per-host placement.** Fungible-within-a-class; pinning is the
  degenerate case. Requires hosts to fall into a small number of classes.
- **Agent platform.** Binding is via NodePool `agentLabelSelector`; the one
  method-specific seam is isolated in `pkg/binder`.
- **Holdings are region-scoped store rows**, not per-MCE CRDs — there's no hub
  cluster to host a region-wide object, and reservation is an accounting overlay.
- **The home MCE is the grip.** Spare and maintenance hosts keep their BMH, so
  they stay operationally controllable without being in a workload cluster.

---

## 7. Repository layout

```
inventory/
  api/v1alpha1/            CRDs
    hostclaim_types.go         claim interface
    inventoryrecord_types.go   host: declared spec + discovered status
    groupversion_info.go
  pkg/store/              central store
    store.go                   interfaces (lease/inventory/lifecycle/capacity/reservation/forecast)
    postgres.go                pgx implementation
  pkg/binder/             NodePool binding seam
    binder.go                  AgentBinder (live) + CAPM3Binder (stub)
  pkg/inventory/          collectors
    collector.go               Collector interface + registry
    bmh/ ome/ ucs/ switchtopo/ adapters
  internal/controller/
    hostclaim_controller.go    the everyday allocation reconciler
  cmd/manager/              per-MCE manager entrypoint
  workflows/               Argo WorkflowTemplates
    host-install.yaml          enroll: branches Redfish vs IPMI+PXE
    verify-teardown.yaml       move gate: disks wiped + no orphans
    verify-install.yaml        move gate: node ready + config
  db/schema.sql           store schema (inventory, lease, allocation, state,
                          reservation, mce_reach + capacity/headroom/eligibility views)
  config/samples/         example HostClaim / InventoryRecord manifests
  docs/                   architecture + flow diagrams (SVG)
```

See `BUILD.md` for the component inventory and creation tasks.
