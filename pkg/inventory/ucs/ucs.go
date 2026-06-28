// Package ucs implements the inventory.Collector for the Cisco fleet.
//
// DECISION POINT — confirm which aggregator you run in the air-gap, the API
// differs substantially:
//
//   - Intersight Private Virtual Appliance (PVA): modern REST/JSON, HMAC API-key
//     auth. Hosts appear as compute.Blade / compute.RackUnit objects. Preferred
//     if you have it. This stub targets Intersight.
//   - UCS Central / UCS Manager: XML API (the ucsmsdk dialect). If that's your
//     path, swap the transport for an XML client and resolve managedObjects.
//
// THE GOTCHA: UCS abstracts hardware behind service profiles. "What CPU/RAM does
// logical server X have" resolves through the profile -> physical blade binding
// (lsServer.assignedToDn in UCSM; the associated compute.Blade Moid in
// Intersight). The adapter must follow that association, not read a logical
// server as if it were physical.
package ucs

import (
	"context"
	"net/http"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory"
)

type Collector struct {
	baseURL string // https://intersight.airgap.local
	creds   inventory.Credentials
	http    *http.Client
}

func New(baseURL string, creds inventory.Credentials, c *http.Client) *Collector {
	if c == nil {
		c = http.DefaultClient // TODO: internal CA bundle
	}
	return &Collector{baseURL: baseURL, creds: creds, http: c}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceUCS }

func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	// Intersight path:
	// 1) Physical units:  GET /api/v1/compute/Blades  and  /api/v1/compute/RackUnits
	//    -> Serial, Model, NumCpus, TotalMemory, OperPowerState, Mgmt addr.
	// 2) Per unit, expand the inventory:
	//      /api/v1/processor/Units?$filter=parent...   -> sockets/cores/model
	//      /api/v1/memory/Units / memory/Arrays        -> RAMGiB
	//      /api/v1/storage/PhysicalDisks               -> disks (size/type/model)
	//      /api/v1/adapter/HostEthInterfaces           -> NIC MACs (topology join key)
	// 3) If you care about the LOGICAL view, follow server.Profile -> associated
	//    compute.Blade Moid; do not read the profile as hardware.
	// 4) topology: Intersight fabric port mapping — available pre-boot, no switch creds.
	//    GET /api/v1/network/ElementSummaries or /api/v1/adapter/HostEthInterfaces
	//    -> each interface carries PeerInterfaceDn (fabric interconnect port dn).
	//    Resolve Dn to switch name + port. Map to TopologyLink{NICMac, LeafName,
	//    LeafPort, LeafMgmt}. For blade chassis: fabric interconnect is the leaf;
	//    port is the IOM slot/port. LeafMgmt = fabric interconnect mgmt IP.
	//
	// Auth: Intersight signs each request (HMAC-SHA256 over a digest of method,
	// path, date, body) using APIKeyID + APIKeyPEM. TODO: implement the signer.
	panic("not implemented: Intersight REST enumerate + map -> v1alpha1.DiscoveredInventory")
}

// compile-time check
var _ inventory.Collector = (*Collector)(nil)
