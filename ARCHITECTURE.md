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

**Runtime truth — the hub (Postgres + vendor collectors).** The hub is deliberately
narrow: a Postgres instance and the Python collector Deployments that feed it. No
Kubernetes API server required on the hub — just the database and the collector pods.
The store holds the authoritative ownership lease, the aggregated inventory,
allocations, holdings, lifecycle state, and the capacity/headroom views.
The Python collectors (OME, Intersight, UCS Central) run here because they only need
network reach to the vendor APIs and to Postgres — no k8s API access, no MCE coupling.

**Execution — each MCE.** Holds only its own slice of objects (BMHs, Agents,
NodePools, InventoryRecords) plus the controllers that act on them. The Go collectors
(BMH introspection, Redfish) run here because they need k8s API access to read
BareMetalHost objects and BMC credential Secrets. The data path — ISO serving, PXE,
disk writes, ignition — never leaves the MCE.

> Key consequence: an MCE only has Kubernetes objects for hosts it currently owns.
> The store is the only thing with the whole fleet. "Which MCE owns host X right
> now" is answered by the **lease row in the store**, never by searching MCEs.

---

## 3. Data model — declared vs runtime

`InventoryRecord` (a.k.a. Host) splits cleanly:

- **spec** (declared, GitOps, authored at enrollment): serviceTag, BMC address +
  method + credentialsRef, `network.segment`, physical placement, class. The cluster
  binding is deliberately **not** here.
- **status** (runtime, controller-written): reflected ownership lease and the
  **allocation outcome** (which HostedCluster/NodePool consumes it). Hardware facts
  are **not** cached in the CR — they live exclusively in the central store.

**Postgres is the single source of truth for hardware facts** (vendor, model, cores,
RAM, storage). Go collectors (BMH introspection, Redfish) and Python collectors (OME,
Intersight, UCS Central) all write directly to `host_inventory`. The IR reconciler
reads only spec fields; it never caches hardware in status. This eliminates the
dual-truth problem where k8s status and Postgres could drift.

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

The only BMH-less moment is the transient `Free` lease window during an intra-site
MCE move; even then the lease in the store knows where the host is.

---

## 5. The flows

**Enrollment & discovery.** The **hub-side enroll-bot** watches the store for
`discovered` hosts, picks a target MCE via `EligibleMCEs(segment)`, and writes the
`InventoryRecord` into `fleet-config` (GitOps PR or direct API write). ArgoCD
delivers the IR to the target MCE. The **IR reconciler** then drives enrollment:

```
IR reconciler (per-MCE)
  ↓  acquire lease (Free → Owned, CAS)
  ↓  copy BMC credential Secret → bmc-<serviceTag>
  ↓  launch enroll-<serviceTag> Argo Workflow
     │
     ├─ redfish-prep / pxe-prep  (parallel, conditional on boot method)
     ↓
     create-nmstate   — NMStateConfig for VLAN tagging (before ISO boot)
     ↓
     create-bmh       — BareMetalHost created; Ironic takes over
     ↓
     wait-available   — introspection completes
     ↓
     classify         — class label stamped on BMH + Agent
     ↓
     register         — facts pushed to store, phase → spare
  ↓  IR reconciler polls workflow → mirrors phase → Enrolled condition
  ↓  Enrolled=True flips dispatch to reconcileInService
```

**Provisioning VLAN tagging (`create-nmstate`).** Before the host boots the
Assisted Installer discovery ISO it must receive DHCP on the correct provisioning
VLAN. The `NMStateConfig` (nmstate.io/v1) object instructs the agent running inside
the ISO how to configure networking:

- **Two-interface config**: the physical boot NIC (MAC bound, no IP) plus a VLAN
  sub-interface (`<nic>.<vlan_id>`) with full DHCP. The physical NIC is "up" so the
  VLAN sub-interface can carry traffic, but holds no IP to avoid routing conflicts.
