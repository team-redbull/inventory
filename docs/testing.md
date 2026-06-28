# Local Testing Guide

How to exercise each external component outside the air-gapped environment.
No real hardware, no MCE, no OpenShift required.

---

## Prerequisites

| Tool | Purpose |
|------|---------|
| `docker` + `docker compose` | Postgres |
| `kind` | Local Kubernetes cluster |
| `kubectl` | Cluster interaction |
| `go 1.22+` | Build + run manager and mock servers |
| `psql` | Query the store directly |

---

## Component map

| Component | Substitute |
|-----------|-----------|
| Postgres | `docker compose up -d` (auto-applies schema + seed) |
| k8s API (InventoryRecord, HostClaim CRDs) | kind cluster |
| Agent CRD (assisted-service) | Stub CRD in `config/test/crds/` |
| NodePool CRD (HyperShift) | Stub CRD in `config/test/crds/` |
| BareMetalHost CRD (Metal3) | Stub CRD in `config/test/crds/` |
| OME REST API | `go run ./hack/mock/ome` (`:8081`) — target for `collectors/ome.py` |
| Intersight PVA REST API | `go run ./hack/mock/intersight` (`:8082`) — target for `collectors/cisco_intersight.py` |
| UCS Central XML API | `go run ./hack/mock/ucscentral` (`:8083`) — target for `collectors/ucscentral.py` |
| Redfish / BMC | [sushy-emulator](https://docs.openstack.org/sushy-tools/latest/user/dynamic-emulator.html) (external, not included) |

---

## Quick start

```bash
# 1. One-time: create kind cluster + apply CRDs
make dev-setup

# 2. Start Postgres (schema + seed loaded automatically)
make dev-store

# 3. Apply test InventoryRecords, Agents, NodePool, HostClaim
make dev-samples

# 4. Run the manager (IR reconciler writes declared fields to store on first reconcile)
make dev-run MCE=dev

# 5. Optionally simulate hardware facts from a Python collector:
#    (the IR reconciler handles declared fields; Python collectors write hardware facts)
psql postgres://postgres:fleet@localhost/fleet <<'SQL'
UPDATE host_inventory SET vendor='Dell', model='PowerEdge R750', cores=56, ram_gib=512
WHERE service_tag IN ('SRV001','SRV002');
SQL

# 6. Check store state
make dev-status
```

---

## Component-by-component

### 1. Postgres store (standalone — no k8s needed)

Tests the schema, views, and store Go interface in isolation.

```bash
make dev-store

# Apply schema manually if not using docker compose auto-init:
psql postgres://postgres:fleet@localhost/fleet -f db/schema.sql

# Insert a host directly and verify views:
psql postgres://postgres:fleet@localhost/fleet <<'SQL'
INSERT INTO host_inventory (service_tag, site, class, vendor, model, cores, ram_gib, segment)
VALUES ('TEST01', 'dc1', 'a', 'Dell', 'R750', 56, 512, 'vlan-100');

INSERT INTO host_lease (service_tag, owner_mce, state)
VALUES ('TEST01', 'dev', 'Owned');

SELECT * FROM host_capacity;
SELECT * FROM host_eligible_mce;
SQL

# Test maintenance exclusion:
psql postgres://postgres:fleet@localhost/fleet <<'SQL'
INSERT INTO host_state (service_tag, phase) VALUES ('TEST01', 'maintenance')
ON CONFLICT (service_tag) DO UPDATE SET phase = 'maintenance';

-- available should now be 0:
SELECT service_tag, available, maintenance FROM host_capacity;

-- Restore:
DELETE FROM host_state WHERE service_tag = 'TEST01';
SQL
```

---

### 2. Controllers (InventoryRecord projector + HostClaim reconciler)

Tests the actual Go controller logic against real k8s objects and real Postgres.

```bash
# Cluster and store must be up (steps 1+2 above)
make dev-samples

make dev-run MCE=dev
# Manager logs should show: postgres store connected, reconciling SRV001/SRV002
# The IR reconciler writes declared spec fields (site/segment/class/bmc) to the store
# on the first reconcile — no identity gate, no kubectl patch needed.

# Verify projector wrote to store:
psql postgres://postgres:fleet@localhost/fleet -c "SELECT * FROM host_inventory;"
psql postgres://postgres:fleet@localhost/fleet -c "SELECT * FROM host_lease;"

# Verify HostClaim reconciler sized the NodePool:
kubectl get nodepool test-cluster-workers -o jsonpath='{.spec.replicas}'
# Expected: 2

# Check claim status:
kubectl get hostclaim test-claim -o yaml
# status.phase should be Pending (binding in progress — Agents aren't really binding)
```

---

### 3. Allocation write-back

Tests that the projector detects Agent binding and writes to `host_allocation`.

```bash
# Simulate SRV001 being bound to test-cluster:
kubectl apply -f config/test/samples/bind-srv001.yaml

# Wait up to 30s (projector poll interval), then:
psql postgres://postgres:fleet@localhost/fleet -c "SELECT * FROM host_allocation;"
# Expected: one row for SRV001 with hosted_cluster=test-cluster

# Check InventoryRecord status mirror:
kubectl get inventoryrecord SRV001 -o jsonpath='{.status.allocation}'

# Undo (simulate unbind):
kubectl apply -f config/test/samples/agents.yaml
# host_allocation row should disappear within 30s
```

---

### 4. Maintenance-aware availability

Tests that maintenance hosts are excluded from the available count.

```bash
# With SRV001 and SRV002 in the store (in_service by default):
psql postgres://postgres:fleet@localhost/fleet \
  -c "SELECT available FROM host_capacity WHERE class='a';"
# Expected: 2

# Put SRV001 into maintenance:
psql postgres://postgres:fleet@localhost/fleet \
  -c "INSERT INTO host_state (service_tag, phase) VALUES ('SRV001','maintenance') ON CONFLICT (service_tag) DO UPDATE SET phase='maintenance';"

# Check available count (store + claim reconciler will now see 1):
psql postgres://postgres:fleet@localhost/fleet \
  -c "SELECT available, maintenance FROM host_capacity WHERE class='a';"
# Expected: available=1, maintenance=1

# HostClaim with count=2 should now report Unsatisfiable (only 1 available):
kubectl get hostclaim test-claim -o jsonpath='{.status.unsatisfiableReason}'

# Restore:
psql postgres://postgres:fleet@localhost/fleet \
  -c "DELETE FROM host_state WHERE service_tag='SRV001';"
```

---

### 5. OME collector mock

Serves the OpenManage Enterprise REST endpoints.
Run this when testing the `collectors/ome.py` Python collector.

```bash
make mock-ome
# Listening on :8081

# Verify manually:
curl -s http://localhost:8081/api/DeviceService/Devices | jq '.value[].DeviceServiceTag'
# Expected: "SRV001", "SRV002"

curl -s http://localhost:8081/api/DeviceService/Devices/1001/InventoryDetails | jq '.InventoryInfo[].InventoryType'
# Expected: serverProcessors, serverMemoryInfo, serverNetworkInterfaces, serverStorageDiskView

# Topology (Connection View via LLDP neighbour):
curl -s http://localhost:8081/api/DeviceService/Devices/1001/InventoryDetails \
  | jq '.InventoryInfo[] | select(.InventoryType=="serverNetworkInterfaces") | .InventoryData[].Ports[0] | {mac:.CurrentMacAddress, neighbour:.partnerPortDescription}'
# Expected: mac=aa:bb:cc:dd:ee:01, neighbour=leaf-01:Ethernet1/1
```

Seed data: SRV001 + SRV002 (Dell PowerEdge R750, 2×28 cores, 512 GiB RAM, 2×960 GiB SSD).

---

### 6. Intersight collector mock

Serves the Intersight PVA REST endpoints.
Run this when testing the `collectors/cisco_intersight.py` Python collector.

```bash
make mock-intersight
# Listening on :8082

# Verify:
curl -s http://localhost:8082/api/v1/compute/RackUnits | jq '.Results[].Serial'
# Expected: "SRV003"

curl -s http://localhost:8082/api/v1/compute/Blades | jq '.Count'
# Expected: 0

curl -s http://localhost:8082/api/v1/adapter/HostEthInterfaces | jq '.Results[] | {mac:.MacAddress, peerDn:.PeerInterfaceDn}'
# Expected: mac=aa:bb:cc:dd:ee:03, peerDn=sys/fex-1/phys/slot-1/port-1
```

Seed data: SRV003 (Cisco UCSC-C240-M6SN, 2×28 cores, 512 GiB RAM). Auth (HMAC) is not enforced by the mock — any request is accepted.

---

### 7. UCSM collector mock

Serves the UCS Manager XML API (`POST /nuova`).
Run this when testing the `collectors/ucscentral.py` Python collector.

```bash
make mock-ucscentral
# Listening on :8083

# Login:
curl -s -X POST http://localhost:8083/nuova \
  -H 'Content-Type: text/xml' \
  -d '<aaaLogin inName="admin" inPassword="password"/>'
# Expected: <aaaLoginResponse outCookie="MOCK-COOKIE-123" .../>

# List rack units:
curl -s -X POST http://localhost:8083/nuova \
  -H 'Content-Type: text/xml' \
  -d '<configResolveClass cookie="MOCK-COOKIE-123" classId="computeRackUnit" inHierarchical="false"/>'
# Expected: <computeRackUnit ... serial="SRV004" .../>

# Fabric paths (topology):
curl -s -X POST http://localhost:8083/nuova \
  -H 'Content-Type: text/xml' \
  -d '<configResolveClass cookie="MOCK-COOKIE-123" classId="fabricPathEp" inHierarchical="false"/>'
# Expected: two fabricPathEp entries (switchId=A and switchId=B)
```

Seed data: SRV004 (Cisco UCSC-C240-M6SN, 2×28 cores, 512 GiB RAM, 2×960 GiB SSD). Supported classIds: `computeRackUnit`, `computeBlade`, `processorUnit`, `memoryUnit`, `adaptorEthInterface`, `fabricPathEp`, `storageLocalDisk`.

---

### 8. Redfish / BMC

No mock included. Use [sushy-tools sushy-emulator](https://docs.openstack.org/sushy-tools/latest/user/dynamic-emulator.html) which emulates a Redfish BMC backed by libvirt VMs.

```bash
pip install sushy-tools
sushy-emulator --port 8000 --libvirt-uri qemu:///system
# BMC at http://localhost:8000/redfish/v1/
```

Point the `redfish` collector at `http://localhost:8000` with any username/password.

---

## Teardown

```bash
make dev-teardown
```

Deletes the kind cluster and removes docker compose volumes (including Postgres data).

---

## Seed data summary

| ServiceTag | Source | Class | Site | Segment | Mock server |
|-----------|--------|-------|------|---------|-------------|
| SRV001 | ome | a | dc1 | vlan-100 | `:8081` |
| SRV002 | ome | a | dc1 | vlan-100 | `:8081` |
| SRV003 | intersight | b | dc1 | vlan-200 | `:8082` |
| SRV004 | ucscentral | — | — | — | `:8083` |

`mce_reach` seed: MCE `dev` serves `dc1` on `vlan-100` and `vlan-200`, so all four hosts are eligible for the `dev` MCE.
