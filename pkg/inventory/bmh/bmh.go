// Package bmh implements the inventory.Collector backed by Metal3
// BareMetalHost objects. This is the PRIMARY discovered-hardware source for any
// host Metal3 manages: Ironic introspection (the IPA ramdisk) already wrote a
// normalized, vendor-neutral inventory into status.hardwareDetails at
// registration. No BMC API call, no vendor branching — read the CRD in-cluster.
//
// OME/UCS/redfish collectors are then only needed for (a) hosts Metal3 does NOT
// manage and (b) enrichment fields BMH lacks (BMC firmware version, warranty,
// firmware-compliance baselines). Topology still comes from the switch
// collector (or from LLDP captured during introspection — see note below).
package bmh

import (
	"context"
	"strings"

	metal3v1alpha1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory"
)

type Collector struct {
	c         client.Client
	namespace string // empty = all namespaces
}

func New(c client.Client, namespace string) *Collector {
	return &Collector{c: c, namespace: namespace}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceBMH }

func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	var list metal3v1alpha1.BareMetalHostList
	opts := []client.ListOption{}
	if c.namespace != "" {
		opts = append(opts, client.InNamespace(c.namespace))
	}
	if err := c.c.List(ctx, &list, opts...); err != nil {
		return nil, err
	}

	out := make([]inventory.Observation, 0, len(list.Items))
	for i := range list.Items {
		h := &list.Items[i]
		hw := h.Status.HardwareDetails
		if hw == nil {
			// Not inspected yet (registering / inspecting). Skip; it'll appear
			// on a later reconcile once introspection completes.
			continue
		}
		out = append(out, inventory.Observation{
			Key:       key(h),
			Inventory: toInventory(h),
		})
	}
	return out, nil
}

// key correlates the BMH to an InventoryRecord. Prefer the hardware serial
// (== Dell service tag / Cisco serial) so it matches spec.serviceTag set in
// GitOps; fall back to the BMH name.
func key(h *metal3v1alpha1.BareMetalHost) string {
	if h.Status.HardwareDetails != nil {
		if s := h.Status.HardwareDetails.SystemVendor.SerialNumber; s != "" {
			return s
		}
	}
	return h.Name
}

func toInventory(h *metal3v1alpha1.BareMetalHost) *v1alpha1.DiscoveredInventory {
	hw := h.Status.HardwareDetails
	inv := &v1alpha1.DiscoveredInventory{
		Identity: &v1alpha1.Identity{
			ServiceTag: hw.SystemVendor.SerialNumber,
			Vendor:     hw.SystemVendor.Manufacturer,
			Model:      hw.SystemVendor.ProductName,
		},
		BMC: &v1alpha1.BMCInfo{
			Address: h.Spec.BMC.Address,
			Type:    bmcTypeFromAddress(h.Spec.BMC.Address),
			// NOTE: BMH gives BIOS version (hw.Firmware.BIOS.Version), not BMC
			// firmware. Leave FirmwareVersion for the OME/redfish enrichment pass.
		},
		Compute: &v1alpha1.Compute{
			CPUModel: hw.CPU.Model,
			// hw.CPU.Count is logical CPU count; BMH does not expose socket count.
			// Treat as cores for now, refine via enrichment if you need sockets.
			CoresTotal: int32(hw.CPU.Count),
			RAMGiB:     int64(hw.RAMMebibytes) / 1024,
		},
	}

	// Storage
	st := &v1alpha1.Storage{}
	for _, d := range hw.Storage {
		gib := int64(d.SizeBytes) / (1024 * 1024 * 1024)
		st.Disks = append(st.Disks, v1alpha1.Disk{
			Type:    diskType(d),
			SizeGiB: gib,
			Model:   d.Model,
			WWN:     d.WWN,
		})
		st.TotalGiB += gib
	}
	st.DiskCount = int32(len(st.Disks))
	inv.Storage = st

	// Network — NIC MACs are the join key the switch collector needs.
	for _, n := range hw.NIC {
		inv.Network = append(inv.Network, v1alpha1.NIC{
			Name:     n.Name,
			MAC:      n.MAC,
			SpeedMbs: int64(n.SpeedGbps) * 1000,
		})
	}

	return inv
}

// diskType normalizes the rotational/type hints into ssd|hdd|nvme.
func diskType(d metal3v1alpha1.Storage) string {
	if t := strings.ToLower(string(d.Type)); t != "" {
		return t // newer Metal3 sets Type directly (HDD/SSD/NVME)
	}
	if d.Rotational {
		return "hdd"
	}
	return "ssd"
}

// bmcTypeFromAddress maps the Metal3 BMC URL scheme to our BMCType.
func bmcTypeFromAddress(addr string) v1alpha1.BMCType {
	scheme := addr
	if i := strings.Index(addr, "://"); i >= 0 {
		scheme = addr[:i]
	}
	switch {
	case strings.HasPrefix(scheme, "idrac"):
		return v1alpha1.BMCTypeIDRAC
	case strings.HasPrefix(scheme, "redfish"), strings.HasPrefix(scheme, "ipmi"), strings.HasPrefix(scheme, "irmc"):
		return v1alpha1.BMCTypeGeneric
	default:
		return v1alpha1.BMCTypeGeneric
	}
}

// compile-time check
var _ inventory.Collector = (*Collector)(nil)

// -------------------------------------------------------------------------
// TOPOLOGY-FROM-INTROSPECTION (alternative to the switch collector):
// If you enable LLDP collection in the IPA ramdisk, Ironic's introspection data
// carries per-interface LLDP TLVs (chassis id, port id, sometimes sysName /
// mgmt address). That gives you leaf links captured at inspect time without
// switch credentials — but it's point-in-time and inherits the same chassis-id
// caveats (often a MAC, not a hostname). The live switch-side poll
// (pkg/inventory/switchtopo) stays the authoritative source; introspection LLDP
// is a credential-free complement. Note this data is NOT in BMH status — read
// it from the stored Ironic inspection data / HardwareData object.
// -------------------------------------------------------------------------
