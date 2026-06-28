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
	"example.io/inventory/pkg/inventory/redfish"
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

// InventoryRecordReconciler projects InventoryRecord spec into Postgres and
// mirrors runtime state (lease, allocation) back into status.
//
// Postgres is the single source of truth for hardware facts. This reconciler
// never caches hardware data in status — it writes discovered facts directly
// to the store and reads them back only from there.
//
// Three responsibilities per reconcile:
//  1. UpsertHost     — writes declared spec fields + any Go-discovered hardware
//     facts (BMH introspection, Redfish) directly to host_inventory. Python
//     collectors (OME/Intersight/UCS) write independently on their own schedule.
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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

func (r *InventoryRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec v1alpha1.InventoryRecord
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Discover hardware facts from Go-side sources and write them directly to
	// the store. Status never carries hardware data — Postgres is the single truth.
	hw := r.discoverFacts(ctx, &rec)

	f := store.HostFact{
		ServiceTag: rec.Spec.ServiceTag,
		Site:       rec.Spec.Placement.Site,
		Class:      rec.Spec.Class,
		Segment:    rec.Spec.Network.Segment,
		BMCAddress: rec.Spec.BMC.Address,
		BMCType:    string(rec.Spec.BMC.Type),
	}
	if hw != nil {
		if hw.Identity != nil {
			f.Vendor = hw.Identity.Vendor
			f.Model = hw.Identity.Model
		}
		if hw.Compute != nil {
			f.Cores = hw.Compute.CoresTotal
			f.RAMGiB = hw.Compute.RAMGiB
		}
		if hw.Storage != nil {
			f.StorageGiB = hw.Storage.TotalGiB
		}
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

// discoverFacts gathers hardware inventory from Go-side sources (BMH introspection,
// Redfish) and returns it for direct write to the store. Returns nil when no
// Go-side source has data yet (Python collectors may still populate the row).
func (r *InventoryRecordReconciler) discoverFacts(ctx context.Context, rec *v1alpha1.InventoryRecord) *v1alpha1.DiscoveredInventory {
	// Primary: BareMetalHost introspection result (Ironic).
	bmhObj := &unstructured.Unstructured{}
	bmhObj.SetGroupVersionKind(bmhGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: rec.Name, Namespace: rec.Namespace}, bmhObj); err == nil {
		if hw, found, _ := unstructured.NestedMap(bmhObj.Object, "status", "hardwareDetails"); found {
			if inv := bmh.MapHardwareDetails(hw); inv != nil {
				return inv
			}
		}
	}

	// Fallback: direct Redfish query for generic/whitebox BMC types not managed
	// by OME, Intersight, or UCS Central.
	if rec.Spec.BMC.Type == v1alpha1.BMCTypeGeneric || rec.Spec.BMC.Type == "" {
		return redfish.Enrich(ctx, r.Client, rec)
	}
	return nil
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
