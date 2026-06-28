package controller

import (
	"context"
	"errors"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/store"
)

// InventoryRecordReconciler projects InventoryRecord status into Postgres.
// It is the single write path from k8s state into the central fleet store.
//
// Two responsibilities per reconcile:
//  1. UpsertHost — keeps host_inventory current with discovered facts.
//  2. Acquire    — on first enrollment (lease is Free), claims the host for the
//     home MCE (Free → Owned). Idempotent: ErrLeaseConflict means already owned.
type InventoryRecordReconciler struct {
	client.Client
	Store store.Store
	// MCE is the fallback owner when spec.placement.homeMce is unset.
	MCE string
}

// +kubebuilder:rbac:groups=inventory.example.io,resources=inventoryrecords,verbs=get;list;watch

func (r *InventoryRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec v1alpha1.InventoryRecord
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Wait until a collector has written identity — not inspected yet.
	if rec.Status.Identity == nil {
		return ctrl.Result{}, nil
	}

	f := store.HostFact{
		ServiceTag: rec.Spec.ServiceTag,
		Site:       rec.Spec.Placement.Site,
		Class:      rec.Status.Class,
		Vendor:     rec.Status.Identity.Vendor,
		Model:      rec.Status.Identity.Model,
		Segment:    rec.Spec.Network.Segment,
		BMCAddress: rec.Spec.BMC.Address,
		BMCType:    string(rec.Spec.BMC.Type),
	}
	if rec.Status.Compute != nil {
		f.Cores = rec.Status.Compute.CoresTotal
		f.RAMGiB = rec.Status.Compute.RAMGiB
	}
	if rec.Status.Storage != nil {
		f.StorageGiB = rec.Status.Storage.TotalGiB
	}

	if err := r.Store.UpsertHost(ctx, f); err != nil {
		return ctrl.Result{}, fmt.Errorf("upsert host %s: %w", f.ServiceTag, err)
	}

	// Acquire for home MCE on first enrollment. UpsertHost inserts the lease row
	// as Free; if it's still Free, this MCE owns it. ErrLeaseConflict = already
	// claimed by another writer — not an error.
	homeMCE := rec.Spec.Placement.HomeMCE
	if homeMCE == "" {
		homeMCE = r.MCE
	}
	lease, err := r.Store.Get(ctx, f.ServiceTag)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get lease %s: %w", f.ServiceTag, err)
	}
	if lease != nil && lease.State == store.LeaseFree {
		if _, err := store.Acquire(ctx, r.Store, f.ServiceTag, homeMCE); err != nil && !errors.Is(err, store.ErrLeaseConflict) {
			return ctrl.Result{}, fmt.Errorf("acquire lease %s: %w", f.ServiceTag, err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *InventoryRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.InventoryRecord{}).
		Complete(r)
}
