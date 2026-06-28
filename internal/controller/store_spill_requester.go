package controller

import (
	"context"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/store"
)

// StoreSpillRequester implements SpillRequester by writing to host_spill_request.
// The fleet allocator (component #12) reads that table to decide which hosts to
// move across MCEs to satisfy the shortfall.
type StoreSpillRequester struct {
	Store store.SpillStore
	MCE   string
}

func (s *StoreSpillRequester) RequestSpill(ctx context.Context, claim *v1alpha1.HostClaim, class string, shortBy int32) error {
	return s.Store.UpsertSpillRequest(ctx, store.SpillRequest{
		ClaimName: claim.Name,
		ClaimNS:   claim.Namespace,
		Class:     class,
		ShortBy:   shortBy,
		MCE:       s.MCE,
	})
}

func (s *StoreSpillRequester) CancelSpill(ctx context.Context, claim *v1alpha1.HostClaim) error {
	return s.Store.DeleteSpillRequest(ctx, claim.Name, claim.Namespace)
}
