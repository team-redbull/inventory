// Mock OpenManage Enterprise server for collector development and testing.
// Serves the OME REST endpoints the ome collector will call.
// No auth enforced — accepts all requests.
//
// Usage: go run ./hack/mock/ome   (listens on :8081)
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Seed data: two Dell PowerEdge R750 servers with topology via iDRAC Connection View.
var devices = []map[string]any{
	{
		"Id":               1001,
		"DeviceServiceTag": "SRV001",
		"Model":            "PowerEdge R750",
		"DeviceName":       "srv001.dc1.local",
		"Type":             1000, // server
		"Identifier":       "aa:bb:cc:dd:ee:01",
	},
	{
		"Id":               1002,
		"DeviceServiceTag": "SRV002",
		"Model":            "PowerEdge R750",
		"DeviceName":       "srv002.dc1.local",
		"Type":             1000,
		"Identifier":       "aa:bb:cc:dd:ee:02",
	},
}

// inventoryDetails returns the hardware inventory for a given device ID.
var inventoryDetails = map[int]map[string]any{
	1001: buildInventory("SRV001", "aa:bb:cc:dd:ee:01", "leaf-01", "Ethernet1/1", "00:11:22:33:44:01", 2, 28, 512, 2),
	1002: buildInventory("SRV002", "aa:bb:cc:dd:ee:02", "leaf-01", "Ethernet1/2", "00:11:22:33:44:02", 2, 28, 512, 2),
}

func buildInventory(tag, mac, leafName, leafPort, neighbourMac string, sockets, coresPerSocket, ramGiB, diskCount int) map[string]any {
	// Build DIMM list: 16 × 32GiB = 512GiB
	dims := make([]map[string]any, 16)
	for i := range dims {
		dims[i] = map[string]any{"Id": i + 1, "Size": 32768, "Status": "OK"}
	}
	// Build disk list
	disks := make([]map[string]any, diskCount)
	for i := range disks {
		disks[i] = map[string]any{
			"Id":        i + 1,
			"Size":      int64(960197124096),
			"MediaType": "SSD",
			"Model":     "SAMSUNG MZ7LH960HAJR",
		}
	}
	// Network interfaces with Connection View (LLDP neighbour) topology.
	nics := []map[string]any{
		{
			"NicId": "NIC.Integrated.1",
			"Ports": []map[string]any{
				{
					"PortId":                 "NIC.Integrated.1-1",
					"CurrentMacAddress":      mac,
					"LinkStatus":             "Up",
					"partnerMacAddress":      neighbourMac,
					"partnerPortDescription": leafName + ":" + leafPort,
				},
			},
		},
	}

	procs := make([]map[string]any, sockets)
	for i := range procs {
		procs[i] = map[string]any{
			"Id":           i + 1,
			"Family":       "Intel",
			"NumberOfCores": coresPerSocket,
			"ModelName":    "Intel(R) Xeon(R) Gold 6330 CPU @ 2.00GHz",
			"MaxSpeed":     2000,
		}
	}

	return map[string]any{
		"Id":       tag,
		"DeviceId": tag,
		"InventoryInfo": []map[string]any{
			{"InventoryType": "serverProcessors", "InventoryData": procs},
			{"InventoryType": "serverMemoryInfo", "InventoryData": dims},
			{"InventoryType": "serverNetworkInterfaces", "InventoryData": nics},
			{"InventoryType": "serverStorageDiskView", "InventoryData": disks},
		},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	mux := http.NewServeMux()

	// GET /api/DeviceService/Devices — list all servers
	mux.HandleFunc("/api/DeviceService/Devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{
			"@odata.count": len(devices),
			"value":        devices,
		})
	})

	// GET /api/DeviceService/Devices/{id}/InventoryDetails
	mux.HandleFunc("/api/DeviceService/Devices/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/InventoryDetails") {
			http.NotFound(w, r)
			return
		}
		// Extract numeric ID from path: /api/DeviceService/Devices/1001/InventoryDetails
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 6 {
			http.NotFound(w, r)
			return
		}
		idStr := parts[len(parts)-2]
		var id int
		for _, d := range devices {
			if fmt.Sprintf("%d", d["Id"]) == idStr {
				id = d["Id"].(int)
				break
			}
		}
		det, ok := inventoryDetails[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, det)
	})

	addr := ":8081"
	log.Printf("mock OME listening on %s", addr)
	log.Printf("  GET  /api/DeviceService/Devices")
	log.Printf("  GET  /api/DeviceService/Devices/{id}/InventoryDetails")
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
