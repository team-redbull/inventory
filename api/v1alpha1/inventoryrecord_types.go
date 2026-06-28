// Package v1alpha1 contains the InventoryRecord (a.k.a. Host) and HostClaim APIs.
//
// Design rule: spec holds DECLARED facts (enrollment identity, physical
// placement, BMC coordinates) authored in GitOps. status holds DISCOVERED and
// RUNTIME facts written by the local controller in each MCE: hardware from
// introspection, the reflected ownership/lease, and the allocation outcome.
// A collector reconcile NEVER writes spec, so re-inventory can't clobber
// human-declared data.
//
// The AUTHORITATIVE ownership lease lives in the central store (Postgres), not
// in this CR. status.ownership is only a reflected copy for observability.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelClass is the metadata label carrying a host's class. It must be set on
// the BareMetalHost (so Metal3 hostSelector can match it) and is mirrored onto
// the InventoryRecord for the capacity view. Example: inventory.example.io/class=a
const LabelClass = "inventory.example.io/class"

// BMCType identifies the management controller dialect.
type BMCType string

const (
	BMCTypeIDRAC   BMCType = "idrac"
	BMCTypeCIMC    BMCType = "cimc"
	BMCTypeUCSM    BMCType = "ucsm"
	BMCTypeGeneric BMCType = "generic"
)

// CollectorSource identifies which backend produced a record's status.
type CollectorSource string

const (
	SourceBMH         CollectorSource = "bmh"         // Metal3 BareMetalHost introspection (primary)
	SourceOME         CollectorSource = "ome"         // OpenManage Enterprise — Dell aggregator
	SourceIntersight  CollectorSource = "intersight"  // Intersight PVA — Cisco aggregator (REST/HMAC)
	SourceUCSCentral  CollectorSource = "ucscentral"  // UCS Central — Cisco multi-domain aggregator (XML API, /centralApi/)
	SourceRedfish     CollectorSource = "redfish"     // direct Redfish, whitebox / no aggregator
	SourceSwitch      CollectorSource = "switch"      // superseded: switch-side MAC poll not used; topology comes from BMC (iDRAC Connection View / Intersight fabric)
)

// LeaseState mirrors the central store's ownership state machine.
type LeaseState string

const (
	LeaseOwned     LeaseState = "Owned"
	LeaseReleasing LeaseState = "Releasing"
	LeaseFree      LeaseState = "Free"
	LeaseClaiming  LeaseState = "Claiming"
)

// -------------------------------------------------------------------------
// Spec — declared / desired (GitOps-authored at enrollment)
// -------------------------------------------------------------------------

type InventoryRecordSpec struct {
	// ServiceTag is the stable identity (Dell service tag / Cisco serial) used
	// to correlate this record with discovered hardware and with claims.
	// +kubebuilder:validation:Required
	ServiceTag string `json:"serviceTag"`

	// Source names the collector responsible for primary discovery.
	// Metal3-managed hosts use "bmh"; non-Metal3 hosts use an aggregator/redfish.
	// +kubebuilder:validation:Enum=bmh;ome;intersight;ucscentral;redfish
	Source CollectorSource `json:"source"`

	// Class optionally overrides automatic classification. If empty, the
	// classifier derives the class from the hardware profile and sets the
	// LabelClass label. If set, it wins and is mirrored to the label.
	// +optional
	Class string `json:"class,omitempty"`

	// Placement holds declared PHYSICAL facts. Note: cluster is NOT here — the
	// cluster binding is an allocation outcome in status, not declared.
	Placement Placement `json:"placement,omitempty"`

	// BMC connection coordinates.
	BMC BMCRef `json:"bmc,omitempty"`

	// Network carries the L2 segment used as the move-eligibility gate.
	Network NetworkSpec `json:"network,omitempty"`
}

type Placement struct {
	Site    string `json:"site,omitempty"`
	Rack    string `json:"rack,omitempty"`
	Role    string `json:"role,omitempty"`
	HomeMCE string `json:"homeMce,omitempty"` // default owning MCE at enrollment
}

type BMCRef struct {
	Address string  `json:"address,omitempty"`
	Type    BMCType `json:"type,omitempty"`
	// BootMACAddress is required for the IPMI+PXE method (network boot NIC).
	// +optional
	BootMACAddress string `json:"bootMACAddress,omitempty"`
	// CredentialsRef points at a Secret (created out-of-band via ESO/sealed
	// secret) holding the BMC username/password. Never inline the password.
	CredentialsRef SecretReference `json:"credentialsRef,omitempty"`
}

type SecretReference struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type NetworkSpec struct {
	// Segment is the L2/VLAN the provisioning NIC lives on. An IPMI+PXE host is
	// only movable to an MCE whose provisioning network serves this segment.
	Segment string `json:"segment,omitempty"`
	// LeafHint is an optional operator-supplied leaf name, refined by the
	// switch topology collector.
	LeafHint string `json:"leafHint,omitempty"`
}

// -------------------------------------------------------------------------
// Status — discovered + runtime
// -------------------------------------------------------------------------

