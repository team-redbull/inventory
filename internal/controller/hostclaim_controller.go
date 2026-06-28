// Package controller holds the HostClaim reconciler: the everyday allocation
// path. It resolves a claim against the LOCAL pool first (native NodePool +
// class selector) and only signals the fleet allocator when short and spill is
// allowed. It never owns the cross-MCE machinery — that's the move controller.
package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/binder"
)

// SpillRequester is how the reconciler signals a shortfall to the fleet
// allocator. Implementations: write a SpillRequest CR, post to the allocator
// API, or record the shortfall in the store. Kept abstract so the everyday
// path doesn't depend on the overflow machinery being present.
type SpillRequester interface {
	RequestSpill(ctx context.Context, claim *v1alpha1.HostClaim, class string, shortBy int32) error
}

// HostClaimReconciler reconciles a HostClaim into local NodePool capacity.
type HostClaimReconciler struct {
	client.Client
	Binder binder.Binder
	Spill  SpillRequester // may be nil before Phase 3 — then shortfalls are reported as Unsatisfiable
	MCE    string         // name of the MCE this controller runs in
}

// +kubebuilder:rbac:groups=inventory.example.io,resources=hostclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=inventory.example.io,resources=hostclaims/status,verbs=get;update;patch

func (r *HostClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var hc v1alpha1.HostClaim
	if err := r.Get(ctx, req.NamespacedName, &hc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	class, ok := classFromSelector(&hc.Spec.Selector)
	if !ok {
		return r.fail(ctx, &hc, "selector has no inventory.example.io/class label; "+
			"a claim must select a class (pin via a unique label + count:1)")
	}

	desired := hc.Spec.Count
	pool := hc.Spec.NodePool
	if pool == "" {
		pool = defaultWorkerPool(hc.Spec.TargetHostedCluster.Name)
	}

	// Local-first: size the NodePool to request `desired` hosts of `class`.
	if err := r.Binder.EnsureNodePool(ctx, hc.Spec.TargetHostedCluster, pool, class, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure nodepool: %w", err)
	}

	bound, err := r.Binder.BoundCount(ctx, hc.Spec.TargetHostedCluster, pool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("bound count: %w", err)
	}
	if bound >= desired {
		return r.satisfied(ctx, &hc, bound)
	}

	// Short. How many of this class are available locally to bind right now?
	avail, err := r.Binder.AvailableHosts(ctx, class)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("available hosts: %w", err)
	}
	shortBy := desired - bound
	if int32(avail) >= shortBy {
		// Capacity exists locally; binding is in flight. Requeue to observe it.
		return r.pending(ctx, &hc, bound, "binding in progress")
	}

	// Genuinely short of local capacity.
	stillShort := shortBy - int32(avail)
	allowSpill := hc.Spec.AllowSpill == nil || *hc.Spec.AllowSpill
	if allowSpill && r.Spill != nil {
		if err := r.Spill.RequestSpill(ctx, &hc, class, stillShort); err != nil {
			return ctrl.Result{}, fmt.Errorf("request spill: %w", err)
		}
		return r.pending(ctx, &hc, bound,
			fmt.Sprintf("local pool short by %d; requested fleet spill", stillShort))
	}

	// No spill possible -> say so, concretely, instead of silently pending.
	reason := fmt.Sprintf("need %d host(s) of class %q in MCE %s; only %d available and spill is %s",
		shortBy, class, r.MCE, avail, spillWord(allowSpill, r.Spill != nil))
	return r.unsatisfiable(ctx, &hc, bound, reason)
}

// ---- status helpers ---------------------------------------------------------

func (r *HostClaimReconciler) satisfied(ctx context.Context, hc *v1alpha1.HostClaim, bound int32) (ctrl.Result, error) {
	return r.setStatus(ctx, hc, v1alpha1.ClaimSatisfied, bound, "", 0)
}

func (r *HostClaimReconciler) pending(ctx context.Context, hc *v1alpha1.HostClaim, bound int32, msg string) (ctrl.Result, error) {
	phase := v1alpha1.ClaimPending
	if bound > 0 {
		phase = v1alpha1.ClaimPartial
	}
	return r.setStatus(ctx, hc, phase, bound, "", 15*time.Second)
}

func (r *HostClaimReconciler) unsatisfiable(ctx context.Context, hc *v1alpha1.HostClaim, bound int32, reason string) (ctrl.Result, error) {
	return r.setStatus(ctx, hc, v1alpha1.ClaimUnsatisfiable, bound, reason, time.Minute)
}

func (r *HostClaimReconciler) fail(ctx context.Context, hc *v1alpha1.HostClaim, reason string) (ctrl.Result, error) {
	return r.setStatus(ctx, hc, v1alpha1.ClaimUnsatisfiable, 0, reason, 0)
}

func (r *HostClaimReconciler) setStatus(ctx context.Context, hc *v1alpha1.HostClaim,
	phase v1alpha1.ClaimPhase, bound int32, reason string, requeue time.Duration) (ctrl.Result, error) {

	hc.Status.Phase = phase
	hc.Status.Desired = hc.Spec.Count
	hc.Status.Bound = bound
	hc.Status.UnsatisfiableReason = reason
	now := metav1.Now()
	hc.Status.LastTransitionTime = &now

	cond := metav1.Condition{Type: "Satisfied", ObservedGeneration: hc.Generation, LastTransitionTime: now}
	switch phase {
	case v1alpha1.ClaimSatisfied:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "Bound", "all requested hosts bound"
	case v1alpha1.ClaimUnsatisfiable:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Unsatisfiable", reason
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Pending", "allocation in progress"
	}
	meta.SetStatusCondition(&hc.Status.Conditions, cond)

	if err := r.Status().Update(ctx, hc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// ---- small helpers ----------------------------------------------------------

func classFromSelector(sel *metav1.LabelSelector) (string, bool) {
	if sel == nil {
		return "", false
	}
	if c, ok := sel.MatchLabels[v1alpha1.LabelClass]; ok && c != "" {
		return c, true
	}
	return "", false
}

func defaultWorkerPool(hostedCluster string) string { return hostedCluster + "-workers" }

func spillWord(allow, haveRequester bool) string {
	if !allow {
		return "disabled (allowSpill=false)"
	}
	if !haveRequester {
		return "unavailable (fleet allocator not deployed)"
	}
	return "enabled"
}

func (r *HostClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HostClaim{}).
		Complete(r)
}
