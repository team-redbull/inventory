"""OpenManage Enterprise collector.

Reads Dell server inventory from the OME REST API and upserts discovered
hardware facts into host_inventory.

Configuration (env vars):
  OME_URL       https://ome.airgap.local
  OME_USERNAME  admin
  OME_PASSWORD  ...
  OME_VERIFY_SSL  true|false  (default true; set false for self-signed certs in dev)
  POSTGRES_URL  postgres://user:pass@host/db
  POLL_INTERVAL seconds between polls (default 300)
"""
import logging
import os
import time

import requests

import common

log = logging.getLogger(__name__)
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

OME_DEVICE_TYPE_SERVER = 1000


class OMESession:
    def __init__(self, base_url: str, username: str, password: str, verify: bool = True):
        self.base_url = base_url.rstrip("/")
        self.session = requests.Session()
        self.session.verify = verify

        r = self.session.post(
            f"{self.base_url}/api/SessionService/Sessions",
            json={"UserName": username, "Password": password, "SessionType": "API"},
            timeout=30,
        )
        r.raise_for_status()
        self.session.headers["X-Auth-Token"] = r.headers["X-Auth-Token"]
        log.info("OME session established: %s", self.base_url)

    def get(self, path: str, **kwargs) -> dict:
        r = self.session.get(f"{self.base_url}{path}", timeout=30, **kwargs)
        r.raise_for_status()
        return r.json()

    def devices(self) -> list[dict]:
        """Return all server-type devices (paginated)."""
        results = []
        url = "/api/DeviceService/Devices"
        params = {"$filter": f"Type eq {OME_DEVICE_TYPE_SERVER}", "$top": 100, "$skip": 0}
        while url:
            data = self.get(url, params=params)
            results.extend(data.get("value", []))
            next_link = data.get("@odata.nextLink")
            if next_link:
                # nextLink is a full path or relative path
                url = next_link if next_link.startswith("/") else "/" + next_link.split("/", 3)[-1]
                params = {}
            else:
                url = None
        return results

    def inventory_details(self, device_id: int) -> list[dict]:
        data = self.get(f"/api/DeviceService/Devices/{device_id}/InventoryDetails")
        return data.get("InventoryInfo", [])


def _sum_by_type(inventory: list[dict], inv_type: str) -> list[dict]:
    for section in inventory:
        if section.get("InventoryType") == inv_type:
            return section.get("InventoryData", [])
    return []


def _map(device: dict, inventory: list[dict]) -> common.DiscoveredFact:
    service_tag = device.get("DeviceServiceTag", "")
    vendor = "Dell"
    model = device.get("Model", "")

    # Processors: sum cores across all sockets
    procs = _sum_by_type(inventory, "serverProcessors")
    cores = sum(int(p.get("NumberOfCores", 0)) for p in procs)

    # Memory: sum DIMM sizes (MiB → GiB)
    dims = _sum_by_type(inventory, "serverMemoryInfo")
    ram_mib = sum(int(d.get("Size", 0)) for d in dims)
    ram_gib = ram_mib // 1024

    # Storage: sum disk sizes (bytes → GiB)
    disks = _sum_by_type(inventory, "serverStorageDiskView")
    storage_gib = sum(int(d.get("Size", 0)) for d in disks) // (1024 ** 3)

    return common.DiscoveredFact(
        service_tag=service_tag,
        vendor=vendor,
        model=model,
        cores=cores,
        ram_gib=ram_gib,
        storage_gib=storage_gib,
    )


def collect(session: OMESession) -> list[common.DiscoveredFact]:
    facts = []
    for device in session.devices():
        tag = device.get("DeviceServiceTag", "")
        if not tag:
            continue
        try:
            inv = session.inventory_details(device["Id"])
            facts.append(_map(device, inv))
        except Exception as e:
            log.warning("skipping device %s: %s", tag, e)
    return facts


def main() -> None:
    base_url = os.environ["OME_URL"]
    username = os.environ["OME_USERNAME"]
    password = os.environ["OME_PASSWORD"]
    verify = os.environ.get("OME_VERIFY_SSL", "true").lower() != "false"
    dsn = common.pg_dsn()
    interval = int(os.environ.get("POLL_INTERVAL", "300"))

    session = OMESession(base_url, username, password, verify=verify)

    while True:
        try:
            facts = collect(session)
            common.flush(facts, dsn)
        except requests.HTTPError as e:
            if e.response is not None and e.response.status_code in (401, 403):
                log.warning("OME session expired, re-authenticating")
                session = OMESession(base_url, username, password, verify=verify)
            else:
                log.error("OME collect error: %s", e)
        except Exception as e:
            log.error("OME collect error: %s", e)
        time.sleep(interval)


if __name__ == "__main__":
    main()
