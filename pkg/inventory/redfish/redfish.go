// Package redfish queries a host's BMC directly over the Redfish REST API to
// discover hardware facts (vendor, model, CPU cores, RAM, storage). It is the
// fallback collector for whitebox / generic servers that are not managed by an
// aggregator (OME, Intersight, UCS Central).
//
// Called from the InventoryRecord reconciler when:
//   - spec.bmc.type is "generic" or unset (not idrac/cimc/ucsm)
//   - status.identity is still nil after BMH enrichment
//
// Credential resolution: reads spec.bmc.credentialsRef Secret (same namespace
// as the InventoryRecord) for "username" and "password" keys.
package redfish

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
)

// Enrich queries the Redfish BMC described by rec.Spec.BMC and returns a
// DiscoveredInventory. Returns nil (non-fatal) when the host is not a
// Redfish target, when credentials are missing, or when the BMC is
// unreachable — the caller should treat nil as "nothing to merge".
func Enrich(ctx context.Context, c client.Client, rec *v1alpha1.InventoryRecord) *v1alpha1.DiscoveredInventory {
	addr := rec.Spec.BMC.Address
	if addr == "" {
		return nil
	}

	baseURL, ok := redfishBaseURL(addr)
	if !ok {
		return nil // IPMI-only or Cisco-proprietary — no Redfish endpoint
	}

	if rec.Spec.BMC.CredentialsRef.Name == "" {
		return nil
	}

	ns := rec.Spec.BMC.CredentialsRef.Namespace
	if ns == "" {
		ns = rec.Namespace
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: rec.Spec.BMC.CredentialsRef.Name, Namespace: ns}, secret); err != nil {
		return nil
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return nil
	}

	rf := &rfClient{
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: strings.HasPrefix(baseURL, "http://")}, // only skip verify for plain http
			},
		},
		base:     baseURL,
		username: username,
		password: password,
	}

	return rf.inventory(ctx, rec.Spec.ServiceTag)
}

// redfishBaseURL converts a Metal3 BMC address to a Redfish http(s) base URL.
// Returns ("", false) for protocols that don't support standard Redfish.
func redfishBaseURL(addr string) (string, bool) {
	switch {
	case strings.HasPrefix(addr, "ipmi://"), strings.HasPrefix(addr, "ipmitool://"):
		return "", false // IPMI-only, no Redfish
	case strings.HasPrefix(addr, "cimc://"):
		return "", false // Cisco IMC proprietary (handled by UCS collectors)
	case strings.HasPrefix(addr, "redfish+http://"):
		return "http://" + strings.TrimPrefix(addr, "redfish+http://"), true
	case strings.HasPrefix(addr, "redfish+https://"):
		return "https://" + strings.TrimPrefix(addr, "redfish+https://"), true
	case strings.HasPrefix(addr, "redfish://"):
		return "https://" + strings.TrimPrefix(addr, "redfish://"), true
	default:
		// idrac-virtualmedia, ilo4, ilo5, irmc, bare IP/host — assume Redfish over HTTPS.
		// Strip any leading scheme; the host is what remains after "://".
		host := addr
		if i := strings.Index(addr, "://"); i >= 0 {
			host = addr[i+3:]
		}
		// Strip any trailing path (e.g. idrac-virtualmedia://host/redfish/v1/Systems/...)
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		return "https://" + host, true
	}
}

// ---- internal HTTP client ---------------------------------------------------

type rfClient struct {
	http     *http.Client
	base     string
	username string
	password string
}

func (rf *rfClient) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rf.base+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(rf.username, rf.password)
	req.Header.Set("Accept", "application/json")

	resp, err := rf.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("redfish %s: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// ---- Redfish JSON shapes (minimal — only fields we consume) ----------------

type collection struct {
	Members []odata `json:"Members"`
}
type odata struct {
	ID string `json:"@odata.id"`
}

type system struct {
	Manufacturer string `json:"Manufacturer"`
	Model        string `json:"Model"`
	SerialNumber string `json:"SerialNumber"`

	// ProcessorSummary.CoreCount is total physical cores across all sockets
	// per DSP0266; some firmware (older iDRAC) reports per-socket instead.
	// Use it as-is; it's always better than socket count.
	ProcessorSummary struct {
		Count     int `json:"Count"`     // sockets
		CoreCount int `json:"CoreCount"` // total physical cores
	} `json:"ProcessorSummary"`

	MemorySummary struct {
		TotalSystemMemoryGiB float64 `json:"TotalSystemMemoryGiB"`
	} `json:"MemorySummary"`

	Storage odata `json:"Storage"`
}

type storageCtrl struct {
	Drives []odata `json:"Drives"`
}

type drive struct {
	CapacityBytes int64 `json:"CapacityBytes"`
}

// ---- inventory extraction --------------------------------------------------

func (rf *rfClient) inventory(ctx context.Context, serviceTag string) *v1alpha1.DiscoveredInventory {
	// 1. Find the first system member.
	var col collection
	if err := rf.get(ctx, "/redfish/v1/Systems", &col); err != nil || len(col.Members) == 0 {
		return nil
	}
	var sys system
	if err := rf.get(ctx, col.Members[0].ID, &sys); err != nil {
		return nil
	}

	// Use the Redfish serial as a cross-check; if it doesn't match spec.serviceTag
	// (operator misconfiguration), still proceed — the caller knows which IR to enrich.
	cores := sys.ProcessorSummary.CoreCount
	if cores == 0 {
		cores = sys.ProcessorSummary.Count // degraded: socket count as floor
	}
	ramGiB := int64(sys.MemorySummary.TotalSystemMemoryGiB)

	// 2. Storage: walk controllers → drives → sum CapacityBytes.
	storageGiB := rf.totalStorageGiB(ctx, sys.Storage.ID)

	inv := &v1alpha1.DiscoveredInventory{
		Identity: &v1alpha1.Identity{
			ServiceTag: sys.SerialNumber,
			Vendor:     sys.Manufacturer,
			Model:      sys.Model,
		},
		Compute: &v1alpha1.Compute{
			CoresTotal: int32(cores),
			RAMGiB:     ramGiB,
		},
	}
	if storageGiB > 0 {
		inv.Storage = &v1alpha1.Storage{TotalGiB: storageGiB}
	}
	return inv
}

func (rf *rfClient) totalStorageGiB(ctx context.Context, storagePath string) int64 {
	if storagePath == "" {
		return 0
	}
	var col collection
	if err := rf.get(ctx, storagePath, &col); err != nil {
		return 0
	}
	var totalBytes int64
	for _, member := range col.Members {
		var ctrl storageCtrl
		if err := rf.get(ctx, member.ID, &ctrl); err != nil {
			continue
		}
		for _, d := range ctrl.Drives {
			var dr drive
			if err := rf.get(ctx, d.ID, &dr); err != nil {
				continue
			}
			totalBytes += dr.CapacityBytes
		}
	}
	return totalBytes / (1024 * 1024 * 1024)
}
