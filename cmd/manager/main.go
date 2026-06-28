// Command manager runs the per-MCE controllers. One instance per MCE.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	inventoryv1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/internal/controller"
	"example.io/inventory/pkg/binder"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(inventoryv1alpha1.AddToScheme(scheme))
	// NodePool/Agent/BMH are accessed via unstructured (see pkg/binder,
	// pkg/inventory/bmh), so their Go schemes are intentionally NOT registered —
	// that's what keeps this image MCE-version-independent.
}

func main() {
	var mceName, agentNamespace, metricsAddr string
	flag.StringVar(&mceName, "mce", "", "name of the MCE this manager runs in")
	flag.StringVar(&agentNamespace, "agent-namespace", "", "namespace where Agents live (empty = all)")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.Parse()

	log := zap.New(zap.UseDevMode(true))
	ctrl.SetLogger(log)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Everyday allocation path. The Spill requester is nil until the fleet
	// allocator exists (Phase 3) — shortfalls report Unsatisfiable until then.
	b := binder.NewAgentBinder(mgr.GetClient(), agentNamespace)
	if err := (&controller.HostClaimReconciler{
		Client: mgr.GetClient(),
		Binder: b,
		MCE:    mceName,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up HostClaim reconciler")
		os.Exit(1)
	}

	// TODO: register enroll, lifecycle, move controllers + collectors here.

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
