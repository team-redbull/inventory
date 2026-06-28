// Package ome implements the inventory.Collector for Dell OpenManage Enterprise.
//
// One integration covers the entire PowerEdge/iDRAC fleet OME manages: you read
// OME's already-normalized inventory, you do NOT walk per-iDRAC Redfish trees.
package ome

import (
	"context"
	"fmt"
	"net/http"

	v1alpha1 "example.io/inventory/api/v1alpha1"
	"example.io/inventory/pkg/inventory"
)

// Collector talks to one OME appliance.
type Collector struct {
	baseURL string // https://ome.airgap.local
	creds   inventory.Credentials
	http    *http.Client
	token   string // X-Auth-Token from the session, refreshed on 401
}

func New(baseURL string, creds inventory.Credentials, c *http.Client) *Collector {
	if c == nil {
		c = http.DefaultClient // TODO: inject a client with the internal CA bundle
	}
	return &Collector{baseURL: baseURL, creds: creds, http: c}
}

func (c *Collector) Source() v1alpha1.CollectorSource { return v1alpha1.SourceOME }

// List enumerates servers and their inventory from OME.
func (c *Collector) List(ctx context.Context) ([]inventory.Observation, error) {
	if err := c.authenticate(ctx); err != nil {
		return nil, fmt.Errorf("ome auth: %w", err)
	}

	// 1) Enumerate devices, server type only.
	//    GET /api/DeviceService/Devices?$filter=Type eq 1000
	//    Page via @odata.nextLink (OME uses $skip/$top; nextLink is returned).
	devices, err := c.listServerDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("ome list devices: %w", err)
	}

	out := make([]inventory.Observation, 0, len(devices))
	for _, d := range devices {
		// 2) Pull the inventory sub-resources for this device id. Either:
		//    GET /api/DeviceService/Devices(<id>)/InventoryDetails
		//    or per-type:
		//      .../InventoryDetails('serverProcessors')
		//      .../InventoryDetails('serverMemoryDevices')
		//      .../InventoryDetails('serverArrayDisks')
		//      .../InventoryDetails('serverNetworkInterfaces')   // <- NIC MACs (topology join key)
		//      .../InventoryDetails('deviceManagement')          // <- iDRAC addr + firmware
		inv, err := c.fetchInventory(ctx, d.ID)
		if err != nil {
			// TODO: collect per-host errors instead of failing the whole fleet list.
			return nil, fmt.Errorf("ome inventory device=%d: %w", d.ID, err)
		}
		out = append(out, inventory.Observation{Key: d.ServiceTag, Inventory: inv})
	}
	return out, nil
}

// authenticate performs OME session login and stores X-Auth-Token.
// POST /api/SessionService/Sessions  {"UserName","Password","SessionType":"API"}
// -> 201, token in the "X-Auth-Token" response header (send it on every call).
func (c *Collector) authenticate(ctx context.Context) error {
	panic("not implemented: POST /api/SessionService/Sessions, capture X-Auth-Token")
}

type omeDevice struct {
	ID         int64
	ServiceTag string
}

func (c *Collector) listServerDevices(ctx context.Context) ([]omeDevice, error) {
	panic("not implemented: GET /api/DeviceService/Devices, follow @odata.nextLink")
}

// fetchInventory maps OME inventory payloads onto the canonical model.
func (c *Collector) fetchInventory(ctx context.Context, deviceID int64) (*v1alpha1.DiscoveredInventory, error) {
	// MAPPING NOTES (fill these in):
	//   identity:  DeviceServiceTag, Model, "Dell", UUID from device summary
	//   bmc:       deviceManagement[].NetworkAddress + .ManagementProfile firmware
	//   compute:   sum serverProcessors -> sockets/cores, NumberOfCpus, BrandName;
	//              serverMemoryDevices -> sum Size (bytes) -> RAMGiB
	//   storage:   serverArrayDisks -> per-disk Size/MediaType/Model/Wwn; sum TotalGiB
	//   network:   serverNetworkInterfaces -> ports -> PermanentMACAddress, LinkSpeed
	panic("not implemented: map OME InventoryDetails -> v1alpha1.DiscoveredInventory")
}

// compile-time check
var _ inventory.Collector = (*Collector)(nil)
