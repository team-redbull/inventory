package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClaimPhase is the high-level satisfaction state of a HostClaim.
type ClaimPhase string

const (
	// ClaimPending: not yet satisfied, allocation in progress or waiting.
	ClaimPending ClaimPhase = "Pending"
	// ClaimSatisfied: count hosts bound.
	ClaimSatisfied ClaimPhase = "Satisfied"
	// ClaimPartial: some but not all of count bound.
	ClaimPartial ClaimPhase = "PartiallySatisfied"
	// ClaimUnsatisfiable: cannot be met (no eligible hosts, segment unreachable,
	// pinned host unavailable). unsatisfiableReason explains which.
	ClaimUnsatisfiable ClaimPhase = "Unsatisfiable"
)

// -------------------------------------------------------------------------
// Spec
// -------------------------------------------------------------------------

type HostClaimSpec struct {
	// Selector chooses candidate hosts by label. This spans the whole range:
	//   - "any N of class a"  -> matchLabels{inventory.example.io/class: a}
	//   - a specific host     -> a selector that matches exactly one host
	//     (e.g. matchLabels on a unique tag) with count: 1.
	// A pinned (single-host) selector removes the allocator's freedom to
	// substitute, so it is the most likely to be Unsatisfiable.
	// +kubebuilder:validation:Required
	Selector metav1.LabelSelector `json:"selector"`

	// Count is how many matching hosts to bind.
	// +kubebuilder:validation:Minimum=1
	Count int32 `json:"count"`

	// TargetHostedCluster is the HyperShift HostedCluster that consumes the
	// bound capacity. Its home MCE (the one hosting that control plane and
	// owning the BMHs) is DERIVED from this, not declared — that derived MCE is
	// the local-first target, and spill considers other MCEs.
	// +kubebuilder:validation:Required
	TargetHostedCluster HostedClusterRef `json:"targetHostedCluster"`

	// NodePool optionally names which of the HostedCluster's NodePools to size.
	// If empty, the reconciler uses the cluster's default/worker NodePool.
	// +optional
	NodePool string `json:"nodePool,omitempty"`

	// AllowSpill permits the fleet allocator to satisfy a shortfall by pulling
	// eligible hosts from other MCEs (the cross-MCE move path). Defaults true.
	// Set false to confine a claim to the target HostedCluster's own MCE.
	// +optional
	// +kubebuilder:default=true
	AllowSpill *bool `json:"allowSpill,omitempty"`
}

// HostedClusterRef points at a HyperShift HostedCluster (namespaced in MCE).
type HostedClusterRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
}

// -------------------------------------------------------------------------
// Status
// -------------------------------------------------------------------------

type HostClaimStatus struct {
	// Phase is the high-level satisfaction state.
	// +kubebuilder:validation:Enum=Pending;Satisfied;PartiallySatisfied;Unsatisfiable
	Phase ClaimPhase `json:"phase,omitempty"`

	// Desired echoes spec.count; Bound is how many are currently bound.
	Desired int32 `json:"desired,omitempty"`
	Bound   int32 `json:"bound,omitempty"`

	// BoundHosts lists the service tags of hosts satisfying this claim.
	BoundHosts []string `json:"boundHosts,omitempty"`

	// UnsatisfiableReason is set when the claim cannot be met, with a concrete
	// cause the team can act on, e.g.:
	//   "no available host with class=a"
	//   "1 candidate exists but its segment prov-vlan-220 is unreachable from mce-2"
	//   "pinned host AB12CD3 is owned by workload-red and AllowSpill is false"
	// Never leave a claim silently Pending when it is actually impossible.
	UnsatisfiableReason string `json:"unsatisfiableReason,omitempty"`

	// LastTransitionTime is when Phase last changed.
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// -------------------------------------------------------------------------
// Boilerplate
// -------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hc
// +kubebuilder:printcolumn:name="HostedCluster",type=string,JSONPath=`.spec.targetHostedCluster.name`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desired`
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.bound`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.unsatisfiableReason`,priority=1

// HostClaim is a declarative request for N hosts of a class, bound to a HostedCluster.
type HostClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostClaimSpec   `json:"spec,omitempty"`
	Status HostClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HostClaimList contains a list of HostClaim.
type HostClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostClaim{}, &HostClaimList{})
}
