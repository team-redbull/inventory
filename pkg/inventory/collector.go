// Package inventory defines the vendor-neutral collector seam.
//
// The architecture is aggregator-first: instead of holding a session to every
// BMC, each Collector talks to one backend that already inventories a fleet
// (OME for Dell, UCS Central / Intersight PVA for Cisco) and returns a list of
// Observations. The redfish collector is the per-host fallback for whitebox
// hardware no aggregator owns. All vendor quirks die inside an adapter; nothing
// downstream knows which vendor a host came from.
package inventory

import (
	"context"

	v1alpha1 "example.io/inventory/api/v1alpha1"
)

// Credentials for a backend. Aggregators use one set each (configured once);
// the redfish collector resolves them per host from the InventoryRecord secret.
type Credentials struct {
	Username string
	Password string
	// Token / API key path for Intersight-style auth (HMAC-signed requests).
	APIKeyID    string
	APIKeyPEM   []byte
	BearerToken string
}

// Observation is one host as seen by a collector, keyed for correlation
// against an InventoryRecord (by service tag / serial).
type Observation struct {
	// Key correlates to InventoryRecord.spec.serviceTag.
	Key string
	// Inventory is the canonical discovered model. OME/UCS collectors populate
	// Topology from BMC (iDRAC Connection View / Intersight fabric mapping).
	Inventory *v1alpha1.DiscoveredInventory
	// Raw optionally carries the untouched vendor payload for audit.
	Raw []byte
}

// Collector is the single contract every vendor adapter satisfies.
type Collector interface {
	// Source identifies the backend (ome, ucs, redfish, switch).
	Source() v1alpha1.CollectorSource

	// List enumerates every host the backend knows about. For aggregators this
	// is one API conversation returning the whole fleet; for redfish the
	// collector is constructed with its target set and loops internally.
	List(ctx context.Context) ([]Observation, error)
}

// Registry holds the active collectors. The controller iterates them on each
// reconcile (or on a timer) and merges their Observations into status.
type Registry struct {
	collectors []Collector
}

func NewRegistry(c ...Collector) *Registry { return &Registry{collectors: c} }

func (r *Registry) Collectors() []Collector { return r.collectors }

// -------------------------------------------------------------------------
// Reconcile-merge contract (implemented in the controller, sketched here so
// the rule travels with the seam):
//
//   obs := collector.List(ctx)                      // per collector
//   for each o in obs:
//     rec := lookupInventoryRecord(o.Key)           // match by service tag
//     if rec == nil { continue }                    // unknown host -> skip/alert
//
//     // OWNERSHIP: each collector writes only its owned fields; nil = don't erase.
//     // OME/UCS write identity/bmc/compute/storage/network/topology (topology from
//     // iDRAC Connection View or Intersight fabric mapping — available pre-boot).
//     // BMH writes identity/bmc/compute/storage/network only (no topology —
//     // Ironic does not surface iDRAC Connection View data).
//     if o.Inventory.Identity  != nil { rec.Status.Identity  = o.Inventory.Identity }
//     if o.Inventory.Compute   != nil { rec.Status.Compute   = o.Inventory.Compute }
//     ... etc ...
//     if o.Inventory.Topology  != nil { rec.Status.Topology  = o.Inventory.Topology }
//
//     // spec.Placement is NEVER written here — it stays GitOps-authoritative.
//     rec.Status.Collection = CollectionStatus{Source: collector.Source(), LastSuccess: now}
//     setReadyCondition(rec)
//     status().Update(rec)
//
// A separate projector watches InventoryRecord and upserts the merged view
// into Postgres for the UI / history / fleet analytics.
// -------------------------------------------------------------------------
