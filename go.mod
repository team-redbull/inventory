module example.io/inventory

go 1.22

// MCE-version-independent: NodePool, Agent, and BareMetalHost are accessed via
// unstructured + the dynamic client, so there is NO dependency on the
// hypershift, assisted-service, or metal3 Go modules — whose import paths and
// field types drift across MCE 2.7 (OCP 4.16) and MCE 2.10 (OCP 4.20). One image
// runs on both. Run `go mod tidy` against your internal proxy to resolve.
require (
	github.com/jackc/pgx/v5 v5.6.0
	k8s.io/apimachinery v0.30.3
	k8s.io/client-go v0.30.3
	sigs.k8s.io/controller-runtime v0.18.4
)
