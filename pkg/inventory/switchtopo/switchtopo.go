// Package switchtopo is SUPERSEDED and not wired.
//
// Topology is now sourced from the BMC aggregators: OME via iDRAC Connection
// View (per-port LLDP, available pre-boot) and Intersight via fabric port
// mapping. Switch-side MAC/LLDP poll is not used — it requires switch creds,
// doesn't work on powered-off hosts, and adds no value given the BMC path.
package switchtopo

import (
	"context"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory"
)

// Leaf describes one switch to poll.
type Leaf struct {
	Name   string
	MgmtIP string
	Vendor string // nexus | dellos10 | ... (selects the gNMI/SNMP dialect)
	Creds  inventory.Credentials
}

type Collector struct {
	leaves []Leaf
	// macToNIC resolves a MAC to its owning host service tag + NIC name. The
	// controller injects this from current InventoryRecord status (the NICs the
	// hardware collectors already discovered), so topology joins cleanly.
	macToNIC func(mac string) (serviceTag, nicName string, ok bool)
}

func New(leaves []Leaf, macToNIC func(string) (string, string, bool)) *Collector {
	return &Collector{leaves: leaves, macToNIC: macToNIC}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceSwitch }

func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	// For each leaf:
	//   - read LLDP neighbors (gNMI: openconfig-lldp; or LLDP-MIB via SNMP)
	//   - read the MAC address table (which MAC is learned on which port)
	// Then for each learned MAC, resolve it to a host via c.macToNIC and append a
	// TopologyLink{NICMac, LeafName, LeafPort, LeafMgmt}. Group links by host and
	// emit one Observation per host so the merge writes a complete topology slice.
	//
	// Bonded NICs: a host MAC may appear on two leaves (MLAG/VLT) — keep both
	// links; that's how you capture the dual-homed leaf pair.
	panic("not implemented: poll leaves, join MAC->NIC, emit per-host topology")
}

// compile-time check
var _ inventory.Collector = (*Collector)(nil)
