package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory/bmh"
	"example.io/inventory/pkg/store"
)

// agentBMHLabel is set by BMAC on the Agent when it pairs with a BMH.
// Value = BMH name = serviceTag in this system.
const agentBMHLabel = "agent-install.openshift.io/bmh"

var (
	agentListGVK = schema.GroupVersionKind{
		Group:   "agent-install.openshift.io",
		Version: "v1beta1",
		Kind:    "AgentList",
	}
	bmhGVK = schema.GroupVersionKind{
		Group:   "metal3.io",
		Version: "v1alpha1",
		Kind:    "BareMetalHost",
	}
)

// InventoryRecordReconciler projects InventoryRecord status into Postgres.
// It is the single write path from k8s state into the central fleet store.
//
// Three responsibilities per reconcile:
//  1. UpsertHost     — keeps host_inventory current with discovered facts.
//  2. Acquire        — on first enrollment (lease is Free), claims the host for
//     the home MCE (Free → Owned). Idempotent: ErrLeaseConflict means already owned.
//  3. SetAllocation  — mirrors Agent binding state into the store and into
//     status.allocation. Polled every 30s to catch bind/unbind without an Agent watch.
type InventoryRecordReconciler struct {
	client.Client
	Store store.Store
	// MCE is the fallback owner when spec.placement.homeMce is unset.
	MCE string
	// AgentNamespace is where Agents live; empty = search all namespaces.
	AgentNamespace string
}

// +kubebuilder:rbac:groups=inventory.example.io,resources=inventoryrecords,verbs=get;list;watch
// +kubebuilder:rbac:groups=inventory.example.io,resources=inventoryrecords/status,verbs=update;patch
// +kubebuilder:rbac:groups=agent-install.openshift.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal3.io,resources=baremetalhosts,verbs=get;list;watch

func (r *InventoryRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec v1alpha1.InventoryRecord
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Enrich status from a co-located BareMetalHost (same name+namespace).
	// By convention the BMH is named after the serviceTag. This runs before the
	// identity nil-check so that a fresh IR gets populated on first BMH inspection.
	r.enrichFromBMH(ctx, &rec)

	// Wait until identity is set — either by the BMH enrichment above, or by an
	// external collector (OME/Intersight/UCS Central) writing directly to Postgres.
	// For the Python-collector path the IR status may stay nil; those collectors
	// write directly to host_inventory and the IR reconciler writes only declared
	// fields (site/segment/class/bmc_*) via UpsertHost with COALESCE guards.
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

	// Allocation write-back: find the Agent BMAC paired to this BMH, and mirror
	// its binding state into the store + status. nil Allocation clears the row.
	alloc, err := r.resolveAllocation(ctx, rec.Spec.ServiceTag)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve allocation %s: %w", rec.Spec.ServiceTag, err)
	}
	if err := r.Store.SetAllocation(ctx, rec.Spec.ServiceTag, alloc); err != nil {
		return ctrl.Result{}, fmt.Errorf("set allocation %s: %w", rec.Spec.ServiceTag, err)
	}

	base := rec.DeepCopy()
	if alloc != nil {
		rec.Status.Allocation = &v1alpha1.AllocationStatus{
			HostedCluster: alloc.HostedCluster,
			NodeName:      alloc.NodeName,
		}
	} else {
		rec.Status.Allocation = nil
	}
	if err := r.Status().Patch(ctx, &rec, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status %s: %w", rec.Name, err)
	}

	// Poll so bind/unbind events on the Agent side are reflected promptly
	// without requiring a cross-namespace Agent watch.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// resolveAllocation looks up the Agent that BMAC paired to this BMH
// (label agent-install.openshift.io/bmh = serviceTag) and returns an Allocation
// if the Agent is bound to a cluster, or nil if unbound / not yet matched.
// NodePool and ClaimRef are not available on the Agent; they remain empty and
// can be enriched by the claim reconciler if needed.
func (r *InventoryRecordReconciler) resolveAllocation(ctx context.Context, serviceTag string) (*store.Allocation, error) {
	agents := &unstructured.UnstructuredList{}
	agents.SetGroupVersionKind(agentListGVK)

	opts := []client.ListOption{
		client.MatchingLabels{agentBMHLabel: serviceTag},
	}
	if r.AgentNamespace != "" {
		opts = append(opts, client.InNamespace(r.AgentNamespace))
	}
	if err := r.Client.List(ctx, agents, opts...); err != nil {
		return nil, err
	}

	for i := range agents.Items {
		obj := agents.Items[i].Object
		clusterName, _, _ := unstructured.NestedString(obj, "spec", "clusterDeploymentName", "name")
		if clusterName == "" {
			continue
		}
		nodeName, _, _ := unstructured.NestedString(obj, "status", "inventory", "hostname")
		return &store.Allocation{
			HostedCluster: clusterName,
			NodeName:      nodeName,
		}, nil
	}
	return nil, nil
}

// enrichFromBMH looks up the BareMetalHost with the same name+namespace as the
// InventoryRecord. If Ironic has completed introspection (status.hardwareDetails
// is present), it merges the hardware into the IR status in-memory. The caller
// must then patch the status to persist it. Errors are non-fatal: if the BMH
// does not exist or has not been inspected yet, enrichment is a no-op.
func (r *InventoryRecordReconciler) enrichFromBMH(ctx context.Context, rec *v1alpha1.InventoryRecord) {
	bmhObj := &unstructured.Unstructured{}
	bmhObj.SetGroupVersionKind(bmhGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: rec.Name, Namespace: rec.Namespace}, bmhObj); err != nil {
		return
	}
	hw, found, _ := unstructured.NestedMap(bmhObj.Object, "status", "hardwareDetails")
	if !found {
		return
	}
	inv := bmh.MapHardwareDetails(hw)
	if inv == nil {
		return
	}
	// Merge into rec.Status — only overwrite nil fields so an explicit status
	// patch from an operator or test harness is not silently discarded.
	if rec.Status.Identity == nil {
		rec.Status.Identity = inv.Identity
	}
	if rec.Status.Compute == nil {
		rec.Status.Compute = inv.Compute
	}
	if rec.Status.Storage == nil {
		rec.Status.Storage = inv.Storage
	}
	if len(rec.Status.Network) == 0 {
		rec.Status.Network = inv.Network
	}
}

func (r *InventoryRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch BareMetalHost changes and reconcile the InventoryRecord with the
	// same name+namespace (by convention BMH name = serviceTag = IR name).
	bmhType := &unstructured.Unstructured{}
	bmhType.SetGroupVersionKind(bmhGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.InventoryRecord{}).
		Watches(bmhType, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{
					{NamespacedName: types.NamespacedName{
						Name:      obj.GetName(),
						Namespace: obj.GetNamespace(),
					}},
				}
			},
		)).
		Complete(r)
}