type InventoryRecordStatus struct {
	// DiscoveredInventory is the canonical, vendor-neutral model every collector
	// targets. Inlined so the collector return type and the CRD status share one
	// definition.
	DiscoveredInventory `json:",inline"`

	// Class is the effective (possibly derived) class label value.
	Class string `json:"class,omitempty"`

	// Ownership is a REFLECTED copy of the central lease for observability only;
	// the authoritative lease lives in the store.
	Ownership *OwnershipStatus `json:"ownership,omitempty"`

	// Allocation is the current consumption outcome (which cluster/claim/node).
	Allocation *AllocationStatus `json:"allocation,omitempty"`

	Collection CollectionStatus `json:"collection,omitempty"`

	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

type OwnershipStatus struct {
	OwnerMCE   string     `json:"ownerMce,omitempty"`
	LeaseState LeaseState `json:"leaseState,omitempty"`
}

type AllocationStatus struct {
	HostedCluster string `json:"hostedCluster,omitempty"` // HostedCluster currently consuming the host
	NodePool      string `json:"nodePool,omitempty"`      // NodePool it joined
	ClaimRef      string `json:"claimRef,omitempty"`      // namespace/name of the satisfying HostClaim
	NodeName      string `json:"nodeName,omitempty"`      // node name once joined
}

// DiscoveredInventory is what a Collector returns. Topology is populated by
// the BMC aggregator collectors (OME via iDRAC Connection View, UCS/Intersight
// via fabric port mapping) and merged per-collector. A collector that leaves
// Topology nil must not erase an existing topology slice.
// Note: BMC-sourced topology may leave LeafMgmt empty and LeafName as a
// chassis-id MAC rather than a resolved switch name — enrich offline if needed.
type DiscoveredInventory struct {
	Identity *Identity      `json:"identity,omitempty"`
	BMC      *BMCInfo       `json:"bmc,omitempty"`
	Compute  *Compute       `json:"compute,omitempty"`
	Storage  *Storage       `json:"storage,omitempty"`
	Network  []NIC          `json:"network,omitempty"`
	Topology []TopologyLink `json:"topology,omitempty"`
}

type Identity struct {
	ServiceTag string `json:"serviceTag,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Vendor     string `json:"vendor,omitempty"`
	Model      string `json:"model,omitempty"`
}

type BMCInfo struct {
	Address         string  `json:"address,omitempty"`
	Type            BMCType `json:"type,omitempty"`
	FirmwareVersion string  `json:"firmwareVersion,omitempty"`
}

type Compute struct {
	CPUModel   string `json:"cpuModel,omitempty"`
	Sockets    int32  `json:"sockets,omitempty"`
	CoresTotal int32  `json:"coresTotal,omitempty"`
	RAMGiB     int64  `json:"ramGiB,omitempty"`
}

type Disk struct {
	// Type: ssd | hdd | nvme
	Type    string `json:"type,omitempty"`
	SizeGiB int64  `json:"sizeGiB,omitempty"`
	Model   string `json:"model,omitempty"`
	WWN     string `json:"wwn,omitempty"`
}

type Storage struct {
	Disks     []Disk `json:"disks,omitempty"`
	TotalGiB  int64  `json:"totalGiB,omitempty"`
	DiskCount int32  `json:"diskCount,omitempty"`
}

type NIC struct {
	Name     string `json:"name,omitempty"`
	MAC      string `json:"mac,omitempty"`
	SpeedMbs int64  `json:"speedMbs,omitempty"`
}

// TopologyLink maps one server NIC to the leaf port it lands on. Correlated by
// MAC against switch LLDP-neighbour / MAC-address tables.
type TopologyLink struct {
	NICMac   string `json:"nicMac,omitempty"`
	LeafName string `json:"leafName,omitempty"`
	LeafPort string `json:"leafPort,omitempty"`
	LeafMgmt string `json:"leafMgmt,omitempty"`
}

type CollectionStatus struct {
	Source      CollectorSource `json:"source,omitempty"`
	LastSuccess *metav1.Time    `json:"lastSuccess,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
	RawRef      string          `json:"rawRef,omitempty"`
}

// -------------------------------------------------------------------------
// Boilerplate
// -------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=inv
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.status.class`
// +kubebuilder:printcolumn:name="Vendor",type=string,JSONPath=`.status.identity.vendor`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.identity.model`
// +kubebuilder:printcolumn:name="RAM-GiB",type=integer,JSONPath=`.status.compute.ramGiB`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.status.ownership.ownerMce`
// +kubebuilder:printcolumn:name="Lease",type=string,JSONPath=`.status.ownership.leaseState`
// +kubebuilder:printcolumn:name="HostedCluster",type=string,JSONPath=`.status.allocation.hostedCluster`

// InventoryRecord is the canonical record for one bare-metal host.
type InventoryRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InventoryRecordSpec   `json:"spec,omitempty"`
	Status InventoryRecordStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InventoryRecordList contains a list of InventoryRecord.
type InventoryRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InventoryRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InventoryRecord{}, &InventoryRecordList{})
}
