module example.io/inventory

go 1.22

// NOTE: versions below are indicative. Run `go mod tidy` against your internal
// module proxy, and PIN the openshift modules to the exact versions your MCE
// runs (assisted-service + hypershift API paths/fields drift across releases).
require (
	github.com/jackc/pgx/v5 v5.6.0
	github.com/openshift/assisted-service/api v0.0.0-00010101000000-000000000000
	github.com/openshift/hypershift/api v0.0.0-00010101000000-000000000000
	github.com/metal3-io/baremetal-operator/apis v0.5.1
	k8s.io/apimachinery v0.30.3
	k8s.io/client-go v0.30.3
	sigs.k8s.io/controller-runtime v0.18.4
)
