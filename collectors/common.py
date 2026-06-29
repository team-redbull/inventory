"""Shared Postgres writer and config helpers for all collectors.

Python collectors write ONLY discovered hardware facts (vendor, model, cores,
ram_gib, storage_gib) and NIC-to-leaf topology. Declared fields (site, segment,
class, bmc_address, bmc_type) are written by the Go projector reading the
InventoryRecord spec and must not be overwritten here.
"""
import logging
import os
from dataclasses import dataclass, field
from typing import Optional

import psycopg

log = logging.getLogger(__name__)


@dataclass
class TopologyLink:
    nic_mac: str
    leaf_name: str = ""
    leaf_port: str = ""
    leaf_mgmt: str = ""


@dataclass
class DiscoveredFact:
    service_tag: str
    vendor: str = ""
    model: str = ""
    cores: int = 0
    ram_gib: int = 0
    storage_gib: int = 0
    topology: list[TopologyLink] = field(default_factory=list)


def pg_dsn() -> str:
    dsn = os.environ.get("POSTGRES_URL")
    if not dsn:
        raise RuntimeError("POSTGRES_URL env var not set")
    return dsn


def upsert(conn: psycopg.Connection, f: DiscoveredFact) -> None:
    """Insert or enrich one host row.

    On first insert (no InventoryRecord yet): creates the row with NULL
    declared fields; the Go projector fills them in on its next run.
    On conflict: updates only the discovered fields, leaving declared
    fields (site, segment, class, bmc_*) untouched.
    """
    conn.execute(
        """
        INSERT INTO host_inventory
            (service_tag, vendor, model, cores, ram_gib, storage_gib, last_seen)
        VALUES
            (%(service_tag)s, %(vendor)s, %(model)s, %(cores)s, %(ram_gib)s,
             %(storage_gib)s, now())
        ON CONFLICT (service_tag) DO UPDATE SET
            vendor      = COALESCE(NULLIF(EXCLUDED.vendor, ''),      host_inventory.vendor),
            model       = COALESCE(NULLIF(EXCLUDED.model, ''),       host_inventory.model),
            cores       = CASE WHEN EXCLUDED.cores > 0       THEN EXCLUDED.cores       ELSE host_inventory.cores       END,
            ram_gib     = CASE WHEN EXCLUDED.ram_gib > 0     THEN EXCLUDED.ram_gib     ELSE host_inventory.ram_gib     END,
            storage_gib = CASE WHEN EXCLUDED.storage_gib > 0 THEN EXCLUDED.storage_gib ELSE host_inventory.storage_gib END,
            last_seen   = now()
        """,
        {
            "service_tag": f.service_tag,
            "vendor":      f.vendor,
            "model":       f.model,
            "cores":       f.cores,
            "ram_gib":     f.ram_gib,
            "storage_gib": f.storage_gib,
        },
    )

    if f.topology:
        _upsert_topology(conn, f.service_tag, f.topology)


def _upsert_topology(conn: psycopg.Connection, service_tag: str, links: list[TopologyLink]) -> None:
    """Replace all topology rows for this host atomically."""
    conn.execute("DELETE FROM host_topology WHERE service_tag = %s", (service_tag,))
    conn.executemany(
        """
        INSERT INTO host_topology (service_tag, nic_mac, leaf_name, leaf_port, leaf_mgmt, updated_at)
        VALUES (%s, %s, %s, %s, %s, now())
        """,
        [
            (service_tag, lnk.nic_mac, lnk.leaf_name or None, lnk.leaf_port or None, lnk.leaf_mgmt or None)
            for lnk in links
            if lnk.nic_mac
        ],
    )


def flush(facts: list[DiscoveredFact], dsn: str) -> None:
    """Write a batch of discovered facts (+ topology) in a single transaction."""
    if not facts:
        return
    with psycopg.connect(dsn) as conn:
        for f in facts:
            upsert(conn, f)
        conn.commit()
    topo_count = sum(len(f.topology) for f in facts)
    log.info("flushed %d host facts, %d topology links", len(facts), topo_count)