- **VLAN ID** comes from `mce_reach.vlan_id` keyed by `(mce, segment)`. VLAN is a
  property of the L2 segment the host is wired to — **not** a property of the class
  or InfraEnv. Multiple classes (InfraEnvs) can share the same VLAN when they live
  on the same physical segment; each MCE may assign different VLANs to the same
  segment name (different data-centre provisioning networks per MCE).
- **NIC name** resolved from `host_nics` by matching `spec.bmc.bootMACAddress` →
  the interface name the agent should configure. NIC data was written by the hub-side
  Python collectors (OME/Intersight/UCS) before enrollment begins.
- **InfraEnv binding** via label `infraenvs.agent-install.openshift.io: <class>`.
  Assisted Installer picks up every `NMStateConfig` carrying this label when the
  host registers; no BMH reference needed.
- `fleetctl nmstate` (per-MCE, store access) queries `mce_reach` for VLAN and
  `host_nics` for NIC name, renders the two-interface YAML, and applies it before
  `create-bmh`.

**Inventory & capacity.** Hub-side Python collectors (OME, Intersight, UCS Central)
poll vendor APIs and write hardware facts to the store continuously. Per-MCE Go
collectors (BMH introspection, Redfish) write during the IR reconcile loop. Nothing
polls down — all writes flow into Postgres (CQRS write side). The store aggregates
into `host_capacity` (per MCE) and `region_headroom` (per class, region-wide: total /
allocated / maintenance / discovered / spare / reserved / free_headroom / shortage).
The Capacity API + UI reads this — the regional control surface.

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

**Spare buffer and replenishment.** Each MCE keeps a small pool of enrolled spare
hosts (inspected, claimable in minutes). When an MCE's buffer drops, the
**hub-side enroll-bot** queries `discovered` hosts from the store, selects one via
`EligibleMCEs`, and triggers enrollment into that MCE directly — no cross-MCE
handoff needed. Discovered hosts are pre-known via the hub's OME/Intersight/UCS
collectors; their NIC facts and topology are already in Postgres before enrollment
starts, so `create-nmstate` can run without any additional discovery round-trip.

**Overflow (deferred).** Cross-MCE host moves are only needed if every discovered
host in the region is already enrolled in another MCE AND a cluster still needs
more capacity. That edge case is deferred until observed at production scale.
When eventually needed, the move phase in the IR reconciler runs:
drain → deprovision → **teardown gate** (disks wiped) → lease `Owned→Releasing→Free`
→ target MCE claims `Free→Owned` → BMH recreated → inspect → bind.

---

## 6. Where things run

| Component | Where | Why |
|-----------|-------|-----|
| Postgres | **Hub** | Fleet-wide shared state; one instance (or HA pair) per region |
| OME / Intersight / UCS Central collectors | **Hub** | Only need vendor API + Postgres access; no k8s dependency |
| Enroll-bot (Git PR automation) | **Hub** | Watches store for discovered hosts; needs Postgres + Git credentials |
| Capacity API + UI | **Hub** | Reads store views; no k8s dependency |
| Fleet allocator | **Hub** | Reads whole-region store; no k8s dependency |
| `fleet-manager` (IR + claim reconciler) | **Per-MCE** | Needs k8s API: BMH, Agents, Secrets, NodePools |
| Go collectors (BMH, Redfish) | **Per-MCE** (inside fleet-manager) | Need k8s API to read BareMetalHost + credential Secrets |
| Argo Workflows | **Per-MCE** | Provisioning actions run inside the MCE |
| ArgoCD | **Per-MCE** | Pulls fleet-config slice for this MCE only |

The hub has no Kubernetes API server requirement — Postgres + collector pods can run on a plain VM or a minimal container runtime. The "no k8s hub" principle holds: nothing on the hub manages k8s objects.

---

## 7. Key design decisions

