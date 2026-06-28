# Bare-metal Fleet Manager — Components & Build Tasks

Status legend: `[x]` done · `[~]` partial / skeleton · `[ ]` to do.
Type: **build** = you write it · **stock** = configure existing · **config** = declarative setup.

---

## Component inventory

| # | Component | Plane | Type | Role | Status |
|---|-----------|-------|------|------|--------|
| 1 | `HostClaim` CRD | Git/MCE | build | Declarative capacity request (selector+count+cluster) | `[x]` |
| 2 | `InventoryRecord` CRD | Git/MCE | build | Host: declared spec + discovered status | `[x]` |
| 3 | Store schema | Store | build | inventory/lease/allocation/state/reservation/reach + views | `[x]` |
| 4 | Store Go (interfaces+pg) | Store | build | lease CAS, inventory, lifecycle, capacity, reservations, forecast, eligibility | `[x]` |
| 5 | Claim reconciler | MCE | build | Everyday local allocation (HostClaim → NodePool) | `[x]` |
| 6 | Binder (Agent) | MCE | build | NodePool agentLabelSelector binding | `[x]` |
| 7 | Collectors | MCE | build | Push inventory/topology to store (bmh/ome/ucs/switch/redfish) | `[~]` (redfish pending) |
| 8 | Classifier | MCE | stock | Class declared in `InventoryRecord.spec`; InfraEnv per class stamps `agentLabels` → superseded by #19 | `[x]` |
| 9 | Enroll controller | MCE | build | Lease acquire + BMH create + creds wiring | `[ ]` |
| 10 | Lifecycle/maintenance controller | MCE | build | Reflect phase → BMH (power/maintenance) | `[ ]` |
| 11 | Move controller | MCE | build | Cross-MCE handoff state machine (overflow) | `[ ]` |
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
- [x] **InventoryRecord CRD** — declared spec (no cluster), discovered status, ownership/allocation reflection.
- [x] **Store schema** — `host_inventory`, `host_lease`, `host_allocation`, `host_state`, `host_reservation(+member)`, `mce_reach`; views `host_capacity`, `region_headroom`, `host_eligible_mce`.
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
- [x] Python: `collectors/ome.py`, `collectors/cisco_intersight.py`, `collectors/ucscentral.py` — real implementations using vendor SDKs. Write directly to `host_inventory` (bypass Go seam).
- [x] **Finish `bmh`** (Go): `MapHardwareDetails` exported from `pkg/inventory/bmh`; InventoryRecord reconciler calls `enrichFromBMH` on every reconcile — reads co-located BMH (same name+namespace), merges `status.hardwareDetails` into IR status if inspected. Watches BMH changes to trigger IR reconcile. RBAC marker added.
- [x] **OME session management**: store session ID from login response body; `disconnect()` issues `DELETE /api/SessionService/Sessions('{id}')` before reconnect (prevents session pool exhaustion on OME). Matches reference pattern from `dell_server_strategy.py`.
- [x] **Finish `cisco_intersight.py`**: cores = `num_threads // 2` (logical→physical, HT assumed; falls back to socket count); storage via `storage_api.get_storage_physical_disk_list` filtered by `RegisteredDevice.Moid`.
- [x] **`UpsertHost` COALESCE fix**: IR reconciler no longer stomps Python collector writes — `ON CONFLICT` uses `COALESCE(NULLIF(EXCLUDED.x,''), existing)` for text fields and `CASE WHEN > 0` for numeric fields.
- [ ] **`redfish.py`** (Python): per-host fallback for whitebox hardware. Blocked on credential distribution (BMC creds are in k8s Secrets, not accessible from Python without explicit mounting).

### 8. Classifier `[x]` — superseded by InfraEnv
Class is declared in `InventoryRecord.spec.class` (GitOps, set at enrollment). No runtime derivation needed.
- [x] One `InfraEnv` per class; `spec.agentLabels: {"inventory.example.io/class": "<class>"}` stamps all Agents from that InfraEnv automatically.
- [x] BMH `image.url` points at the class-matching InfraEnv ISO (set by enroll controller from spec.class).
- [x] NodePool `agentLabelSelector` matches on the class label. Nothing else needed.
See #19 for InfraEnv config.

### 9. Enroll controller `[ ]`
- [ ] On a new InventoryRecord: resolve creds Secret → `Acquire` lease (Free→Owned) → launch the `host-install` WorkflowTemplate (branches on boot method) → drive to `available`.
- [ ] Set phase `in_service` and push initial inventory.

### 10. Lifecycle / maintenance controller `[ ]`
- [ ] Watch desired phase (Git/API) → `SetHostPhase` in store → reflect on BMH: power off + Metal3 maintenance for `maintenance`; cleaning for `decommissioning`.
- [ ] Restore to `in_service` re-enables claiming.

### 11. Move controller (overflow) `[ ]`
- [ ] Reconcile the move state machine; lease client (`BeginRelease`/`FreeLease`/`Acquire`).
- [ ] Source: drain → deprovision → invoke teardown gate → release lease + delete BMH/Secret.
- [ ] Target: claim lease → ESO creds → create BMH → inspect → (claim binds).
- [ ] Crash-safe: resume from lease + BMH actual state.

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

1. **Make the everyday path live**: finish #5 (allocation write-back, maintenance-aware), pin #6, finish #7 `bmh` + #8 classifier, #9 enroll, #16/#17/#18/#19 config. → declarative allocation works end to end.
2. **Regional surface**: #15 Capacity API + UI, #20 `mce_reach`, #13 discovery. → full visibility incl. spare/maintenance/discovered + shortage.
3. **Lifecycle**: #10 maintenance reflection.
4. **Overflow**: #12 allocator, #11 move controller, #14 gates. → cross-MCE movement with verification.
