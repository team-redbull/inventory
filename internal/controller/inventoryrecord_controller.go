package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
const agentBMHLabel = "agent-install.openshift.io/bmh"

// condEnrolled is the IR condition that gates the in-service path.
// True = host-install workflow succeeded; False = workflow failed (manual cleanup
// of the failed Workflow object required before re-enrollment can start).
const condEnrolled = "Enrolled"

// bmcSecretPrefix is the naming convention the host-install WorkflowTemplate
// expects: credentialsName: bmc-<serviceTag>.
const bmcSecretPrefix = "bmc-"

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
	workflowGVK = schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "Workflow",
	}
)

// InventoryRecordReconciler is the single IR controller — a phase-dispatching
// state machine. Two per-MCE controllers total: this one and HostClaimReconciler.
//
// Phase dispatch (lease state + Enrolled condition):
//
//	Enrolled=True (Owned, workflow done) → reconcileInService
//	else                                 → reconcileEnroll
//
// Future phases (lifecycle #10, move #11) extend the dispatch here.
//
// Postgres is the single source of truth for hardware facts. Hardware is never
// cached in IR status — UpsertHost writes directly to host_inventory on every
// reconcile regardless of phase.
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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update
// +kubebuilder:rbac:groups=argoproj.io,resources=workflows,verbs=create;get

func (r *InventoryRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec v1alpha1.InventoryRecord
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Always project spec + any Go-discovered hardware facts to the store.
	// Python collectors (OME/Intersight/UCS) write independently; COALESCE guards
	// in UpsertHost prevent either side from zeroing the other's data.
	hw := r.discoverFacts(ctx, &rec)
	if err := r.Store.UpsertHost(ctx, buildHostFact(&rec, hw)); err != nil {
		return ctrl.Result{}, fmt.Errorf("upsert host %s: %w", rec.Spec.ServiceTag, err)
	}
	if hw != nil && len(hw.Network) > 0 {
		nics := make([]store.NIC, 0, len(hw.Network))
		for _, n := range hw.Network {
			nics = append(nics, store.NIC{MAC: n.MAC, Name: n.Name, SpeedMbs: n.SpeedMbs})
		}
		if err := r.Store.UpsertNICs(ctx, rec.Spec.ServiceTag, nics); err != nil {
			return ctrl.Result{}, fmt.Errorf("upsert nics %s: %w", rec.Spec.ServiceTag, err)
		}
	}

	// Enrolled=True gates the in-service path. Absent/Unknown/False → enroll phase.
	if apimeta.IsStatusConditionTrue(rec.Status.Conditions, condEnrolled) {
		return r.reconcileInService(ctx, &rec)
	}
	return r.reconcileEnroll(ctx, &rec)
}

// reconcileEnroll handles new hosts: acquire the lease, copy the BMC credential
// Secret into the IR's namespace (where the BMH will also live), then launch and
// monitor the host-install Argo WorkflowTemplate.
func (r *InventoryRecordReconciler) reconcileEnroll(ctx context.Context, rec *v1alpha1.InventoryRecord) (ctrl.Result, error) {
	homeMCE := rec.Spec.Placement.HomeMCE
	if homeMCE == "" {
		homeMCE = r.MCE
	}

	lease, err := r.Store.Get(ctx, rec.Spec.ServiceTag)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get lease %s: %w", rec.Spec.ServiceTag, err)
	}

	if lease == nil || lease.State == store.LeaseFree {
		lease, err = store.Acquire(ctx, r.Store, rec.Spec.ServiceTag, homeMCE)
		if errors.Is(err, store.ErrLeaseConflict) {
			// Another MCE holds it — not our host; stop.
			return ctrl.Result{}, nil
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("acquire lease %s: %w", rec.Spec.ServiceTag, err)
		}
	}

	// Guard: BMH creation grips the physical BMC — only proceed with confirmed
	// lease ownership. ArgoCD scoping makes collisions rare, but the lease is
	// the single-writer guarantee (ARCHITECTURE §7).
	if lease == nil || lease.OwnerMCE != r.MCE {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.ensureBMCSecret(ctx, rec); err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileEnrollWorkflow(ctx, rec)
}

