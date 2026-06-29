# Bare-metal Fleet Manager — Components & Build Tasks

Status legend: `[x]` done · `[~]` partial / skeleton · `[ ]` to do.
Type: **build** = you write it · **stock** = configure existing · **config** = declarative setup.

---

## Component inventory

| # | Component | Plane | Type | Role | Status |
|---|-----------|-------|------|------|--------|
| 1 | `HostClaim` CRD | Git/MCE | build | Declarative capacity request (selector+count+cluster) | `[x]` |
| 2 | `InventoryRecord` CRD | Git/MCE | build | Host: declared spec + runtime status (lease/allocation); hardware facts in Postgres | `[x]` |
| 3 | Store schema | Store | build | inventory/lease/allocation/state/reservation/reach + views | `[x]` |
| 4 | Store Go (interfaces+pg) | Store | build | lease CAS, inventory, lifecycle, capacity, reservations, forecast, eligibility | `[x]` |
| 5 | Claim reconciler | MCE | build | Everyday local allocation (HostClaim → NodePool) | `[x]` |
| 6 | Binder (Agent) | MCE | build | NodePool agentLabelSelector binding | `[x]` |
| 7 | Collectors | Hub + MCE | build | Push inventory to store: Python collectors (OME/Intersight/UCS) on hub; Go collectors (BMH/Redfish) per-MCE | `[x]` |
| 8 | Classifier | MCE | stock | Class declared in `InventoryRecord.spec`; InfraEnv per class stamps `agentLabels` → superseded by #19 | `[x]` |
| 9 | IR reconciler — enroll phase | MCE | build | Lease acquire + BMH create + creds wiring + launch host-install workflow | `[x]` |
| 10 | IR reconciler — lifecycle phase | MCE | build | Reflect desired phase → BMH (power/maintenance/decommission) | `[ ]` |
| 11 | IR reconciler — move phase | MCE | build | Cross-MCE handoff (overflow) — deferred; spare buffer + enroll replenishment covers the common case | `[ ]` |
| 12 | Fleet allocator | Store-side | build | Eligibility + donor selection + emit moves; placement policy | `[ ]` |
| 13 | Discovery sources | MCE | build | Switch/aggregator → `discovered` hosts | `[ ]` |
| 14 | Argo Workflows | MCE | build+stock | host-install (PXE\|Redfish) + teardown/install gates | `[~]` |
| 15 | Capacity API + UI | Store | build | Regional read surface + holdings mgmt | `[ ]` |
| 16 | Per-MCE ArgoCD | MCE | config | Pull-mode GitOps, ApplicationSet, app-of-apps | `[ ]` |
| 17 | External Secrets / sealed | MCE | config | BMC creds without plaintext in Git | `[ ]` |
| 18 | Metal3 BMO + Ironic | MCE | stock | Provisioning, two boot methods | `[ ]` |
| 19 | Assisted / InfraEnv | MCE | stock | Agent platform; one InfraEnv per class with `agentLabels` = class label (replaces Classifier) | `[ ]` |
| 20 | `mce_reach` config | Store | config | Which MCE serves which segment (eligibility) | `[ ]` |

---

## Tasks by component

### Done — refine as needed
- [x] **HostClaim CRD** — selector/count/targetHostedCluster/nodePool/allowSpill; phase + unsatisfiableReason.
- [x] **InventoryRecord CRD** — declared spec (no cluster), runtime status (ownership + allocation only). Hardware facts live in Postgres, not in the CR.
- [x] **Store schema** — `host_inventory`, `host_lease`, `host_allocation`, `host_state`, `host_reservation(+member)`, `mce_reach` (incl. `vlan_id` per segment per MCE), `host_nics`, `host_topology`; views `host_capacity`, `region_headroom`, `host_eligible_mce`.
- [x] **Store Go** — `LeaseStore` (CAS Transition + Acquire/Release helpers), `InventoryStore`, `LifecycleStore` (SetHostPhase + EligibleMCEs), `CapacityStore`, `ReservationStore`, `ForecastStore`; pgx impl.

