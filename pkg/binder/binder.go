// Package binder isolates the ONE place the provisioning flow differs: how a
// HostedCluster NodePool selects hosts of a class. You run the Agent platform
// (Assisted Installer), so AgentBinder is the live implementation; CAPM3Binder
// is retained as a stub for the alternate flow.
//
// VERSION-INDEPENDENCE: this package talks to NodePool and Agent via
// unstructured + the controller-runtime client, so the binary depends only on
// apimachinery — NOT on the hypershift or assisted-service Go modules. Those
// modules' import paths and field types drift across MCE releases, but the CRD
// apiVersions and the small field surface we touch are stable. Result: ONE
// manager image runs across MCE 2.7 (OCP 4.16) and MCE 2.10 (OCP 4.20).
//
// CLASS LABEL PREREQUISITE: AvailableHosts and the NodePool selector both key on
// the Agent carrying inventory.example.io/class=<class>. The classifier sets the
// class on the BareMetalHost at inspection; ensure it reaches the Agent too —
// cleanest via an InfraEnv per class (spec.agentLabels{class: X}).
package binder

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
)

// GVKs of the version-drifting CRDs we touch via unstructured.
// Verified against upstream source (2026-06):
//   Agent    github.com/openshift/assisted-service      api/v1beta1/agent_types.go
//   NodePool github.com/openshift/hypershift            api/hypershift/v1beta1/agent.go + nodepool_types.go
//
// Field paths confirmed:
//   Agent.spec.approved                                 bool (required)
//   Agent.spec.clusterDeploymentName.name               string (kubebuilder printcolumn confirms path)
//   NodePool.spec.replicas                              int32
//   NodePool.spec.platform.agent.agentLabelSelector     *metav1.LabelSelector (AgentNodePoolPlatform)
//   NodePool.status.replicas                            int32 "latest observed number of nodes in pool"
//
// TODO (needs live cluster): compile against exact MCE version in use and run
// integration test to confirm status.replicas tracks bound Agents as expected.
var (
	agentListGVK = schema.GroupVersionKind{Group: "agent-install.openshift.io", Version: "v1beta1", Kind: "AgentList"}
	nodePoolGVK  = schema.GroupVersionKind{Group: "hypershift.openshift.io", Version: "v1beta1", Kind: "NodePool"}
)

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
	agents := &unstructured.UnstructuredList{}
	agents.SetGroupVersionKind(agentListGVK)

	opts := []client.ListOption{client.MatchingLabels{v1alpha1.LabelClass: class}}
	if b.AgentNamespace != "" {
		opts = append(opts, client.InNamespace(b.AgentNamespace))
	}
	if err := b.Client.List(ctx, agents, opts...); err != nil {
		return 0, err
	}

	n := 0
	for i := range agents.Items {
		obj := agents.Items[i].Object
		approved, _, _ := unstructured.NestedBool(obj, "spec", "approved")
		// spec.clusterDeploymentName is an object {name,namespace} once bound.
		boundTo, _, _ := unstructured.NestedString(obj, "spec", "clusterDeploymentName", "name")
		if approved && boundTo == "" {
			n++
		}
	}
	return n, nil
}

// EnsureNodePool sizes an EXISTING NodePool and guarantees its agentLabelSelector
// targets the class. The NodePool itself is created with the HostedCluster (via
// GitOps); the claim only sizes/steers it.
func (b *AgentBinder) EnsureNodePool(ctx context.Context, hc v1alpha1.HostedClusterRef,
	nodePool, class string, replicas int32) error {

	np := &unstructured.Unstructured{}
	np.SetGroupVersionKind(nodePoolGVK)
	key := client.ObjectKey{Namespace: hc.Namespace, Name: nodePool}
	if err := b.Client.Get(ctx, key, np); err != nil {
		return fmt.Errorf("get nodepool %s/%s: %w", hc.Namespace, nodePool, err)
	}
	orig := np.DeepCopy()

	if err := unstructured.SetNestedField(np.Object, int64(replicas), "spec", "replicas"); err != nil {
		return fmt.Errorf("set replicas: %w", err)
	}

	// Merge the class into spec.platform.agent.agentLabelSelector.matchLabels,
	// preserving any selector terms already present.
	sel := []string{"spec", "platform", "agent", "agentLabelSelector", "matchLabels"}
	labels, _, err := unstructured.NestedStringMap(np.Object, sel...)
	if err != nil {
		return fmt.Errorf("read agentLabelSelector: %w", err)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[v1alpha1.LabelClass] = class
	if err := unstructured.SetNestedStringMap(np.Object, labels, sel...); err != nil {
		return fmt.Errorf("set agentLabelSelector: %w", err)
	}

	return b.Client.Patch(ctx, np, client.MergeFrom(orig))
}

// BoundCount returns the NodePool's current (provisioned) replica count.
func (b *AgentBinder) BoundCount(ctx context.Context, hc v1alpha1.HostedClusterRef,
	nodePool string) (int32, error) {

	np := &unstructured.Unstructured{}
	np.SetGroupVersionKind(nodePoolGVK)
	key := client.ObjectKey{Namespace: hc.Namespace, Name: nodePool}
	if err := b.Client.Get(ctx, key, np); err != nil {
		return 0, err
	}
	r, _, err := unstructured.NestedInt64(np.Object, "status", "replicas")
	if err != nil {
		return 0, err
	}
	return int32(r), nil
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