// reconcileEnrollWorkflow creates the host-install Workflow if not present, then
// mirrors its phase into the Enrolled condition. Once Succeeded, Enrolled=True
// flips the reconcile dispatch to reconcileInService on the next reconcile.
func (r *InventoryRecordReconciler) reconcileEnrollWorkflow(ctx context.Context, rec *v1alpha1.InventoryRecord) (ctrl.Result, error) {
	wfName := "enroll-" + rec.Spec.ServiceTag

	wf := &unstructured.Unstructured{}
	wf.SetGroupVersionKind(workflowGVK)
	err := r.Get(ctx, types.NamespacedName{Name: wfName, Namespace: rec.Namespace}, wf)

	if apierrors.IsNotFound(err) {
		wf = r.buildEnrollWorkflow(rec, wfName)
		if createErr := r.Create(ctx, wf); createErr != nil {
			return ctrl.Result{}, fmt.Errorf("create enroll workflow %s: %w", wfName, createErr)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get enroll workflow %s: %w", wfName, err)
	}

	phase, _, _ := unstructured.NestedString(wf.Object, "status", "phase")
	msg, _, _ := unstructured.NestedString(wf.Object, "status", "message")
	return r.patchEnrolledCondition(ctx, rec, phase, msg)
}

// buildEnrollWorkflow returns an Argo Workflow that references the host-install
// WorkflowTemplate. The method is derived from BMC type + BootMACAddress:
// a generic BMC with a boot MAC means IPMI+PXE; everything else uses Redfish.
// BMH and Workflow are created in rec.Namespace so discoverFacts + the BMH
// watch in SetupWithManager stay in sync.
func (r *InventoryRecordReconciler) buildEnrollWorkflow(rec *v1alpha1.InventoryRecord, name string) *unstructured.Unstructured {
	method := "redfish"
	if rec.Spec.BMC.BootMACAddress != "" &&
		(rec.Spec.BMC.Type == v1alpha1.BMCTypeGeneric || rec.Spec.BMC.Type == "") {
		method = "ipmi-pxe"
	}

	params := []interface{}{
		map[string]interface{}{"name": "serviceTag", "value": rec.Spec.ServiceTag},
		map[string]interface{}{"name": "method", "value": method},
		map[string]interface{}{"name": "bmcAddress", "value": rec.Spec.BMC.Address},
		map[string]interface{}{"name": "bootMAC", "value": rec.Spec.BMC.BootMACAddress},
		map[string]interface{}{"name": "namespace", "value": rec.Namespace},
		map[string]interface{}{"name": "class", "value": rec.Spec.Class},
	}

	wf := &unstructured.Unstructured{}
	wf.SetGroupVersionKind(workflowGVK)
	wf.SetName(name)
	wf.SetNamespace(rec.Namespace)
	_ = unstructured.SetNestedField(wf.Object, map[string]interface{}{
		"workflowTemplateRef": map[string]interface{}{"name": "host-install"},
		"arguments":           map[string]interface{}{"parameters": params},
	}, "spec")
	return wf
}

// patchEnrolledCondition maps Argo Workflow phase → IR Enrolled condition and
// patches status. Succeeded flips the dispatch to reconcileInService next round.
func (r *InventoryRecordReconciler) patchEnrolledCondition(ctx context.Context, rec *v1alpha1.InventoryRecord, wfPhase, msg string) (ctrl.Result, error) {
	cond := metav1.Condition{
		Type:               condEnrolled,
		ObservedGeneration: rec.Generation,
	}
	switch wfPhase {
	case "Succeeded":
		cond.Status = metav1.ConditionTrue
		cond.Reason = "EnrollComplete"
		cond.Message = "host-install workflow succeeded"
	case "Failed", "Error":
		cond.Status = metav1.ConditionFalse
		cond.Reason = "EnrollFailed"
		cond.Message = msg
	default: // Pending, Running, ""
		cond.Status = metav1.ConditionUnknown
		cond.Reason = "Enrolling"
		cond.Message = "host-install workflow running"
	}

	base := rec.DeepCopy()
	apimeta.SetStatusCondition(&rec.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch enrolled condition %s: %w", rec.Name, err)
	}

	if wfPhase == "Failed" || wfPhase == "Error" {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ensureBMCSecret copies the IR's credentialsRef Secret to bmc-<serviceTag> in
// rec.Namespace, which is where the host-install WorkflowTemplate expects it
// (credentialsName: bmc-<serviceTag> in the BMH manifest). The source namespace
// is always rec.Namespace — the user-supplied credentialsRef.Namespace is ignored
// per the confused-deputy prevention policy.
func (r *InventoryRecordReconciler) ensureBMCSecret(ctx context.Context, rec *v1alpha1.InventoryRecord) error {
	targetName := bmcSecretPrefix + rec.Spec.ServiceTag

	// Nothing to do when the creds Secret is already named correctly.
	if rec.Spec.BMC.CredentialsRef.Name == targetName {
		return nil
	}

	// Idempotent: skip if target already exists.
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: rec.Namespace}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get bmc secret %s/%s: %w", rec.Namespace, targetName, err)
	}

	// Read source from rec.Namespace — never from user-supplied namespace.
	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      rec.Spec.BMC.CredentialsRef.Name,
		Namespace: rec.Namespace,
	}, src); err != nil {
		return fmt.Errorf("read bmc creds %s/%s: %w", rec.Namespace, rec.Spec.BMC.CredentialsRef.Name, err)
	}

	out := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetName,
			Namespace: rec.Namespace,
		},
		Type: src.Type,
		Data: src.Data,
	}
	if err := r.Create(ctx, out); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create bmc secret %s/%s: %w", rec.Namespace, targetName, err)
	}
	return nil
}

// reconcileInService runs every 30s once a host is enrolled. Mirrors Agent
// binding state into the store and into status.allocation.
func (r *InventoryRecordReconciler) reconcileInService(ctx context.Context, rec *v1alpha1.InventoryRecord) (ctrl.Result, error) {
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
	if err := r.Status().Patch(ctx, rec, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status %s: %w", rec.Name, err)
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// buildHostFact assembles a store.HostFact from spec fields + optional discovered
// hardware. Called on every reconcile regardless of phase.
func buildHostFact(rec *v1alpha1.InventoryRecord, hw *v1alpha1.DiscoveredInventory) store.HostFact {
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
	return f
}

// discoverFacts gathers hardware inventory from Go-side sources (BMH introspection,
// Redfish) for direct write to the store. Returns nil when no Go-side source has
// data yet (Python collectors may still populate the row independently).
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

// resolveAllocation looks up the Agent that BMAC paired to this BMH and returns
// an Allocation if the Agent is bound to a cluster, or nil if unbound.
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
	// Watch BareMetalHost changes and reconcile the InventoryRecord with the same
	// name+namespace (BMH name = serviceTag = IR name, all in rec.Namespace).
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