### 5. Claim reconciler `[x]`
- [x] Core reconcile: class from selector → EnsureNodePool → BoundCount → status.
- [x] Unsatisfiable-with-reason; spill signal hook (nil-safe).
- [x] **Allocation write-back**: projector resolves Agent via `agent-install.openshift.io/bmh=<serviceTag>`, calls `store.SetAllocation`, mirrors to `status.allocation`. Polled every 30s.
- [x] **Maintenance-aware availability**: `availableHosts` uses `store.Capacity` (excludes maintenance/decommissioning via `host_capacity` view) when store is wired; falls back to Agent count.
- [x] **SpillRequester**: `StoreSpillRequester` writes to `host_spill_request` (upsert on shortfall, delete on satisfied). Fleet allocator (#12) reads that table. Wired in main.go when Postgres is present; nil otherwise (Unsatisfiable fallback).

### 6. Binder (Agent) `[x]`
- [x] AgentBinder: AvailableHosts (approved+unbound Agents by class), EnsureNodePool, BoundCount.
- [x] **Pin API versions**: field paths verified against upstream source (assisted-service api/v1beta1, hypershift api/hypershift/v1beta1). GVKs, `spec.approved`, `spec.clusterDeploymentName.name`, `spec.platform.agent.agentLabelSelector`, `status.replicas` all confirmed. RBAC markers added for Agent (list/watch) and NodePool (get/list/watch/update/patch). Cluster integration test still needed (see TODO in binder.go).
- [x] Exclude hard-held hosts: `AddReservationMember`/`RemoveReservationMember`/`ListReservationMembers` added to `ReservationStore` + pg impl. `host_capacity` view now excludes hard-pinned service tags from `available` via `hard_held_hosts` helper view.

### 7. Collectors `[~]`
- [x] Go: `Collector` interface + registry + `bmh` stub. `switchtopo` superseded.
- [x] Python: `collectors/ome.py`, `collectors/cisco_intersight.py`, `collectors/ucscentral.py` — real implementations using vendor SDKs. Write directly to `host_inventory` + `host_topology` (bypass Go seam).
- [x] **NIC inventory**: `host_nics(service_tag, mac, name, speed_mbs)`. OME: `serverNetworkInterfaces` (NicId, CurrentMacAddress, LinkSpeed). Intersight: `AdapterHostEthInterface` (name, mac_address, max_speed). UCS Central: `adaptorExtEthIf` (dn→name, mac, oper_speed parsed from "Ngbps" string).
- [x] **Topology scraping**: `host_topology(service_tag, nic_mac, leaf_name, leaf_port, leaf_mgmt)`. OME: `serverConnectedPortProfiles` (iDRAC Connection View; falls back to NIC MACs only). Intersight: `AdapterHostEthInterface.peer_interface` DN. UCS Central: `adaptorExtEthIf.peer_dn`. NIC and topology share one API call per server.
- [x] **Finish `bmh`** (Go): `MapHardwareDetails` exported from `pkg/inventory/bmh`; InventoryRecord reconciler calls `discoverFacts` on every reconcile — reads co-located BMH (same name+namespace), writes discovered facts directly to `store.UpsertHost` (not to IR status). Watches BMH changes to trigger IR reconcile. RBAC marker added.
- [x] **OME session management**: store session ID from login response body; `disconnect()` issues `DELETE /api/SessionService/Sessions('{id}')` before reconnect (prevents session pool exhaustion on OME). Matches reference pattern from `dell_server_strategy.py`.
- [x] **Finish `cisco_intersight.py`**: cores = `num_threads // 2` (logical→physical, HT assumed; falls back to socket count); storage via `storage_api.get_storage_physical_disk_list` filtered by `RegisteredDevice.Moid`.
- [x] **`UpsertHost` COALESCE fix**: IR reconciler no longer stomps Python collector writes — `ON CONFLICT` uses `COALESCE(NULLIF(EXCLUDED.x,''), existing)` for text fields and `CASE WHEN > 0` for numeric fields.
- [x] **`redfish` collector** (Go, `pkg/inventory/redfish`): per-host fallback for whitebox / generic BMC hosts. Resolves `spec.bmc.credentialsRef` Secret → queries Redfish `/redfish/v1/Systems` → extracts vendor/model/cores/RAM/storage. Wired into IR reconciler after BMH enrichment; only fires when `bmc.type=generic` and BMH has no introspection data. Handles all BMC URL schemes (redfish://, redfish+http://, idrac-virtualmedia://, ilo5://, bare IP). RBAC: Secrets get. Security: SSRF guard (RFC1918 allowlist + redirect blocking), cross-namespace Secret exfiltration prevented (always uses `rec.Namespace`).

### 8. Classifier `[x]` — superseded by InfraEnv
Class is declared in `InventoryRecord.spec.class` (GitOps, set at enrollment). No runtime derivation needed.
- [x] One `InfraEnv` per class; `spec.agentLabels: {"inventory.example.io/class": "<class>"}` stamps all Agents from that InfraEnv automatically.
- [x] BMH `image.url` points at the class-matching InfraEnv ISO (set by enroll controller from spec.class).
- [x] NodePool `agentLabelSelector` matches on the class label. Nothing else needed.
See #19 for InfraEnv config.

### 9–11. IR reconciler phases `[ ]`

All lifecycle logic lives in `internal/controller/inventoryrecord_controller.go` — a single
reconciler with an internal phase dispatch. There are **2 per-MCE controllers total**:
`HostClaim` reconciler and `InventoryRecord` reconciler. No separate enroll/lifecycle/move
controller files.

The IR reconciler dispatches on lease state + `spec.desiredPhase`:

```
lease == nil || Free               →  reconcileEnroll      [x] built
spec.desiredPhase == maintenance   →  reconcileMaintenance [ ] #10
spec.desiredPhase == decommission  →  reconcileDecommission[ ] #10
lease.State == Releasing           →  reconcileRelease     [ ] #11 deferred
default (Owned, in_service)        →  reconcileInService   [x] built
```

#### 9. Enroll phase `[x]`
- [x] On Free/nil lease: `Acquire` (Free→Owned) → confirm `lease.OwnerMCE == r.MCE` → copy BMC creds Secret to `bmc-<serviceTag>` in `rec.Namespace` → create `enroll-<serviceTag>` Workflow referencing `host-install` WorkflowTemplate.
- [x] Poll Workflow status → mirror to IR `Enrolled` condition (Unknown/True/False). `Enrolled=True` flips dispatch to `reconcileInService`. `SetHostPhase(in_service)` called by the workflow's `register` step (`fleetctl register`).
- [x] Method derived from `spec.bmc.type` + `bootMACAddress`: generic+MAC → `ipmi-pxe`, all others → `redfish`.
- [x] Failed Workflow sets `Enrolled=False`; manual deletion of the Workflow object required before re-enrollment.
- [x] **NMState VLAN config** (`create-nmstate` step, runs before `create-bmh`): `fleetctl nmstate` queries `mce_reach.vlan_id` by `(mce, segment)` for the VLAN ID and `host_nics` by `bootMACAddress` for the NIC name, then creates `NMStateConfig` (nmstate.io/v1) labeled `infraenvs.agent-install.openshift.io: <class>`. Two-interface layout: base ethernet NIC (no IP) + VLAN sub-interface with DHCP. VLAN is per segment — multiple classes can share one VLAN; same segment name maps to different VLAN IDs on different MCEs. See ARCHITECTURE.md §7 and §5 enrollment flow. `fleetctl nmstate` still needs to be built as part of #14 fleet-tools.

#### 10. Lifecycle phase `[ ]`
- [ ] Watch `spec.desiredPhase` (GitOps-written) → `SetHostPhase` in store → reflect on BMH:
  - `maintenance`: power off + `spec.online=false` + Metal3 maintenance annotation.
  - `decommissioning`: trigger cleaning workflow, remove from eligible pool.
- [ ] Clear `spec.desiredPhase` (or set `in_service`) re-enables claiming.

#### 11. Move phase `[ ]` — DEFERRED

Spare hosts stay enrolled in their home MCE as a fast local buffer (minutes to
bind). Discovered-but-not-enrolled hosts in the DB replenish any MCE's buffer
via the enroll phase — no cross-MCE handoff needed.

The move phase is only required if **all** discovered hosts in the region are
already enrolled in the wrong MCE AND a cluster still needs more capacity.
That's an edge case unlikely to hit before production scale. Defer until
observed in production.

If eventually needed:
- [ ] Releasing side: drain → deprovision → teardown gate (Argo Workflow) → `BeginRelease` → `FreeLease` → delete BMH/Secret.
- [ ] Acquiring side: `Acquire` (Free→Owned) → ESO creds → create BMH → inspect → claim binds.
- [ ] Crash-safe: re-entry from lease state + BMH actual state on each reconcile.

### 12. Fleet allocator `[ ]`
- [ ] Eligibility filter: `available ∧ class ∧ EligibleMCEs(target)`.
- [ ] Placement policy over `EligibleMCEs` (free-headroom-weighted / least-loaded / manual).
- [ ] Donor selection + emit moves; quotas/backpressure.

### 13. Discovery sources `[ ]`
- [ ] Switch-collector discovery: unknown MAC on a leaf → `discovered` host with segment/site.
- [ ] OME/Intersight auto-discovery → `discovered` hosts.
- [ ] Operator/auto adoption: pick from `EligibleMCEs` → enroll.

### 14. Argo Workflows `[~]`
- [x] Templates drafted: `workflows/host-install.yaml` (branches Redfish vs IPMI+PXE), `verify-teardown.yaml`, `verify-install.yaml`.
- [ ] Build the `fleet-tools` image the templates call (preflight/classify/register/verify).
- [ ] Install Argo Workflows per MCE (mirrored images, RBAC).
- [ ] Teardown WorkflowTemplate: ramdisk disk-wipe readback + k8s-orphan assertion.
- [ ] Install WorkflowTemplate: node Ready + config + operators.
- [ ] Wire: move controller blocks lease transitions on gate result; failure quarantines.

### 15. Capacity API + UI `[ ]`
- [ ] Read API over `region_headroom` / `host_capacity` / `host_eligible_mce` (replicas).
- [ ] Holdings CRUD (`UpsertReservation` / `ListReservations` / `DeleteReservation`).
- [ ] Dashboard: per-class total/allocated/spare/maintenance/discovered/reserved + shortage; discovered hosts + eligible MCEs.

### 16–20. GitOps & stock `[ ]`
- [ ] Per-MCE ArgoCD: OpenShift GitOps, ApplicationSet (mce-scoped Git generator), app-of-apps, internal CA/registry.
- [ ] ExternalSecrets/sealed-secrets for BMC creds.
- [ ] Metal3 provisioning network for Redfish-virtualmedia + IPMI+PXE; `automatedCleaningMode`; image mirroring.
- [ ] InfraEnv per class with `agentLabels`.
- [ ] Populate `mce_reach` (mce, site, segment) for every MCE.

### Cross-cutting `[ ]`
- [x] Dev/test harness outside air-gap: `docker-compose.yaml` (Postgres), `hack/dev-setup.sh` (kind + stub CRDs), `hack/mock/ome|intersight|ucscentral` (mock servers), `config/test/samples/` (test CRs). See `docs/testing.md`.
- [ ] Air-gap pipeline: skopeo mirroring, internal Git mirror, CA trust.
- [ ] Observability: controller metrics, lease-transition audit, claim-pending reasons in UI.
- [ ] HA review: Postgres failover; confirm store outage stalls only new moves.

---

## Suggested order

1. **Make the everyday path live**: #9 enroll phase (IR reconciler), #16/#17/#18/#19 config. → declarative allocation works end to end.
2. **Regional surface**: #15 Capacity API + UI, #20 `mce_reach`, #13 discovery. → full visibility incl. spare/maintenance/discovered + shortage.
3. **Lifecycle**: #10 maintenance phase (IR reconciler).
4. **Overflow** (deferred): #12 allocator, #11 move phase, #14 gates. Only needed if all discovered hosts are already enrolled in the wrong MCE.
