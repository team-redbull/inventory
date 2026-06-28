// Package binder isolates the ONE place the provisioning flow differs: how a
// HostedCluster NodePool selects hosts of a class. You run the Agent platform
// (Assisted Installer), so AgentBinder is the live implementation; CAPM3Binder
// is retained as a stub for the alternate flow.
//
// CLASS LABEL PREREQUISITE: AvailableHosts and the NodePool selector both key on
// the Agent carrying inventory.example.io/class=<class>. The classifier sets the
// class on the BareMetalHost at inspection; ensure it reaches the Agent too —
// cleanest via an InfraEnv per class (spec.agentLabels{class: X}) so every host
// booting that InfraEnv's ISO is labelled, or a tiny reconciler copying the BMH
// label onto the Agent once it registers.
package binder

import (
	"context"
	"fmt"

	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
)

// NOTE: pin these to the versions your MCE runs — the assisted-service and
// HyperShift API import paths and a few field names have drifted across
// releases (e.g. hypershift/api/v1beta1 vs hypershift/api/hypershift/v1beta1).

// Binder is the method-specific binding surface.
type Binder interface {
	AvailableHosts(ctx context.Context, class string) (int, error)
	EnsureNodePool(ctx context.Context, hc v1alpha1.HostedClusterRef, nodePool, class string, replicas int32) error
	BoundCount(ctx context.Context, hc v1alpha1.HostedClusterRef, nodePool string) (int32, error)
}

// ---- agent-based implementation (live) --------------------------------------

// AgentBinder binds via the Assisted Installer Agent platform.
type AgentBinder struct {
	Client client.Client
	// AgentNamespace is where Agents/InfraEnvs live; empty = search all namespaces.
	AgentNamespace string
}

func NewAgentBinder(c client.Client, agentNamespace string) *AgentBinder {
	return &AgentBinder{Client: c, AgentNamespace: agentNamespace}
}

var _ Binder = (*AgentBinder)(nil)

// AvailableHosts counts approved, unbound Agents of the class in this MCE.
func (b *AgentBinder) AvailableHosts(ctx context.Context, class string) (int, error) {
	var agents aiv1beta1.AgentList
	opts := []client.ListOption{client.MatchingLabels{v1alpha1.LabelClass: class}}
	if b.AgentNamespace != "" {
		opts = append(opts, client.InNamespace(b.AgentNamespace))
	}
	if err := b.Client.List(ctx, &agents, opts...); err != nil {
		return 0, err
	}
	n := 0
	for i := range agents.Items {
		a := &agents.Items[i]
		// approved + not yet bound to a ClusterDeployment = available to bind.
		if a.Spec.Approved && a.Spec.ClusterDeploymentName == nil {
			n++
		}
	}
	return n, nil
}

// EnsureNodePool sizes an EXISTING NodePool and guarantees its agentLabelSelector
// targets the class. The NodePool itself is created with the HostedCluster
// (via GitOps); the claim only sizes/steers it.
func (b *AgentBinder) EnsureNodePool(ctx context.Context, hc v1alpha1.HostedClusterRef,
	nodePool, class string, replicas int32) error {

	var np hyperv1.NodePool
	key := client.ObjectKey{Namespace: hc.Namespace, Name: nodePool}
	if err := b.Client.Get(ctx, key, &np); err != nil {
		return fmt.Errorf("get nodepool %s/%s: %w", hc.Namespace, nodePool, err)
	}
	orig := np.DeepCopy()

	r := replicas
	np.Spec.Replicas = &r

	if np.Spec.Platform.Agent == nil {
		np.Spec.Platform.Agent = &hyperv1.AgentPlatformSpec{}
	}
	if np.Spec.Platform.Agent.AgentLabelSelector == nil {
		np.Spec.Platform.Agent.AgentLabelSelector = &metav1.LabelSelector{}
	}
	if np.Spec.Platform.Agent.AgentLabelSelector.MatchLabels == nil {
		np.Spec.Platform.Agent.AgentLabelSelector.MatchLabels = map[string]string{}
	}
	np.Spec.Platform.Agent.AgentLabelSelector.MatchLabels[v1alpha1.LabelClass] = class

	return b.Client.Patch(ctx, &np, client.MergeFrom(orig))
}

// BoundCount returns the NodePool's current (provisioned) replica count.
func (b *AgentBinder) BoundCount(ctx context.Context, hc v1alpha1.HostedClusterRef,
	nodePool string) (int32, error) {

	var np hyperv1.NodePool
	key := client.ObjectKey{Namespace: hc.Namespace, Name: nodePool}
	if err := b.Client.Get(ctx, key, &np); err != nil {
		return 0, err
	}
	return np.Status.Replicas, nil
}

// ---- CAPM3 implementation (stub for the alternate flow) ---------------------

// CAPM3Binder binds via Cluster API provider Metal3 + hostSelector.
type CAPM3Binder struct{}

func NewCAPM3Binder() *CAPM3Binder { return &CAPM3Binder{} }

var _ Binder = (*CAPM3Binder)(nil)

func (b *CAPM3Binder) AvailableHosts(ctx context.Context, class string) (int, error) {
	panic("not implemented: count available BMHs with class label")
}
func (b *CAPM3Binder) EnsureNodePool(ctx context.Context, hc v1alpha1.HostedClusterRef, nodePool, class string, replicas int32) error {
	panic("not implemented: patch NodePool replicas + Metal3 hostSelector")
}
func (b *CAPM3Binder) BoundCount(ctx context.Context, hc v1alpha1.HostedClusterRef, nodePool string) (int32, error) {
	panic("not implemented: read NodePool/Machine ready count")
}
