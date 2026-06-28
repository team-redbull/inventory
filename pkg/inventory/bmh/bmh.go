// Package bmh implements the inventory.Collector backed by Metal3
// BareMetalHost objects. This is the PRIMARY discovered-hardware source for any
// host Metal3 manages: Ironic introspection already wrote a normalized,
// vendor-neutral inventory into status.hardwareDetails at registration.
//
// VERSION-INDEPENDENCE: read via unstructured so this binary does NOT import the
// metal3-io/baremetal-operator Go module (which drifts across MCE releases). The
// metal3.io/v1alpha1 apiVersion and the fields below are stable across MCE 2.7
// and 2.10. OME/UCS/redfish collectors enrich fields BMH lacks; topology comes
// from OME (iDRAC Connection View) or Intersight (fabric port mapping).
package bmh

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory"
)

var bmhListGVK = schema.GroupVersionKind{Group: "metal3.io", Version: "v1alpha1", Kind: "BareMetalHostList"}

type Collector struct {
	c         client.Client
	namespace string // empty = all namespaces
}

func New(c client.Client, namespace string) *Collector {
	return &Collector{c: c, namespace: namespace}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceBMH }

func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(bmhListGVK)
	opts := []client.ListOption{}
	if c.namespace != "" {
		opts = append(opts, client.InNamespace(c.namespace))
	}
	if err := c.c.List(ctx, list, opts...); err != nil {
		return nil, err
	}

	out := make([]inventory.Observation, 0, len(list.Items))
	for i := range list.Items {
		obj := list.Items[i].Object
		if _, found, _ := unstructured.NestedMap(obj, "status", "hardwareDetails"); !found {
			// Not inspected yet; it'll appear on a later reconcile.
			continue
		}
		out = append(out, inventory.Observation{
			Key:       key(obj),
			Inventory: toInventory(obj),
		})
	}
	return out, nil
}

// small nested accessors (k8s json decoder gives int64 for integers).
func str(m map[string]interface{}, f ...string) string {
	s, _, _ := unstructured.NestedString(m, f...)
	return s
}
func i64(m map[string]interface{}, f ...string) int64 {
	i, _, _ := unstructured.NestedInt64(m, f...)
	return i
}
func num(m map[string]interface{}, f ...string) float64 {
	if v, ok, _ := unstructured.NestedFloat64(m, f...); ok {
		return v
	}
	if v, ok, _ := unstructured.NestedInt64(m, f...); ok {
		return float64(v)
	}
	return 0
}

// key correlates the BMH to an InventoryRecord. Prefer the hardware serial
// (== Dell service tag / Cisco serial) so it matches spec.serviceTag from
// GitOps; fall back to the BMH name.
func key(obj map[string]interface{}) string {
	if s := str(obj, "status", "hardwareDetails", "systemVendor", "serialNumber"); s != "" {
		return s
	}
	return str(obj, "metadata", "name")
}

func toInventory(obj map[string]interface{}) *v1alpha1.DiscoveredInventory {
	hw := []string{"status", "hardwareDetails"}
	bmcAddr := str(obj, "spec", "bmc", "address")

	inv := &v1alpha1.DiscoveredInventory{
		Identity: &v1alpha1.Identity{
			ServiceTag: str(obj, append(hw, "systemVendor", "serialNumber")...),
			Vendor:     str(obj, append(hw, "systemVendor", "manufacturer")...),
			Model:      str(obj, append(hw, "systemVendor", "productName")...),
		},
		BMC: &v1alpha1.BMCInfo{
			Address: bmcAddr,
			Type:    bmcTypeFromAddress(bmcAddr),
			// BMH gives BIOS version, not BMC firmware — leave for enrichment.
		},
		Compute: &v1alpha1.Compute{
			CPUModel:   str(obj, append(hw, "cpu", "model")...),
			CoresTotal: int32(i64(obj, append(hw, "cpu", "count")...)),
			RAMGiB:     i64(obj, append(hw, "ramMebibytes")...) / 1024,
		},
	}

	// Storage
	st := &v1alpha1.Storage{}
	disks, _, _ := unstructured.NestedSlice(obj, append(hw, "storage")...)
	for _, raw := range disks {
		d, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		gib := i64(d, "sizeBytes") / (1024 * 1024 * 1024)
		st.Disks = append(st.Disks, v1alpha1.Disk{
			Type:    diskType(d),
			SizeGiB: gib,
			Model:   str(d, "model"),
			WWN:     str(d, "wwn"),
		})
		st.TotalGiB += gib
	}
	st.DiskCount = int32(len(st.Disks))
	inv.Storage = st

	// Network — NIC MACs are the join key OME/UCS use to populate topology.
	nics, _, _ := unstructured.NestedSlice(obj, append(hw, "nic")...)
	for _, raw := range nics {
		n, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		inv.Network = append(inv.Network, v1alpha1.NIC{
			Name:     str(n, "name"),
			MAC:      str(n, "mac"),
			SpeedMbs: int64(num(n, "speedGbps") * 1000),
		})
	}

	return inv
}

// diskType normalizes the rotational/type hints into ssd|hdd|nvme.
func diskType(d map[string]interface{}) string {
	if t := strings.ToLower(str(d, "type")); t != "" {
		return t // newer Metal3 sets Type directly (HDD/SSD/NVME)
	}
	if rot, _, _ := unstructured.NestedBool(d, "rotational"); rot {
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
// TOPOLOGY NOT POPULATED BY THIS COLLECTOR:
// Ironic introspection can carry per-interface LLDP TLVs if enabled in the
// IPA ramdisk, but BMH status does not surface them. Topology comes from OME
// (iDRAC Connection View per-port LLDP) or Intersight (fabric port mapping).
// BMC-sourced topology is available pre-boot and works without switch creds.
// -------------------------------------------------------------------------
