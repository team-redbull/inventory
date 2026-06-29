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
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "example.io/inventory/api/v1alpha1"
)

// privateRanges are the IPv4/IPv6 management-network ranges a BMC address must
// fall within. Public IPs are rejected to prevent SSRF credential exfiltration.
var privateRanges = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16", // link-local (some BMCs)
		"fc00::/7",       // IPv6 ULA
		"fe80::/10",      // IPv6 link-local
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func isPrivateIP(ip net.IP) bool {
	for _, n := range privateRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validateBMCHost rejects any host that resolves to a public IP.
// Bare IPs are checked directly; hostnames are resolved and every returned
// address must be private. Returns a non-nil error when the check fails.
func validateBMCHost(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if !isPrivateIP(ip) {
			return fmt.Errorf("BMC host %q is not a private/management address", host)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("cannot resolve BMC host %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !isPrivateIP(ip) {
			return fmt.Errorf("BMC host %q resolves to non-private address %s", host, a)
		}
	}
	return nil
}

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

	// Validate the resolved host is within a private/management range before
	// any authenticated request (SSRF guard).
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	if err := validateBMCHost(u.Hostname()); err != nil {
		return nil
	}

	if rec.Spec.BMC.CredentialsRef.Name == "" {
		return nil
	}

	// Always resolve the Secret in the InventoryRecord's own namespace.
	// Never honor credentialsRef.Namespace from the spec — doing so would let
	// an operator-controlled InventoryRecord exfiltrate Secrets from other
	// namespaces (confused-deputy / cross-namespace disclosure).
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      rec.Spec.BMC.CredentialsRef.Name,
		Namespace: rec.Namespace,
	}, secret); err != nil {
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
				TLSClientConfig: &tls.Config{InsecureSkipVerify: strings.HasPrefix(baseURL, "http://")},
			},
			// Never follow redirects: a redirect to a public host would bypass
			// the validateBMCHost check and send credentials off-network.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
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

type ethernetInterface struct {
	Name                string `json:"Name"`
	MACAddress          string `json:"MACAddress"`
	PermanentMACAddress string `json:"PermanentMACAddress"`
	SpeedMbps           int64  `json:"SpeedMbps"`
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
		Network: rf.ethernetInterfaces(ctx, col.Members[0].ID),
	}
	if storageGiB > 0 {
		inv.Storage = &v1alpha1.Storage{TotalGiB: storageGiB}
	}
	return inv
}

func (rf *rfClient) ethernetInterfaces(ctx context.Context, systemPath string) []v1alpha1.NIC {
	var col collection
	if err := rf.get(ctx, systemPath+"/EthernetInterfaces", &col); err != nil {
		return nil
	}
	var nics []v1alpha1.NIC
	for _, m := range col.Members {
		var ei ethernetInterface
		if err := rf.get(ctx, m.ID, &ei); err != nil {
			continue
		}
		mac := ei.MACAddress
		if mac == "" {
			mac = ei.PermanentMACAddress
		}
		if mac == "" {
			continue
		}
		nics = append(nics, v1alpha1.NIC{
			Name:     ei.Name,
			MAC:      mac,
			SpeedMbs: ei.SpeedMbps,
		})
	}
	return nics
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
