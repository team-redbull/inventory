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
| 5 | Claim reconciler | MCE | build | Everyday local allocation (HostClaim → NodePool) | `[~]` |
| 6 | Binder (Agent) | MCE | build | NodePool agentLabelSelector binding | `[~]` |
| 7 | Collectors | MCE | build | Push inventory/topology to store (bmh/ome/ucs/switch/redfish) | `[~]` |
| 8 | Classifier | MCE | build | Derive class label from hardware profile | `[ ]` |
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
| 19 | Assisted / InfraEnv | MCE | stock | Agent platform; per-class `agentLabels` | `[ ]` |
| 20 | `mce_reach` config | Store | config | Which MCE serves which segment (eligibility) | `[ ]` |

---

## Tasks by component

### Done — refine as needed
- [x] **HostClaim CRD** — selector/count/targetHostedCluster/nodePool/allowSpill; phase + unsatisfiableReason.
- [x] **InventoryRecord CRD** — declared spec (no cluster), discovered status, ownership/allocation reflection.
- [x] **Store schema** — `host_inventory`, `host_lease`, `host_allocation`, `host_state`, `host_reservation(+member)`, `mce_reach`; views `host_capacity`, `region_headroom`, `host_eligible_mce`.
- [x] **Store Go** — `LeaseStore` (CAS Transition + Acquire/Release helpers), `InventoryStore`, `LifecycleStore` (SetHostPhase + EligibleMCEs), `CapacityStore`, `ReservationStore`, `ForecastStore`; pgx impl.

### 5. Claim reconciler `[~]`
- [x] Core reconcile: class from selector → EnsureNodePool → BoundCount → status.
- [x] Unsatisfiable-with-reason; spill signal hook (nil-safe).
- [ ] **Allocation write-back**: on bind, call `store.SetAllocation` so hosts flip to `allocated` in the capacity view.
- [ ] **Maintenance-aware availability**: exclude non-`in_service` hosts when counting available.
- [ ] Wire a real `SpillRequester` (Phase 3).

### 6. Binder (Agent) `[~]`
- [x] AgentBinder: AvailableHosts (approved+unbound Agents by class), EnsureNodePool, BoundCount.
- [ ] **Pin API versions** (assisted-service / HyperShift import paths + field names) and compile against the cluster.
- [ ] Exclude hard-held hosts (once hard holdings are wired).

### 7. Collectors `[~]`
- [x] Stubs: `Collector` interface + registry; `bmh`, `ome`, `intersight`, `ucsm`. `switchtopo` superseded — topology from BMC (iDRAC Connection View / Intersight fabric).
- [ ] **Finish `bmh`**: map introspection → store `UpsertHost`; per-host error isolation.
- [ ] **Finish enrichment** `ome`/`ucs` (confirm Intersight vs UCS Central); `redfish` fallback.
- [ ] **Switch topology**: poll leaves; MAC→NIC join; write segment + leaf/port.

### 8. Classifier `[ ]`
- [ ] Define the 3–5 host classes and the hardware-profile → class rules.
- [ ] Stamp `inventory.example.io/class` on the BMH; propagate to the Agent (InfraEnv `agentLabels` per class, or a copy reconciler).

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
- [ ] Dev/test harness outside air-gap: sushy-emulator, Intersight/UCSPE emulator, Redfish mockups; transport abstraction.
- [ ] Air-gap pipeline: skopeo mirroring, internal Git mirror, CA trust.
- [ ] Observability: controller metrics, lease-transition audit, claim-pending reasons in UI.
- [ ] HA review: Postgres failover; confirm store outage stalls only new moves.

---

## Suggested order

1. **Make the everyday path live**: finish #5 (allocation write-back, maintenance-aware), pin #6, finish #7 `bmh` + #8 classifier, #9 enroll, #16/#17/#18/#19 config. → declarative allocation works end to end.
2. **Regional surface**: #15 Capacity API + UI, #20 `mce_reach`, #13 discovery. → full visibility incl. spare/maintenance/discovered + shortage.
3. **Lifecycle**: #10 maintenance reflection.
4. **Overflow**: #12 allocator, #11 move controller, #14 gates. → cross-MCE movement with verification.
