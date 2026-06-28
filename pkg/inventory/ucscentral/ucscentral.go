// Package ucscentral implements the inventory.Collector for Cisco hardware
// managed by UCS Central (multi-domain on-prem aggregator).
//
// UCS Central differs from per-domain UCSM in three important ways:
//
//  1. ENDPOINT: UCS Central exposes its XML API at /centralApi/ (not /nuova).
//     Verify against your instance — older versions may differ.
//     TODO: confirm endpoint path against the production UCS Central.
//
//  2. DNs ARE DOMAIN-SCOPED: every managed object DN is prefixed with
//     "domainGroup-root/domain-<domain-name>/" e.g.:
//       domainGroup-root/domain-dc1-fi/sys/rack-unit-1/adaptor-1/...
//     Strip this prefix when mapping topology to TopologyLink.LeafPort.
//
//  3. CROSS-DOMAIN RESULTS: configResolveClass returns objects from ALL
//     managed UCSM domains in a single response. Group by domain DN prefix
//     to track which domain each server belongs to (useful for site tagging).
//
// Auth: POST https://<ucs-central>/centralApi/ with <aaaLogin>. The response
// includes outDomains (comma-separated list of managed domain names) in
// addition to the outCookie. Re-login on 403 or cookie expiry.
//
// THE GOTCHA: UCS Central abstracts hardware behind service profiles.
// "What CPU/RAM does logical server X have" resolves through
// lsServer.assignedToDn -> physical blade DN. Follow that binding;
// do NOT read the logical server as if it were physical.
package ucscentral

import (
	"context"
	"net/http"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory"
)

type Collector struct {
	baseURL string // https://ucs-central.airgap.local
	creds   inventory.Credentials
	http    *http.Client
	cookie  string // aaaLogin outCookie, refreshed on expiry
	domains []string // outDomains from aaaLoginResponse
}

func New(baseURL string, creds inventory.Credentials, c *http.Client) *Collector {
	if c == nil {
		c = http.DefaultClient // TODO: inject client with internal CA bundle
	}
	return &Collector{baseURL: baseURL, creds: creds, http: c}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceUCSCentral }

func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	// 1) Auth: POST /centralApi/  <aaaLogin inName="..." inPassword="..."/>
	//    -> outCookie stored in c.cookie; outDomains in c.domains.
	//    Re-login on 403 or cookie expiry (outRefreshPeriod seconds).
	//
	// 2) Enumerate physical blades + rack servers across all domains:
	//    POST /centralApi/  <configResolveClass cookie="..." classId="computeBlade"/>
	//    POST /centralApi/  <configResolveClass cookie="..." classId="computeRackUnit"/>
	//    -> results span all managed UCSM domains.
	//    -> DN format: domainGroup-root/domain-<name>/sys/rack-unit-<id>/...
	//    -> fields: dn, model, serial, numOfCpus, totalMemory, adminPower
	//
	// 3) Per server, resolve hardware details (use server DN as parentDn):
	//    classId="processorUnit"     parentDn=<server-dn>  -> cores/model/vendor
	//    classId="memoryUnit"        parentDn=<server-dn>  -> size per DIMM; sum RAMGiB
	//    classId="storageLocalDisk"  parentDn=<server-dn>  -> size/type/model
	//    classId="adaptorEthInterface" parentDn=<server-dn> -> MAC addresses
	//
	// 4) Topology (pre-boot, from fabric interconnect port mapping):
	//    classId="fabricPathEp" parentDn=<server-dn>
	//    -> dn contains domain prefix; strip "domainGroup-root/domain-<name>/" prefix.
	//    -> switchId (A/B), slotId, portId = fabric interconnect port number.
	//    -> Map to TopologyLink{NICMac: <mac>, LeafName: "fi-<switchId>",
	//       LeafPort: "<slotId>/<portId>", LeafMgmt: ""}
	//    -> LeafMgmt resolvable via classId="networkElement" if needed.
	//
	// 5) Domain → site mapping:
	//    Extract domain name from DN prefix and map to site via a
	//    domain→site config map (injected at collector construction time, TBD).
	panic("not implemented: UCS Central XML API enumerate + map -> v1alpha1.DiscoveredInventory")
}

// compile-time check
var _ inventory.Collector = (*Collector)(nil)
