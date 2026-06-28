package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG implements Store over a pgx connection pool. Point it at the primary for
// writes; you can run a second PG bound to a read replica for the capacity API.
type PG struct {
	pool *pgxpool.Pool
}

func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

var _ Store = (*PG)(nil)

// ---- LeaseStore -------------------------------------------------------------

func (p *PG) Get(ctx context.Context, serviceTag string) (*Lease, error) {
	const q = `SELECT service_tag, coalesce(owner_mce,''), state, generation
	             FROM host_lease WHERE service_tag = $1`
	var l Lease
	err := p.pool.QueryRow(ctx, q, serviceTag).
		Scan(&l.ServiceTag, &l.OwnerMCE, &l.State, &l.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// Transition is the heart of the single-writer guarantee: a conditional UPDATE
// that only fires when the row still matches the caller's expectation. Row-level
// MVCC serializes concurrent attempts; no long-held locks.
func (p *PG) Transition(ctx context.Context, serviceTag string,
	expectState LeaseState, expectGen int64,
	newState LeaseState, newOwner string) (*Lease, error) {

	q := `UPDATE host_lease
	         SET state = $2, owner_mce = NULLIF($3,''), generation = generation + 1, updated_at = now()
	       WHERE service_tag = $1 AND state = $4`
	args := []any{serviceTag, string(newState), newOwner, string(expectState)}
	if expectGen != AnyGeneration {
		q += ` AND generation = $5`
		args = append(args, expectGen)
	}
	q += ` RETURNING service_tag, coalesce(owner_mce,''), state, generation`

	var l Lease
	err := p.pool.QueryRow(ctx, q, args...).
		Scan(&l.ServiceTag, &l.OwnerMCE, &l.State, &l.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row matched the guard: either the host doesn't exist or another
		// writer changed state/generation first.
		return nil, ErrLeaseConflict
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ---- InventoryStore ---------------------------------------------------------

func (p *PG) UpsertHost(ctx context.Context, f HostFact) error {
	const q = `
	INSERT INTO host_inventory
	  (service_tag, site, class, vendor, model, cores, ram_gib, storage_gib, segment, bmc_address, bmc_type, last_seen)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now())
	ON CONFLICT (service_tag) DO UPDATE SET
	  site=EXCLUDED.site, class=EXCLUDED.class, vendor=EXCLUDED.vendor, model=EXCLUDED.model,
	  cores=EXCLUDED.cores, ram_gib=EXCLUDED.ram_gib, storage_gib=EXCLUDED.storage_gib,
	  segment=EXCLUDED.segment, bmc_address=EXCLUDED.bmc_address, bmc_type=EXCLUDED.bmc_type,
	  last_seen=now()`
	_, err := p.pool.Exec(ctx, q, f.ServiceTag, f.Site, f.Class, f.Vendor, f.Model,
		f.Cores, f.RAMGiB, f.StorageGiB, f.Segment, f.BMCAddress, f.BMCType)
	if err != nil {
		return err
	}
	// Ensure a lease row exists (defaults to Free) so the host is leaseable.
	_, err = p.pool.Exec(ctx,
		`INSERT INTO host_lease (service_tag) VALUES ($1) ON CONFLICT DO NOTHING`, f.ServiceTag)
	return err
}

func (p *PG) SetAllocation(ctx context.Context, serviceTag string, a *Allocation) error {
	if a == nil {
		_, err := p.pool.Exec(ctx, `DELETE FROM host_allocation WHERE service_tag=$1`, serviceTag)
		return err
	}
	const q = `
	INSERT INTO host_allocation (service_tag, hosted_cluster, node_pool, node_name, claim_ref, updated_at)
	VALUES ($1,$2,$3,$4,$5, now())
	ON CONFLICT (service_tag) DO UPDATE SET
	  hosted_cluster=EXCLUDED.hosted_cluster, node_pool=EXCLUDED.node_pool,
	  node_name=EXCLUDED.node_name, claim_ref=EXCLUDED.claim_ref, updated_at=now()`
	_, err := p.pool.Exec(ctx, q, serviceTag, a.HostedCluster, a.NodePool, a.NodeName, a.ClaimRef)
	return err
}

// ---- CapacityStore ----------------------------------------------------------

func (p *PG) Capacity(ctx context.Context, f CapacityFilter) ([]CapacityRow, error) {
	q := `SELECT site, class, coalesce(owner_mce,''), total, available, allocated, in_transit
	        FROM host_capacity`
	var conds []string
	var args []any
	add := func(col, val string) {
		if val != "" {
			args = append(args, val)
			conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	add("site", f.Site)
	add("class", f.Class)
	add("owner_mce", f.OwnerMCE)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY site, class, owner_mce"

	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CapacityRow
	for rows.Next() {
		var c CapacityRow
		if err := rows.Scan(&c.Site, &c.Class, &c.OwnerMCE,
			&c.Total, &c.Available, &c.Allocated, &c.InTransit); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- ReservationStore (holdings) --------------------------------------------

func (p *PG) UpsertReservation(ctx context.Context, r Reservation) error {
	const q = `
	INSERT INTO host_reservation (id, class, count, site, purpose, hard, expires_at)
	VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7)
	ON CONFLICT (id) DO UPDATE SET
	  class=EXCLUDED.class, count=EXCLUDED.count, site=EXCLUDED.site,
	  purpose=EXCLUDED.purpose, hard=EXCLUDED.hard, expires_at=EXCLUDED.expires_at`
	_, err := p.pool.Exec(ctx, q, r.ID, r.Class, r.Count, r.Site, r.Purpose, r.Hard, r.ExpiresAt)
	return err
}

func (p *PG) DeleteReservation(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM host_reservation WHERE id=$1`, id)
	return err
}

func (p *PG) ListReservations(ctx context.Context) ([]Reservation, error) {
	const q = `SELECT id, class, count, coalesce(site,''), coalesce(purpose,''), hard, expires_at
	             FROM host_reservation ORDER BY class, id`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.ID, &r.Class, &r.Count, &r.Site, &r.Purpose, &r.Hard, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- ForecastStore (region headroom / shortage) -----------------------------

func (p *PG) RegionHeadroom(ctx context.Context, class string) ([]Headroom, error) {
	q := `SELECT class, total, allocated, maintenance, discovered, in_transit, spare, reserved, free_headroom, shortage
	        FROM region_headroom`
	var args []any
	if class != "" {
		q += ` WHERE class = $1`
		args = append(args, class)
	}
	q += ` ORDER BY class`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Headroom
	for rows.Next() {
		var h Headroom
		if err := rows.Scan(&h.Class, &h.Total, &h.Allocated, &h.Maintenance, &h.Discovered, &h.InTransit,
			&h.Spare, &h.Reserved, &h.FreeHeadroom, &h.Shortage); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- LifecycleStore (maintenance / grip on non-installed hosts) -------------

func (p *PG) SetHostPhase(ctx context.Context, serviceTag string, phase HostPhase, note string) error {
	const q = `
	INSERT INTO host_state (service_tag, phase, note, updated_at)
	VALUES ($1,$2,NULLIF($3,''), now())
	ON CONFLICT (service_tag) DO UPDATE SET
	  phase=EXCLUDED.phase, note=EXCLUDED.note, updated_at=now()`
	_, err := p.pool.Exec(ctx, q, serviceTag, string(phase), note)
	return err
}

// EligibleMCEs returns MCEs that can adopt a host, by provisioning reach.
func (p *PG) EligibleMCEs(ctx context.Context, serviceTag string) ([]string, error) {
	const q = `SELECT mce FROM host_eligible_mce WHERE service_tag = $1 ORDER BY mce`
	rows, err := p.pool.Query(ctx, q, serviceTag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