- **Two per-MCE controllers, not five.** All IR lifecycle (enroll, maintenance,
  decommission, move) is a phase dispatch inside `inventoryrecord_controller.go`.
  One reconciler per CRD keeps the watch/predicate surface clean and avoids
  multiple controllers racing to patch the same IR status object. Dispatch layers:
  1. `Enrolled` condition (`metav1.Condition{Type:"Enrolled"}`) is the primary gate —
     `True` → `reconcileInService`; absent/Unknown/False → `reconcileEnroll`.
     This mirrors Argo Workflow phase (Unknown while running, True on Succeeded,
     False on Failed/Error) and is the only write that changes the dispatch branch.
  2. `spec.desiredPhase` drives maintenance/decommission inside `reconcileInService`.
  3. Lease state (`Releasing`) drives the move/release sequence (deferred).
  controller-runtime's single-active-worker-per-key guarantee ensures at most one
  goroutine patches a given IR's status at a time.

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
- **VLAN is per segment, not per class.** `mce_reach.vlan_id` is keyed by
  `(mce, segment)`. A segment is an L2 network; multiple hardware classes (InfraEnvs)
  can share one VLAN when they are wired to the same segment. Conversely, the same
  segment name can map to different VLAN IDs on different MCEs (different data-centre
  provisioning networks). This is why VLAN lives on `mce_reach` (the segment-reach
  config table) rather than on the InfraEnv object or as an IR spec field.
  `NMStateConfig` objects are labeled per class so the correct Assisted Installer
  InfraEnv picks them up, but the VLAN ID itself is a network infrastructure fact
  independent of the hardware class.

---

## 8. Repository layout

```
inventory/
  api/v1alpha1/            CRDs
    hostclaim_types.go         claim interface
    inventoryrecord_types.go   host: declared spec + runtime status (lease/allocation)
    groupversion_info.go
  pkg/store/              central store — single source of truth for hardware facts
    store.go                   interfaces (lease/inventory/lifecycle/capacity/reservation/forecast/network)
    postgres.go                pgx implementation
  pkg/binder/             NodePool binding seam
    binder.go                  AgentBinder (live) + CAPM3Binder (stub)
  pkg/inventory/          Go collectors (run per-MCE — need k8s API access)
    collector.go               Collector interface + registry
    bmh/                       Metal3 BareMetalHost introspection (primary, via k8s unstructured)
    redfish/                   per-host Redfish fallback for whitebox/generic BMC hosts
  collectors/             Python collectors (run on hub — need only Postgres + vendor network)
    ome.py                     Dell OME REST (requests)
    cisco_intersight.py        Cisco Intersight PVA (intersight SDK, HMAC auth)
    ucscentral.py              Cisco UCS Central (ucscentralsdk, XML API)
  config/collectors/      Kubernetes Deployment manifests for the Python collectors
  internal/controller/
    hostclaim_controller.go    the everyday allocation reconciler (HostClaim → NodePool)
    inventoryrecord_controller.go  IR state machine: enroll → in_service → maintenance/move
                               Dispatch: Enrolled condition (primary) → desiredPhase → lease state.
                               Built: enroll (#9, Enrolled condition + host-install workflow),
                                      inService (#9, allocation write-back + inventory projection).
                               Planned: lifecycle phase (#10), move phase (#11 deferred).
  cmd/manager/              per-MCE manager entrypoint
  workflows/               Argo WorkflowTemplates
    host-install.yaml          enroll: preflight → NMState VLAN config → create-BMH → inspect → classify → register
    verify-teardown.yaml       move gate: disks wiped + no orphans
    verify-install.yaml        move gate: node ready + config
  db/schema.sql           store schema (inventory, lease, allocation, state, reservation,
                          host_nics, host_topology, mce_reach (incl. vlan_id) +
                          capacity/headroom/eligibility views)
  config/samples/         example HostClaim / InventoryRecord manifests
  docs/                   architecture + flow diagrams (SVG)
```

See `BUILD.md` for the component inventory and creation tasks.
