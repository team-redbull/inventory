// Package ucsm implements the inventory.Collector for Cisco hardware managed
// by UCS Manager or UCS Central (the XML API / ucsmsdk dialect).
//
// Auth: POST https://<ucsm>/nuova with XML body containing <aaaLogin> to get
// a cookie; send cookie on every subsequent request. Session refresh needed
// for long-running collectors.
//
// THE GOTCHA: UCSM abstracts hardware behind service profiles. "What CPU/RAM
// does logical server X have" resolves through lsServer.assignedToDn -> the
// physical blade DN. Always follow that binding; do NOT read the logical server
// as if it were physical.
package ucsm

import (
	"context"
	"net/http"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory"
)

type Collector struct {
	baseURL string // https://ucsm.airgap.local
	creds   inventory.Credentials
	http    *http.Client
	cookie  string // aaaLogin outCookie, refreshed on expiry
}

func New(baseURL string, creds inventory.Credentials, c *http.Client) *Collector {
	if c == nil {
		c = http.DefaultClient // TODO: inject client with internal CA bundle
	}
	return &Collector{baseURL: baseURL, creds: creds, http: c}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceUCSM }

func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	// 1) Auth: POST /nuova  <aaaLogin inName="..." inPassword="..."/>
	//    -> outCookie stored in c.cookie; re-login on 403 / cookie expiry.
	// 2) Enumerate physical blades + rack servers:
	//    POST /nuova  <configResolveClass cookie="..." classId="computeBlade"/>
	//    POST /nuova  <configResolveClass cookie="..." classId="computeRackUnit"/>
	//    -> each object: serverId, model, serial, numOfCpus, totalMemory, adminPower
	// 3) Per server, resolve hardware details via DN:
	//    classId="processorUnit"  parentDn=<server dn>  -> cores/model/vendor
	//    classId="memoryUnit"     parentDn=<server dn>  -> size per DIMM; sum RAMGiB
	//    classId="storageLocalDisk" parentDn=...        -> size/type/model
	//    classId="adaptorEthInterface" parentDn=...     -> MAC addresses
	// 4) topology (pre-boot, no switch creds):
	//    classId="fabricPathEp" parentDn=<server dn>
	//    -> each path: switchId (A/B), slotId, portId = fabric interconnect port.
	//    Map to TopologyLink{NICMac, LeafName="fi-<switchId>", LeafPort="<slot>/<port>",
	//    LeafMgmt=""}. LeafMgmt resolvable from classId="networkElement" if needed.
	panic("not implemented: UCSM XML API enumerate + map -> v1alpha1.DiscoveredInventory")
}

// compile-time check
var _ inventory.Collector = (*Collector)(nil)
