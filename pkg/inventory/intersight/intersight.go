// Package intersight implements the inventory.Collector for Cisco hardware
// managed by Intersight Private Virtual Appliance (PVA).
//
// Auth: every request is HMAC-SHA256 signed using APIKeyID + APIKeyPEM.
// The signature covers method, path, date, and body digest. TODO: implement
// the signer (see Intersight API auth docs for the exact canonical form).
//
// THE GOTCHA: Intersight abstracts hardware behind service profiles. Follow
// the profile -> physical blade binding (compute.Blade Moid on the associated
// profile) to get real hardware facts. Do NOT read the logical server as if
// it were physical.
package intersight

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
		c = http.DefaultClient // TODO: inject client with internal CA bundle
	}
	return &Collector{baseURL: baseURL, creds: creds, http: c}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceIntersight }

func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	// 1) Physical units:
	//    GET /api/v1/compute/Blades      -> blades (Serial, Model, NumCpus, TotalMemory)
	//    GET /api/v1/compute/RackUnits   -> rack servers (same fields)
	// 2) Per unit, expand inventory:
	//      /api/v1/processor/Units?$filter=Parent.Moid eq '<moid>'  -> sockets/cores/model
	//      /api/v1/memory/Units?$filter=...                          -> RAMGiB
	//      /api/v1/storage/PhysicalDisks?$filter=...                 -> disks (size/type/model)
	//      /api/v1/adapter/HostEthInterfaces?$filter=...             -> NIC MACs
	// 3) topology (pre-boot, no switch creds):
	//    Each HostEthInterface carries PeerInterfaceDn = fabric interconnect port DN.
	//    Resolve DN to: LeafName (fabric interconnect hostname), LeafPort (slot/port),
	//    LeafMgmt (fabric interconnect mgmt IP via /api/v1/network/ElementSummaries).
	//    Map to TopologyLink{NICMac, LeafName, LeafPort, LeafMgmt}.
	//    For blade chassis: fabric interconnect is the leaf; port is the IOM slot/port.
	panic("not implemented: Intersight REST enumerate + map -> v1alpha1.DiscoveredInventory")
}

// compile-time check
var _ inventory.Collector = (*Collector)(nil)
