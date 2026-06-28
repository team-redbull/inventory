-- Central fleet store. The ONE shared write point, off the data path.
--   host_inventory   : collectors push discovered/enrollment facts
--   host_lease       : ownership CAS (single-writer guarantee on the BMC)
--   host_allocation  : consumption outcome (which HostedCluster/NodePool)
--   host_reservation : region-scoped HOLDINGS (earmarked, not yet bound)
--   host_state       : operational lifecycle (in_service / maintenance /
--                      decommissioning) — the "grip" on hosts not in a cluster
-- Keep this on HA Postgres (Patroni); serve reads from replicas.

CREATE TABLE IF NOT EXISTS host_inventory (
    service_tag   TEXT PRIMARY KEY,
    site          TEXT,
    class         TEXT,
    vendor        TEXT,
    model         TEXT,
    cores         INT,
    ram_gib       INT,
    storage_gib   BIGINT,
    segment       TEXT,
    bmc_address   TEXT,
    bmc_type      TEXT,
    last_seen     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_inv_class_site ON host_inventory (class, site);

CREATE TABLE IF NOT EXISTS host_lease (
    service_tag   TEXT PRIMARY KEY REFERENCES host_inventory(service_tag) ON DELETE CASCADE,
    owner_mce     TEXT,
    state         TEXT NOT NULL DEFAULT 'Free'
                  CHECK (state IN ('Owned','Releasing','Free','Claiming')),
    generation    BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_lease_owner_state ON host_lease (owner_mce, state);

CREATE TABLE IF NOT EXISTS host_allocation (
    service_tag    TEXT PRIMARY KEY REFERENCES host_inventory(service_tag) ON DELETE CASCADE,
    hosted_cluster TEXT,
    node_pool      TEXT,
    node_name      TEXT,
    claim_ref      TEXT,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Operational lifecycle. Absent row => in_service. The home MCE still holds the
-- BMH for maintenance/spare hosts (the operational grip); this table is the
-- fleet-wide grip + governs whether a host counts as claimable/spare.
CREATE TABLE IF NOT EXISTS host_state (
    service_tag TEXT PRIMARY KEY REFERENCES host_inventory(service_tag) ON DELETE CASCADE,
    phase       TEXT NOT NULL DEFAULT 'in_service'
                CHECK (phase IN ('discovered','in_service','maintenance','decommissioning')),
    note        TEXT,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS host_reservation (
    id          TEXT PRIMARY KEY,
    class       TEXT NOT NULL,
    count       INT  NOT NULL CHECK (count > 0),
    site        TEXT,
    purpose     TEXT,
    hard        BOOLEAN NOT NULL DEFAULT false,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_res_class ON host_reservation (class);

CREATE TABLE IF NOT EXISTS host_reservation_member (
    reservation_id TEXT REFERENCES host_reservation(id) ON DELETE CASCADE,
    service_tag    TEXT REFERENCES host_inventory(service_tag) ON DELETE CASCADE,
    PRIMARY KEY (reservation_id, service_tag)
);

-- Pending spill requests: written by the claim reconciler when local pool is
-- short and allowSpill=true. The fleet allocator (component #12) reads this
-- table to decide which hosts to move. One active row per claim; upserted on
-- each reconcile, deleted when the claim reaches Satisfied.
CREATE TABLE IF NOT EXISTS host_spill_request (
    claim_name   TEXT NOT NULL,
    claim_ns     TEXT NOT NULL DEFAULT '',
    class        TEXT NOT NULL,
    short_by     INT  NOT NULL CHECK (short_by > 0),
    mce          TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (claim_name, claim_ns)
);
CREATE INDEX IF NOT EXISTS idx_spill_class_mce ON host_spill_request (class, mce);

-- Which provisioning segments each MCE serves. Authored config (one row per
-- (mce, segment)). This is the data that answers "which MCE can adopt this host".
CREATE TABLE IF NOT EXISTS mce_reach (
    mce      TEXT NOT NULL,
    site     TEXT,
    segment  TEXT NOT NULL,
    PRIMARY KEY (mce, segment)
);

-- Eligible MCEs per host: one installation segment (native VLAN) per site,
-- so segment match covers all MCEs in that site for any boot method.
CREATE OR REPLACE VIEW host_eligible_mce AS
SELECT DISTINCT i.service_tag, r.mce
FROM host_inventory i
JOIN mce_reach r ON r.segment = i.segment;

-- Hard-pinned service tags: hosts locked to a specific hard reservation.
-- Excluded from available so they cannot be claimed by another NodePool.
CREATE OR REPLACE VIEW hard_held_hosts AS
SELECT DISTINCT rm.service_tag
FROM host_reservation_member rm
JOIN host_reservation hr ON hr.id = rm.reservation_id
WHERE hr.hard
  AND (hr.expires_at IS NULL OR hr.expires_at > now());

-- Per (site, class, owner_mce). available/spare exclude maintenance + hard-held hosts.
CREATE OR REPLACE VIEW host_capacity AS
SELECT
    i.site,
    i.class,
    l.owner_mce,
    count(*)                                                            AS total,
    count(*) FILTER (WHERE a.service_tag IS NULL AND l.state='Owned'
                       AND coalesce(s.phase,'in_service')='in_service'
                       AND i.service_tag NOT IN (SELECT service_tag FROM hard_held_hosts)) AS available,
    count(*) FILTER (WHERE a.service_tag IS NULL AND l.state='Free'
                       AND coalesce(s.phase,'in_service')='in_service') AS spare_free,
    count(*) FILTER (WHERE coalesce(s.phase,'in_service')='maintenance')AS maintenance,
    count(*) FILTER (WHERE a.service_tag IS NOT NULL)                   AS allocated,
    count(*) FILTER (WHERE l.state IN ('Releasing','Claiming'))         AS in_transit
FROM host_inventory i
LEFT JOIN host_lease      l USING (service_tag)
LEFT JOIN host_allocation a USING (service_tag)
LEFT JOIN host_state      s USING (service_tag)
GROUP BY i.site, i.class, l.owner_mce;

-- Region planning. spare EXCLUDES maintenance, so shortage forecasts are honest.
CREATE OR REPLACE VIEW region_headroom AS
WITH cap AS (
    SELECT
        i.class,
        count(*)                                                              AS total,
        count(*) FILTER (WHERE a.service_tag IS NOT NULL)                     AS allocated,
        count(*) FILTER (WHERE coalesce(s.phase,'in_service')='maintenance')  AS maintenance,
        count(*) FILTER (WHERE s.phase='discovered')                          AS discovered,
        count(*) FILTER (WHERE l.state IN ('Releasing','Claiming'))           AS in_transit,
        count(*) FILTER (WHERE a.service_tag IS NULL
                           AND l.state IN ('Owned','Free')
                           AND coalesce(s.phase,'in_service')='in_service')   AS spare
    FROM host_inventory i
    LEFT JOIN host_lease      l USING (service_tag)
    LEFT JOIN host_allocation a USING (service_tag)
    LEFT JOIN host_state      s USING (service_tag)
    GROUP BY i.class
),
res AS (
    SELECT class, sum(count) AS reserved
    FROM host_reservation
    WHERE expires_at IS NULL OR expires_at > now()
    GROUP BY class
)
SELECT
    cap.class,
    cap.total,
    cap.allocated,
    cap.maintenance,
    cap.discovered,
    cap.in_transit,
    cap.spare,
    coalesce(res.reserved, 0)                       AS reserved,
    cap.spare - coalesce(res.reserved, 0)           AS free_headroom,
    (cap.spare - coalesce(res.reserved, 0)) < 0     AS shortage
FROM cap
LEFT JOIN res USING (class);
