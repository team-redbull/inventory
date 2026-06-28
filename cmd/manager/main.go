// Command manager runs the per-MCE controllers. One instance per MCE.
package main

import (
	"context"
	"flag"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	inventoryv1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/internal/controller"
	"example.io/inventory/pkg/binder"
	"example.io/inventory/pkg/store"
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
	var mceName, agentNamespace, metricsAddr, pgURL string
	flag.StringVar(&mceName, "mce", "", "name of the MCE this manager runs in")
	flag.StringVar(&agentNamespace, "agent-namespace", "", "namespace where Agents live (empty = all)")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&pgURL, "postgres-url", "", "PostgreSQL DSN (postgres://user:pass@host/db)")
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

	// Postgres projector: project InventoryRecord status into the central store.
	// Optional — manager runs without it; store writes are skipped until wired.
	if pgURL != "" {
		pool, err := pgxpool.New(context.Background(), pgURL)
		if err != nil {
			log.Error(err, "unable to create postgres pool")
			os.Exit(1)
		}
		if err := pool.Ping(context.Background()); err != nil {
			log.Error(err, "postgres ping failed")
			os.Exit(1)
		}
		defer pool.Close()

		st := store.NewPG(pool)
		if err := (&controller.InventoryRecordReconciler{
			Client: mgr.GetClient(),
			Store:  st,
			MCE:    mceName,
		}).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up InventoryRecord reconciler")
			os.Exit(1)
		}
		cfg := pool.Config().ConnConfig
		log.Info("postgres store connected", "host", cfg.Host, "db", cfg.Database)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
