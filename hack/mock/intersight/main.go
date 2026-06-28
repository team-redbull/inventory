// Mock Intersight Private Virtual Appliance for collector development and testing.
// Serves the Intersight REST endpoints the intersight collector will call.
// No HMAC auth enforced — accepts all requests.
//
// Usage: go run ./hack/mock/intersight   (listens on :8082)
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// Seed: one Cisco UCS rack server managed by Intersight PVA.
// Topology via PeerInterfaceDn (fabric interconnect port DN).
var rackUnits = []map[string]any{
	{
		"Moid":        "moid-srv003",
		"Serial":      "SRV003",
		"Model":       "UCSC-C240-M6SN",
		"Vendor":      "Cisco",
		"NumCpus":     2,
		"TotalMemory": 524288, // MiB → 512 GiB
	},
}

var blades []map[string]any // no blades in this test environment

// NIC with PeerInterfaceDn pointing to fabric interconnect port.
var hostEthInterfaces = []map[string]any{
	{
		"Moid":            "moid-nic-srv003-1",
		"MacAddress":      "aa:bb:cc:dd:ee:03",
		"Name":            "eth0",
		"PeerInterfaceDn": "sys/fex-1/phys/slot-1/port-1",
	},
	{
		"Moid":            "moid-nic-srv003-2",
		"MacAddress":      "aa:bb:cc:dd:ef:03",
		"Name":            "eth1",
		"PeerInterfaceDn": "sys/fex-2/phys/slot-1/port-1",
	},
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
		// Strip query string for routing
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}

		switch path {
		case "compute/RackUnits":
			writeJSON(w, map[string]any{"Count": len(rackUnits), "Results": rackUnits})
		case "compute/Blades":
			writeJSON(w, map[string]any{"Count": 0, "Results": blades})
		case "adapter/HostEthInterfaces":
			writeJSON(w, map[string]any{"Count": len(hostEthInterfaces), "Results": hostEthInterfaces})
		default:
			http.NotFound(w, r)
		}
	})

	addr := ":8082"
	log.Printf("mock Intersight PVA listening on %s", addr)
	log.Printf("  GET  /api/v1/compute/RackUnits")
	log.Printf("  GET  /api/v1/compute/Blades")
	log.Printf("  GET  /api/v1/adapter/HostEthInterfaces")
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
