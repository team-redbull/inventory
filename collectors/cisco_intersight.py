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
from intersight.api import adapter_api, compute_api, storage_api
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
        signing_algorithm="rsa-sha256",  # change to ecdsa-sha256 for EC keys
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


def _storage_gib(client: ApiClient, device_moid: str | None) -> int:
    """Sum physical disk sizes (MiB) for the given registered device moid."""
    if not device_moid:
        return 0
    try:
        sapi = storage_api.StorageApi(client)
        disks = _list_all(
            sapi.get_storage_physical_disk_list,
            filter=f"RegisteredDevice.Moid eq '{device_moid}'",
        )
        # StoragePhysicalDisk.size is in MiB per Intersight API
        total_mib = sum(getattr(d, "size", 0) or 0 for d in disks)
        return total_mib // 1024
    except Exception as e:
        log.debug("storage query failed for device %s: %s", device_moid, e)
        return 0


def _device_moid(server) -> str | None:
    """Extract registered device moid from a compute object for cross-resource joins."""
    rd = getattr(server, "registered_device", None)
    if rd and hasattr(rd, "moid"):
        return rd.moid
    return None


def _nics_and_topology(client: ApiClient, device_moid: str | None) -> tuple[list[common.NICInfo], list[common.TopologyLink]]:
    """Extract NIC inventory and fabric port mapping from AdapterHostEthInterface.

    One API call per server yields both NIC facts (name, MAC, speed) and topology
    (peer FI/IMM port DN). For FI-attached blades peer_interface resolves to the
    FI port; for IMM-managed rack units it is the ToR switch port when LLDP is
    enabled. max_speed is in Mbps.
    """
    if not device_moid:
        return [], []
    try:
        aapi = adapter_api.AdapterApi(client)
        ifaces = _list_all(
            aapi.get_adapter_host_eth_interface_list,
            filter=f"RegisteredDevice.Moid eq '{device_moid}'",
        )
        nics = []
        links = []
        for iface in ifaces:
            mac = getattr(iface, "mac_address", "") or ""
            if not mac:
                continue
            name = getattr(iface, "name", "") or ""
            speed = int(getattr(iface, "max_speed", 0) or 0)
            nics.append(common.NICInfo(mac=mac, name=name, speed_mbs=speed))

            leaf_name = ""
            leaf_port = ""
            peer = getattr(iface, "peer_interface", None)
            if peer:
                peer_dn = getattr(peer, "dn", "") or ""
                parts = peer_dn.split("/")
                leaf_name = parts[1] if len(parts) > 1 else peer_dn
                leaf_port = getattr(peer, "port_id", "") or ""
            links.append(common.TopologyLink(nic_mac=mac, leaf_name=leaf_name, leaf_port=leaf_port))
        return nics, links
    except Exception as e:
        log.debug("nic/topology query failed for device %s: %s", device_moid, e)
        return [], []


def collect(client: ApiClient) -> list[common.DiscoveredFact]:
    compute = compute_api.ComputeApi(client)

    rack_units = _list_all(compute.get_compute_rack_unit_list)
    blades = _list_all(compute.get_compute_blade_list)

    facts = []
    for s in rack_units + blades:
        try:
            service_tag = s.serial or ""
            if not service_tag:
                continue

            vendor = s.vendor or "Cisco"
            model = s.model or ""

            # num_threads = total logical processors across all sockets.
            # Enterprise UCS servers run HT (2 threads/core), so ÷2 gives physical cores.
            # Falls back to num_cpus (socket count) if num_threads is unavailable.
            threads = getattr(s, "num_threads", None) or 0
            cores = threads // 2 if threads > 0 else (s.num_cpus or 0)

            # total_memory is in MiB
            ram_gib = (s.total_memory or 0) // 1024

            dmoid = _device_moid(s)
            storage = _storage_gib(client, dmoid)
            nics, topo = _nics_and_topology(client, dmoid)

            facts.append(common.DiscoveredFact(
                service_tag=service_tag,
                vendor=vendor,
                model=model,
                cores=cores,
                ram_gib=ram_gib,
                storage_gib=storage,
                nics=nics,
                topology=topo,
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
