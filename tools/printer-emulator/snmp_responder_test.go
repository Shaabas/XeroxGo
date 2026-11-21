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
