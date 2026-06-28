// Package store is the central fleet store: the single shared write point.
// It holds the ownership lease (single-writer CAS), the aggregated inventory,
// and the capacity view. It is deliberately off the data path — load is
// O(state changes), not provisioning bandwidth.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrLeaseConflict is returned when a CAS transition's guard (state/generation)
// did not match — i.e. someone else changed the lease first. Callers re-read
// and retry; they MUST NOT force the transition.
var ErrLeaseConflict = errors.New("lease transition conflict")

type LeaseState string

const (
	LeaseOwned     LeaseState = "Owned"
	LeaseReleasing LeaseState = "Releasing"
	LeaseFree      LeaseState = "Free"
	LeaseClaiming  LeaseState = "Claiming"
)

// Lease is the authoritative ownership record for one host.
type Lease struct {
	ServiceTag string
	OwnerMCE   string
	State      LeaseState
	Generation int64
}

// AnyGeneration disables the generation guard on a Transition (state-only CAS).
const AnyGeneration int64 = -1

// LeaseStore owns the single-writer guarantee.
type LeaseStore interface {
	Get(ctx context.Context, serviceTag string) (*Lease, error)

	// Transition performs a guarded compare-and-swap: it only succeeds if the
	// current row matches expectState (and expectGen, unless AnyGeneration).
	// On success it sets the new state/owner and bumps generation.
	// On guard mismatch it returns ErrLeaseConflict.
	Transition(ctx context.Context, serviceTag string,
		expectState LeaseState, expectGen int64,
		newState LeaseState, newOwner string) (*Lease, error)
}

// HostFact is the inventory row a collector pushes.
type HostFact struct {
	ServiceTag string
	Site       string
	Class      string
	Vendor     string
	Model      string
	Cores      int32
	RAMGiB     int64
	StorageGiB int64
	Segment    string
	BMCAddress string
	BMCType    string
}

// Allocation is the consumption outcome; nil clears it (host becomes free).
type Allocation struct {
	HostedCluster string
	NodePool      string
	NodeName      string
	ClaimRef      string
}

type InventoryStore interface {
	UpsertHost(ctx context.Context, f HostFact) error
	SetAllocation(ctx context.Context, serviceTag string, a *Allocation) error
}

// HostPhase is the operational lifecycle of a host, independent of allocation.
// A maintenance/spare host still has a BMH in its home MCE (the operational
// grip); this governs whether it counts as claimable/spare fleet-wide.
type HostPhase string

const (
	PhaseDiscovered      HostPhase = "discovered"
	PhaseInService       HostPhase = "in_service"
	PhaseMaintenance     HostPhase = "maintenance"
	PhaseDecommissioning HostPhase = "decommissioning"
)

// LifecycleStore governs the grip on hosts not in a cluster.
type LifecycleStore interface {
	// SetHostPhase records the operational phase. The home-MCE controller
	// reflects it onto the BMH (e.g. power off / Metal3 maintenance) so the
	// physical host matches.
	SetHostPhase(ctx context.Context, serviceTag string, phase HostPhase, note string) error

	// EligibleMCEs lists MCEs that could adopt this host, by provisioning reach
	// (segment match always; Redfish hosts also roam within their site). This is
	// the "which MCE can I put it in" answer for a discovered host.
	EligibleMCEs(ctx context.Context, serviceTag string) ([]string, error)
}

// CapacityRow is one (site, class, owner_mce) bucket of the capacity view.
type CapacityRow struct {
	Site      string
	Class     string
	OwnerMCE  string
	Total     int
	Available int
	Allocated int
	InTransit int
}

// CapacityFilter narrows the capacity query; empty fields are wildcards.
type CapacityFilter struct {
	Site     string
	Class    string
	OwnerMCE string
}

type CapacityStore interface {
	Capacity(ctx context.Context, f CapacityFilter) ([]CapacityRow, error)
}

// Reservation is a HOLDING: N hosts of a class earmarked but not bound to a
// cluster. Site empty = whole region. Soft by default (counts against headroom);
// Hard pins specific hosts out of the available pool (extension).
type Reservation struct {
	ID        string
	Class     string
	Count     int
	Site      string // "" = entire region
	Purpose   string
	Hard      bool
	ExpiresAt *time.Time // nil = no expiry
}

type ReservationStore interface {
	UpsertReservation(ctx context.Context, r Reservation) error
	DeleteReservation(ctx context.Context, id string) error
	ListReservations(ctx context.Context) ([]Reservation, error)
}

// Headroom is the per-class regional planning row from region_headroom.
// FreeHeadroom < 0 (Shortage true) means active holdings over-commit the spare
// pool — a forecast shortage for that class.
type Headroom struct {
	Class        string
	Total        int
	Allocated    int
	Maintenance  int
	Discovered   int
	InTransit    int
	Spare        int
	Reserved     int
	FreeHeadroom int
	Shortage     bool
}

type ForecastStore interface {
	// RegionHeadroom returns headroom per class; class "" = all classes.
	RegionHeadroom(ctx context.Context, class string) ([]Headroom, error)
}

// Store is the full surface the controllers and the API consume.
type Store interface {
	LeaseStore
	InventoryStore
	LifecycleStore
	CapacityStore
	ReservationStore
	ForecastStore
}

// ---- convenience helpers built on Transition --------------------------------

// Acquire claims a Free host for mce (Free -> Owned). Used at enrollment and at
// the end of a move handoff.
func Acquire(ctx context.Context, s LeaseStore, serviceTag, mce string) (*Lease, error) {
	return s.Transition(ctx, serviceTag, LeaseFree, AnyGeneration, LeaseOwned, mce)
}

// BeginRelease moves an owned host toward release (Owned -> Releasing). The
// move controller does the teardown, then FreeLease.
func BeginRelease(ctx context.Context, s LeaseStore, serviceTag, mce string, gen int64) (*Lease, error) {
	return s.Transition(ctx, serviceTag, LeaseOwned, gen, LeaseReleasing, mce)
}

// FreeLease releases a host after verified teardown (Releasing -> Free).
func FreeLease(ctx context.Context, s LeaseStore, serviceTag string, gen int64) (*Lease, error) {
	return s.Transition(ctx, serviceTag, LeaseReleasing, gen, LeaseFree, "")
}
