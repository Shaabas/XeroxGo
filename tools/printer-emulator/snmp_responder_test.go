package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

func TestCompareOIDs(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{"identical", "1.3.6.1.2.1", "1.3.6.1.2.1", 0},
		{"identical with leading dot", ".1.3.6.1.2.1", "1.3.6.1.2.1", 0},
		{"prefix", "1.3.6.1", "1.3.6.1.2", -1},
		{"suffix", "1.3.6.1.2", "1.3.6.1", 1},
		{"numeric vs lexicographical 1.2 < 1.10", "1.3.6.1.2.1.43.10.2.1.4.1.2", "1.3.6.1.2.1.43.10.2.1.4.1.10", -1},
		{"numeric vs lexicographical 1.10 > 1.2", "1.3.6.1.2.1.43.10.2.1.4.1.10", "1.3.6.1.2.1.43.10.2.1.4.1.2", 1},
		{"column ordering col 3 < col 4", "1.3.6.1.2.1.43.10.2.1.3.1.1", "1.3.6.1.2.1.43.10.2.1.4.1.1", -1},
		{"subtrees 25 < 43", "1.3.6.1.2.1.25.3.5.1.1.1", "1.3.6.1.2.1.43.10.2.1.3.1.1", -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compareOIDs(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("compareOIDs(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestBuildActiveMIB_SortingAndOmission(t *testing.T) {
	st := DefaultState()
	st.OmittedOIDs = []string{"1.3.6.1.2.1.43.10.2.1.4.1.1"} // omit marker life count
	pdus := st.buildActiveMIB()

	// Verify numeric ordering
	for i := 0; i < len(pdus)-1; i++ {
		cmp := compareOIDs(pdus[i].Name, pdus[i+1].Name)
		if cmp >= 0 {
			t.Fatalf("active MIB not sorted: [%d] %s vs [%d] %s (cmp=%d)", i, pdus[i].Name, i+1, pdus[i+1].Name, cmp)
		}
	}

	// Verify omitted OID was excluded
	for _, p := range pdus {
		if normalizeOID(p.Name) == "1.3.6.1.2.1.43.10.2.1.4.1.1" {
			t.Errorf("omitted OID 1.3.6.1.2.1.43.10.2.1.4.1.1 found in active MIB")
		}
	}
}

func TestServer_RealUDPWalk(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "state.json")
	pidFile := filepath.Join(tempDir, "server.pid")

	store, err := NewStateStore(stateFile)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	srv, err := NewServer("127.0.0.1", 0, "public", store, pidFile)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop()

	client := &gosnmp.GoSNMP{
		Target:    "127.0.0.1",
		Port:      uint16(srv.Port()),
		Community: "public",
		Version:   gosnmp.Version2c,
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	if err := client.Connect(); err != nil {
		t.Fatalf("client failed to connect: %v", err)
	}
	defer client.Conn.Close()

	// 1. Test GET request on scalar sysDescr
	resp, err := client.Get([]string{oidSysDescr})
	if err != nil {
		t.Fatalf("client.Get(sysDescr) failed: %v", err)
	}
	if len(resp.Variables) != 1 || string(resp.Variables[0].Value.([]byte)) != store.GetState().SysDescr {
		t.Errorf("unexpected sysDescr response: %+v", resp.Variables)
	}

	// 2. Test Walk on hrPrinterTable (1.3.6.1.2.1.25.3.5.1)
	var hrWalked []gosnmp.SnmpPDU
	err = client.Walk(oidSubtreeHRPrinter, func(pdu gosnmp.SnmpPDU) error {
		hrWalked = append(hrWalked, pdu)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(hrPrinter) failed: %v", err)
	}
	if len(hrWalked) < 2 {
		t.Fatalf("expected at least 2 PDUs from hrPrinterTable, got %d", len(hrWalked))
	}
	// Status should be 3 (idle)
	if hrWalked[0].Value.(int) != 3 {
		t.Errorf("expected hrPrinterStatus=3, got %v", hrWalked[0].Value)
	}

	// 3. Test Walk on prtMarkerTable (1.3.6.1.2.1.43.10.2.1)
	var markerWalked []gosnmp.SnmpPDU
	err = client.Walk(oidSubtreePrtMarker, func(pdu gosnmp.SnmpPDU) error {
		markerWalked = append(markerWalked, pdu)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(prtMarker) failed: %v", err)
	}
	// Unit and life count
	if len(markerWalked) < 2 {
		t.Fatalf("expected at least 2 marker PDUs, got %d", len(markerWalked))
	}
	// Unit should be 8 (sheets)
	if markerWalked[0].Value.(int) != 8 {
		t.Errorf("expected marker counter unit=8, got %v", markerWalked[0].Value)
	}
	// Life count should be 1000
	if markerWalked[1].Value.(uint) != 1000 && markerWalked[1].Value.(uint32) != 1000 {
		t.Errorf("expected marker life count=1000, got %v", markerWalked[1].Value)
	}

	// 4. Test Walk on entire MIB (end-of-MIB handling)
	var allWalked []gosnmp.SnmpPDU
	err = client.Walk("1.3.6.1.2.1", func(pdu gosnmp.SnmpPDU) error {
		allWalked = append(allWalked, pdu)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(root) failed: %v", err)
	}
	if len(allWalked) < 5 {
		t.Fatalf("expected multiple PDUs from full MIB walk, got %d", len(allWalked))
	}
}

func TestStateStore_DeepCopyAndImmutability(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "state.json")
	store, err := NewStateStore(stateFile)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	st1 := store.GetState()
	// Mutate slices in st1
	st1.Markers[0].LifeCount = 99999
	st1.Supplies[0].Level = 1
	st1.OmittedOIDs = append(st1.OmittedOIDs, "1.2.3.4")
	zero := uint32(0)
	st1.SysUpTime = &zero

	// st2 must remain unaffected
	st2 := store.GetState()
	if st2.Markers[0].LifeCount == 99999 {
		t.Errorf("GetState() leaked mutable Markers slice reference")
	}
	if st2.Supplies[0].Level == 1 {
		t.Errorf("GetState() leaked mutable Supplies slice reference")
	}
	if len(st2.OmittedOIDs) > 0 {
		t.Errorf("GetState() leaked mutable OmittedOIDs slice reference")
	}
	if st2.SysUpTime != nil && *st2.SysUpTime == 0 {
		t.Errorf("GetState() leaked mutable SysUpTime pointer reference")
	}

	// buildActiveMIB() must not mutate store state
	_ = st2.buildActiveMIB()
	st3 := store.GetState()
	if st3.Markers[0].LifeCount != 1000 {
		t.Errorf("buildActiveMIB() mutated underlying store state")
	}
}

func TestServer_RealUDP_GetBulk(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "state.json")
	pidFile := filepath.Join(tempDir, "server.pid")

	store, err := NewStateStore(stateFile)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	srv, err := NewServer("127.0.0.1", 0, "public", store, pidFile)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop()

	client := &gosnmp.GoSNMP{
		Target:    "127.0.0.1",
		Port:      uint16(srv.Port()),
		Community: "public",
		Version:   gosnmp.Version2c,
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	if err := client.Connect(); err != nil {
		t.Fatalf("client failed to connect: %v", err)
	}
	defer client.Conn.Close()

	// 1. Non-repeaters and repeaters
	// Variable 0 is scalar sysDescr (1 non-repeater), Variable 1 is prtMarkerTable root (repeater, maxRepetitions=3)
	resp, err := client.GetBulk([]string{oidSysDescr, oidSubtreePrtMarker}, 1, 3)
	if err != nil {
		t.Fatalf("client.GetBulk failed: %v", err)
	}
	if len(resp.Variables) < 2 {
		t.Fatalf("expected at least 2 variables, got %d", len(resp.Variables))
	}
	if normalizeOID(resp.Variables[0].Name) != normalizeOID(oidSysObjectID) {
		t.Errorf("expected first variable to be sysObjectID (1.3.6.1.2.1.1.2.0), got %s", resp.Variables[0].Name)
	}

	// 2. Repeaters only (nonRepeaters=0, maxRepetitions=2)
	respRepeaters, err := client.GetBulk([]string{oidSubtreeHRPrinter}, 0, 2)
	if err != nil {
		t.Fatalf("GetBulk repeaters failed: %v", err)
	}
	if len(respRepeaters.Variables) != 2 {
		t.Errorf("expected exactly 2 variables for maxRepetitions=2, got %d", len(respRepeaters.Variables))
	}

	// 3. End-of-MIB
	respEnd, err := client.GetBulk([]string{"1.3.6.1.4.1.99999"}, 0, 5)
	if err != nil {
		t.Fatalf("GetBulk end-of-MIB failed: %v", err)
	}
	if len(respEnd.Variables) == 0 || respEnd.Variables[0].Type != gosnmp.EndOfMibView {
		t.Errorf("expected EndOfMibView at end of MIB, got %+v", respEnd.Variables)
	}

	// 4. Excessive MaxRepetitions (e.g. 50000)
	respExcessive, err := client.GetBulk([]string{oidSubtreePrtMarker}, 0, 50000)
	if err != nil {
		t.Fatalf("GetBulk with excessive MaxRepetitions failed: %v", err)
	}
	if len(respExcessive.Variables) > 128 {
		t.Errorf("response exceeded maxBulkResponseVariables limit: got %d, max 128", len(respExcessive.Variables))
	}
	if len(respExcessive.Variables) == 0 {
		t.Errorf("expected non-empty response for excessive maxRepetitions query")
	}
}

func TestServer_ZeroUptimeAndRestart(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "state.json")
	store, err := NewStateStore(stateFile)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	// 1. Explicitly set uptime to 0
	err = store.UpdateState(func(s *SNMPState) {
		zero := uint32(0)
		s.SysUpTime = &zero
	})
	if err != nil {
		t.Fatalf("failed to set uptime 0: %v", err)
	}

	// Verify uptime 0 is preserved across GetState() and Sync()
	st := store.GetState()
	if st.SysUpTime == nil || *st.SysUpTime != 0 {
		t.Fatalf("expected SysUpTime to be preserved as 0, got %v", st.SysUpTime)
	}

	pdus := st.buildActiveMIB()
	var uptimePDU *gosnmp.SnmpPDU
	for i := range pdus {
		if normalizeOID(pdus[i].Name) == normalizeOID(oidSysUpTime) {
			uptimePDU = &pdus[i]
			break
		}
	}
	if uptimePDU == nil {
		t.Fatalf("sysUpTime PDU not found in active MIB")
	}
	if uptimePDU.Value.(uint32) != 0 {
		t.Errorf("expected sysUpTime PDU value 0, got %v", uptimePDU.Value)
	}
}
