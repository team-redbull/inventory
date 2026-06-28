"""Intersight PVA collector (Cisco).

Reads Cisco server inventory from the Intersight Private Virtual Appliance
REST API using the official intersight Python SDK (HMAC-signed requests).

Configuration (env vars):
  INTERSIGHT_URL          https://intersight.airgap.local
  INTERSIGHT_KEY_ID       <API key ID from Intersight portal>
  INTERSIGHT_KEY_FILE     path to PEM private key file (mounted Secret)
  INTERSIGHT_VERIFY_SSL   true|false  (default true)
  POSTGRES_URL            postgres://user:pass@host/db
  POLL_INTERVAL           seconds between polls (default 300)
"""
import logging
import os
import time

import intersight
from intersight.api import compute_api, adapter_api
from intersight.api_client import ApiClient
from intersight.configuration import Configuration
from intersight.signing import HttpSigningConfiguration

import common

log = logging.getLogger(__name__)
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")


def _build_client(base_url: str, key_id: str, key_file: str, verify: bool) -> ApiClient:
    signing = HttpSigningConfiguration(
        key_id=key_id,
        private_key_path=key_file,
        signing_scheme="hs2019",
        signing_algorithm="rsa-sha256",  # adjust to ecdsa-sha256 if using EC key
    )
    config = Configuration(host=base_url, signing_info=signing)
    config.verify_ssl = verify
    return ApiClient(configuration=config)


def _list_all(api_fn, **kwargs) -> list:
    """Fetch all pages from an Intersight list endpoint."""
    results = []
    skip = 0
    top = 100
    while True:
        resp = api_fn(top=top, skip=skip, **kwargs)
        page = resp.results or []
        results.extend(page)
        if len(page) < top:
            break
        skip += top
    return results


def collect(client: ApiClient) -> list[common.DiscoveredFact]:
    compute = compute_api.ComputeApi(client)
    adapters = adapter_api.AdapterApi(client)

    rack_units = _list_all(compute.get_compute_rack_unit_list)
    blades = _list_all(compute.get_compute_blade_list)
    servers = rack_units + blades

    # Build NIC index keyed by parent server Moid for topology (not written
    # to host_inventory — topology goes to InventoryRecord via BMC collector).
    # Kept here as a reference for future enrichment.
    # nics = {n.parent.moid: n for n in _list_all(adapters.get_adapter_host_eth_interface_list)}

    facts = []
    for s in servers:
        try:
            service_tag = s.serial or ""
            if not service_tag:
                continue

            vendor = s.vendor or "Cisco"
            model = s.model or ""

            # num_cpus = socket count; individual processor details via separate API
            # Use num_cpus * (cores per CPU from processor model) for accuracy,
            # or fall back to num_cpus as socket count.
            # TODO: expand /api/v1/processor/Units?$filter=Parent.Moid eq '<moid>'
            # for exact core count. For now use num_cpus as a floor.
            cores = (s.num_cpus or 0) * 1  # placeholder: 1 socket = reported as num_cpus
            # total_memory is in MiB
            ram_gib = (s.total_memory or 0) // 1024

            # storage_gib: not directly on ComputeRackUnit/Blade — requires
            # /api/v1/storage/PhysicalDisks. TODO: implement disk expansion.
            storage_gib = 0

            facts.append(common.DiscoveredFact(
                service_tag=service_tag,
                vendor=vendor,
                model=model,
                cores=cores,
                ram_gib=ram_gib,
                storage_gib=storage_gib,
            ))
        except Exception as e:
            log.warning("skipping server %s: %s", getattr(s, "serial", "?"), e)

    return facts


def main() -> None:
    base_url = os.environ["INTERSIGHT_URL"]
    key_id = os.environ["INTERSIGHT_KEY_ID"]
    key_file = os.environ["INTERSIGHT_KEY_FILE"]
    verify = os.environ.get("INTERSIGHT_VERIFY_SSL", "true").lower() != "false"
    dsn = common.pg_dsn()
    interval = int(os.environ.get("POLL_INTERVAL", "300"))

    client = _build_client(base_url, key_id, key_file, verify)
    log.info("Intersight client configured: %s", base_url)

    while True:
        try:
            facts = collect(client)
            common.flush(facts, dsn)
        except Exception as e:
            log.error("Intersight collect error: %s", e)
        time.sleep(interval)


if __name__ == "__main__":
    main()
