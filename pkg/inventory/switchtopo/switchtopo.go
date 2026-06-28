// Package switchtopo implements the inventory.Collector that produces ONLY the
// topology slice: server NIC -> leaf name/port/mgmt-IP.
//
// Authoritative source is the switch side, not the BMC: pull each leaf's
// LLDP-neighbor table and MAC-address table (gNMI / NETCONF / SNMP) and join on
// MAC against the NICs already in inventory. This avoids trusting every BMC's
// iDRAC Connection View (which only yields chassis-id + port, often a MAC, and
// no mgmt IP) and works uniformly across Dell and Cisco.
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
