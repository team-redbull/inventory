"""UCS Central collector (Cisco).

Reads Cisco server inventory from UCS Central using the official ucscentralsdk
Python SDK. UCS Central manages multiple UCSM domains; this collector returns
servers from ALL managed domains in a single run.

Configuration (env vars):
  UCS_CENTRAL_IP        IP or hostname of UCS Central
  UCS_CENTRAL_USERNAME  admin
  UCS_CENTRAL_PASSWORD  ...
  UCS_CENTRAL_PORT      port (default 443)
  UCS_CENTRAL_SECURE    true|false (default true)
  POSTGRES_URL          postgres://user:pass@host/db
  POLL_INTERVAL         seconds between polls (default 300)

Domain note:
  configResolveClass results have DNs prefixed with
  "domainGroup-root/domain-<name>/". This collector strips that prefix
  when resolving parent DNs for child queries (processors, memory, etc.).
"""
import logging
import os
import time

from ucscentralsdk.ucscentralhandle import UcsCentralHandle

import common

log = logging.getLogger(__name__)
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")


def _login(ip: str, username: str, password: str, port: int, secure: bool) -> UcsCentralHandle:
    handle = UcsCentralHandle(ip=ip, username=username, password=password,
                               port=port, secure=secure)
    handle.login()
    log.info("UCS Central connected: %s", ip)
    return handle


def _cores(handle: UcsCentralHandle, server_dn: str) -> int:
    """Sum cores across all processor units under this server DN."""
    procs = handle.query_children(in_dn=server_dn, class_id="processorUnit")
    return sum(int(getattr(p, "cores", 0) or 0) for p in procs)


def _ram_gib(handle: UcsCentralHandle, server_dn: str) -> int:
    """Sum DIMM capacities (MiB) and convert to GiB."""
    dims = handle.query_children(in_dn=server_dn, class_id="memoryUnit")
    total_mib = sum(int(getattr(d, "capacity", 0) or 0) for d in dims
                    if getattr(d, "presence", "") == "equipped")
    return total_mib // 1024


def _storage_gib(handle: UcsCentralHandle, server_dn: str) -> int:
    """Sum local disk sizes (bytes) and convert to GiB."""
    disks = handle.query_children(in_dn=server_dn, class_id="storageLocalDisk")
    total_bytes = sum(int(getattr(d, "size", 0) or 0) for d in disks
                      if getattr(d, "disk_state", "") == "Good")
    return total_bytes // (1024 ** 3)


def _nics_and_topology(handle: UcsCentralHandle, server_dn: str) -> tuple[list[common.NICInfo], list[common.TopologyLink]]:
    """Extract NIC inventory and FI port mapping from adaptorExtEthIf objects.

    adaptorExtEthIf represents host-side Ethernet interfaces on UCS blades and
    rack adaptors. oper_speed is a string like "1gbps"/"10gbps"/"25gbps" —
    parsed to Mbps. peer_dn points at the connected FI or FEX port.
    """
    nics = []
    links = []
    try:
        ifaces = handle.query_children(in_dn=server_dn, class_id="adaptorExtEthIf")
        for iface in ifaces:
            mac = getattr(iface, "mac", "") or ""
            if not mac:
                continue

            name = getattr(iface, "dn", "").split("/")[-1]  # last DN component as port name
            speed_str = (getattr(iface, "oper_speed", "") or "").lower()
            speed_mbs = _parse_speed(speed_str)
            nics.append(common.NICInfo(mac=mac, name=name, speed_mbs=speed_mbs))

            peer_dn = getattr(iface, "peer_dn", "") or ""
            parts = peer_dn.split("/")
            leaf_name = parts[1] if len(parts) > 1 else ""
            links.append(common.TopologyLink(nic_mac=mac, leaf_name=leaf_name, leaf_port=peer_dn))
    except Exception as e:
        log.debug("nic/topology query failed for %s: %s", server_dn, e)
    return nics, links


def _parse_speed(s: str) -> int:
    """Convert UCS speed string ('1gbps', '10gbps', '25gbps') to Mbps int."""
    s = s.replace("gbps", "").replace("mbps", "M").strip()
    if s.endswith("M"):
        try:
            return int(s[:-1])
        except ValueError:
            return 0
    try:
        return int(float(s) * 1000)
    except ValueError:
        return 0


def _map_server(handle: UcsCentralHandle, server) -> common.DiscoveredFact | None:
    service_tag = getattr(server, "serial", "") or ""
    if not service_tag:
        return None

    dn = server.dn  # e.g. domainGroup-root/domain-dc1-fi/sys/rack-unit-1
    vendor = getattr(server, "vendor", "Cisco") or "Cisco"
    model = getattr(server, "model", "") or ""

    try:
        cores = _cores(handle, dn)
        ram = _ram_gib(handle, dn)
        storage = _storage_gib(handle, dn)
    except Exception as e:
        log.warning("partial inventory for %s: %s", service_tag, e)
        # Still emit the fact with what we have from the top-level object.
        cores = int(getattr(server, "num_of_cpus", 0) or 0)
        ram = int(getattr(server, "total_memory", 0) or 0) // 1024
        storage = 0

    nics, topo = _nics_and_topology(handle, dn)
    return common.DiscoveredFact(
        service_tag=service_tag,
        vendor=vendor,
        model=model,
        cores=cores,
        ram_gib=ram,
        storage_gib=storage,
        nics=nics,
        topology=topo,
    )


def collect(handle: UcsCentralHandle) -> list[common.DiscoveredFact]:
    facts = []

    for class_id in ("computeRackUnit", "computeBlade"):
        try:
            servers = handle.query_classid(class_id)
        except Exception as e:
            log.error("query_classid(%s) failed: %s", class_id, e)
            continue

        for server in servers:
            fact = _map_server(handle, server)
            if fact:
                facts.append(fact)

    return facts


def main() -> None:
    ip = os.environ["UCS_CENTRAL_IP"]
    username = os.environ["UCS_CENTRAL_USERNAME"]
    password = os.environ["UCS_CENTRAL_PASSWORD"]
    port = int(os.environ.get("UCS_CENTRAL_PORT", "443"))
    secure = os.environ.get("UCS_CENTRAL_SECURE", "true").lower() != "false"
    dsn = common.pg_dsn()
    interval = int(os.environ.get("POLL_INTERVAL", "300"))

    handle = _login(ip, username, password, port, secure)

    while True:
        try:
            facts = collect(handle)
            common.flush(facts, dsn)
        except Exception as e:
            log.error("UCS Central collect error: %s", e)
            # Re-login on session errors
            try:
                handle.logout()
            except Exception:
                pass
            try:
                handle = _login(ip, username, password, port, secure)
            except Exception as login_err:
                log.error("UCS Central re-login failed: %s", login_err)
        time.sleep(interval)


if __name__ == "__main__":
    main()
