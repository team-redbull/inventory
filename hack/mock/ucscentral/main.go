// Mock UCS Central XML API for collector development and testing.
// Handles the /centralApi/ endpoint used by the ucscentral collector.
// No real cookie validation — any cookie is accepted after login.
//
// DNs use the UCS Central domain-scoped format:
//   domainGroup-root/domain-<name>/sys/rack-unit-1/...
// The collector must strip this prefix when parsing topology.
//
// Usage: go run ./hack/mock/ucscentral   (listens on :8083)
package main

import (
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"strings"
)

type xmlRequest struct {
	XMLName xml.Name
	Cookie  string `xml:"cookie,attr"`
	ClassID string `xml:"classId,attr"`
	InName  string `xml:"inName,attr"`
	InPass  string `xml:"inPassword,attr"`
}

// UCS Central login: outDomains lists managed UCSM domains.
const loginResp = `<aaaLoginResponse dn="domainGroup-root/user-ext/remote-user-admin-Cisco" outCookie="MOCK-COOKIE-123" outRefreshPeriod="600" outPriv="admin" outDomains="dc1-fi" outChannel="noencssl" outEvtChannel="noencssl" response="yes"/>`

// All DNs are prefixed with domainGroup-root/domain-<name>/
// The collector strips this prefix when extracting per-domain info.
const rackUnitResp = `<configResolveClassResponse classId="computeRackUnit" response="yes">
  <outConfigs>
    <computeRackUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1" id="1"
      model="UCSC-C240-M6SN" numOfCpus="2" totalMemory="524288"
      serverId="1" serial="SRV004" vendor="Cisco Systems Inc"
      adminPower="policy" operPower="on"/>
  </outConfigs>
</configResolveClassResponse>`

const bladeResp = `<configResolveClassResponse classId="computeBlade" response="yes">
  <outConfigs/>
</configResolveClassResponse>`

const processorResp = `<configResolveClassResponse classId="processorUnit" response="yes">
  <outConfigs>
    <processorUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/proc-1" id="1"
      model="Intel(R) Xeon(R) Gold 6330 CPU @ 2.00GHz"
      vendor="Intel(R) Corporation" cores="28" speed="2000" socketDesignation="CPU1"/>
    <processorUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/proc-2" id="2"
      model="Intel(R) Xeon(R) Gold 6330 CPU @ 2.00GHz"
      vendor="Intel(R) Corporation" cores="28" speed="2000" socketDesignation="CPU2"/>
  </outConfigs>
</configResolveClassResponse>`

const memoryResp = `<configResolveClassResponse classId="memoryUnit" response="yes">
  <outConfigs>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-1"  id="1"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-2"  id="2"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-3"  id="3"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-4"  id="4"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-5"  id="5"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-6"  id="6"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-7"  id="7"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-8"  id="8"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-9"  id="9"  capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-10" id="10" capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-11" id="11" capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-12" id="12" capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-13" id="13" capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-14" id="14" capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-15" id="15" capacity="32768" presence="equipped" operability="operable"/>
    <memoryUnit dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/memarray-1/mem-16" id="16" capacity="32768" presence="equipped" operability="operable"/>
  </outConfigs>
</configResolveClassResponse>`

const adaptorResp = `<configResolveClassResponse classId="adaptorEthInterface" response="yes">
  <outConfigs>
    <adaptorEthInterface dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/adaptor-1/host-eth-1"
      id="1" mac="aa:bb:cc:dd:ee:03" mtu="9000" ifType="virtual"/>
    <adaptorEthInterface dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/adaptor-1/host-eth-2"
      id="2" mac="aa:bb:cc:dd:ef:03" mtu="9000" ifType="virtual"/>
  </outConfigs>
</configResolveClassResponse>`

// fabricPathEp: switchId=A|B, slotId, portId identify the FI port.
// Collector: TopologyLink{LeafName="fi-<switchId>", LeafPort="<slotId>/<portId>"}.
const fabricPathResp = `<configResolveClassResponse classId="fabricPathEp" response="yes">
  <outConfigs>
    <fabricPathEp dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/adaptor-1/host-eth-1/path-ep-0"
      name="eth0" switchId="A" slotId="1" portId="1" mac="aa:bb:cc:dd:ee:03"/>
    <fabricPathEp dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/adaptor-1/host-eth-2/path-ep-0"
      name="eth1" switchId="B" slotId="1" portId="1" mac="aa:bb:cc:dd:ef:03"/>
  </outConfigs>
</configResolveClassResponse>`

const diskResp = `<configResolveClassResponse classId="storageLocalDisk" response="yes">
  <outConfigs>
    <storageLocalDisk dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/storage-SAS-SLOT-HBA/pd-1"
      id="1" size="960197124096" diskState="Good" pdType="SSD" model="SAMSUNG MZ7LH960"/>
    <storageLocalDisk dn="domainGroup-root/domain-dc1-fi/sys/rack-unit-1/board/storage-SAS-SLOT-HBA/pd-2"
      id="2" size="960197124096" diskState="Good" pdType="SSD" model="SAMSUNG MZ7LH960"/>
  </outConfigs>
</configResolveClassResponse>`

const errResp = `<error cookie="" response="yes" errorCode="552" invocationResult="unidentified-fail" errorDescr="unknown class"/>`

func main() {
	http.HandleFunc("/centralApi/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)

		var req xmlRequest
		_ = xml.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "text/xml")

		action := req.XMLName.Local
		class := req.ClassID

		log.Printf("UCS Central request: action=%s classId=%s", action, class)

		switch {
		case action == "aaaLogin":
			_, _ = w.Write([]byte(loginResp))
		case action == "configResolveClass":
			switch strings.ToLower(class) {
			case "computerackunit":
				_, _ = w.Write([]byte(rackUnitResp))
			case "computeblade":
				_, _ = w.Write([]byte(bladeResp))
			case "processorunit":
				_, _ = w.Write([]byte(processorResp))
			case "memoryunit":
				_, _ = w.Write([]byte(memoryResp))
			case "adaptorethinterface":
				_, _ = w.Write([]byte(adaptorResp))
			case "fabricpathep":
				_, _ = w.Write([]byte(fabricPathResp))
			case "storagelocaldisk":
				_, _ = w.Write([]byte(diskResp))
			default:
				_, _ = w.Write([]byte(errResp))
			}
		default:
			_, _ = w.Write([]byte(errResp))
		}
	})

	addr := ":8083"
	log.Printf("mock UCS Central listening on %s (domain: dc1-fi)", addr)
	log.Printf("  POST /centralApi/  (aaaLogin, configResolveClass)")
	log.Printf("  DNs prefixed with: domainGroup-root/domain-dc1-fi/")
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
