package printer

import (
	"context"
	"errors"
	"testing"

	"github.com/gosnmp/gosnmp"
)

// mockDynamicSession implements DynamicSNMPSession for testing.
type mockDynamicSession struct {
	getFunc  func(oids []string) (*gosnmp.SnmpPacket, error)
	walkFunc func(rootOid string, walkFn gosnmp.WalkFunc) error
}

func (m *mockDynamicSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	if m.getFunc != nil {
		return m.getFunc(oids)
	}
	return &gosnmp.SnmpPacket{}, nil
}

func (m *mockDynamicSession) Walk(rootOid string, walkFn gosnmp.WalkFunc) error {
	if m.walkFunc != nil {
		return m.walkFunc(rootOid, walkFn)
	}
	return nil
}

func (m *mockDynamicSession) Close() error {
	return nil
}

func TestDynamicSNMP_TableWalk_CustomIndicesAndUnits(t *testing.T) {
	// Simulate an RFC 3805 / RFC 2790 compliant device with non-standard row indices:
	// hrDeviceIndex: 2
	// markerIndex: 3 (counter unit: 8 = sheets, life count: 4521)
	// supply 1: 2.1 (Toner, unit 19 = percent, level: 75)
	// supply 2: 2.2 (Drum, unit 7 = impressions, max: 20000, level: 18500)
	mock := &mockDynamicSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return &gosnmp.SnmpPacket{
				Variables: []gosnmp.SnmpPDU{
					{Name: "1.3.6.1.2.1.1.1.0", Type: gosnmp.OctetString, Value: []byte("HP LaserJet Tank 1020w (Synthetic Test Fixture - Not Real Hardware Measurement)")},
					{Name: "1.3.6.1.2.1.1.2.0", Type: gosnmp.OctetString, Value: []byte("1.3.6.1.4.1.11.2.3.9.1")},
					{Name: "1.3.6.1.2.1.1.3.0", Type: gosnmp.TimeTicks, Value: uint32(1234567)},
				},
			}, nil
		},
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			normRoot := normalizeOID(rootOid)
			switch normRoot {
			case normalizeOID(oidSubtreeHRPrinter): // 1.3.6.1.2.1.25.3.5.1
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.25.3.5.1.1.2", // hrPrinterStatus for index 2
					Type:  gosnmp.Integer,
					Value: int32(3), // idle
				})
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.25.3.5.1.2.2", // hrPrinterDetectedErrorState for index 2
					Type:  gosnmp.OctetString,
					Value: []byte{0x00, 0x00},
				})

			case normalizeOID(oidSubtreePrtGeneral): // 1.3.6.1.2.1.43.5.1
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.43.5.1.1.16.1", // prtGeneralPrinterName
					Type:  gosnmp.OctetString,
					Value: []byte("HP LaserJet Tank 1020w (Synthetic Test Fixture - Not Real Hardware Measurement)"),
				})

			case normalizeOID(oidSubtreePrtCover): // 1.3.6.1.2.1.43.6.1
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.43.6.1.1.2.2.1", // prtCoverDescription
					Type:  gosnmp.OctetString,
					Value: []byte("Top Access Door"),
				})
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.43.6.1.1.3.2.1", // prtCoverStatus (4 = coverClosed)
					Type:  gosnmp.Integer,
					Value: int32(4),
				})

			case normalizeOID(oidSubtreePrtMarker): // 1.3.6.1.2.1.43.10.2.1
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.43.10.2.1.3.2.3", // prtMarkerCounterUnit for index 2.3
					Type:  gosnmp.Integer,
					Value: int32(8), // sheets
				})
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.43.10.2.1.4.2.3", // prtMarkerLifeCount for index 2.3
					Type:  gosnmp.Counter32,
					Value: uint32(4521),
				})

			case normalizeOID(oidSubtreePrtSupplies): // 1.3.6.1.2.1.43.11.1.1
				// Supply 1 (index 2.1): Black Toner (unit 19 = percent, level 75)
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.4.2.1", Type: gosnmp.Integer, Value: int32(3)}) // consumed
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.5.2.1", Type: gosnmp.Integer, Value: int32(3)}) // toner
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.6.2.1", Type: gosnmp.OctetString, Value: []byte("HP Black Toner Tank")})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.7.2.1", Type: gosnmp.Integer, Value: int32(19)}) // percent
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.8.2.1", Type: gosnmp.Integer, Value: int32(100)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.9.2.1", Type: gosnmp.Integer, Value: int32(75)})

				// Supply 2 (index 2.2): Imaging Drum (unit 7 = impressions, max 20000, level 18500)
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.4.2.2", Type: gosnmp.Integer, Value: int32(3)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.5.2.2", Type: gosnmp.Integer, Value: int32(9)}) // opc / drum
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.6.2.2", Type: gosnmp.OctetString, Value: []byte("Imaging Drum")})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.7.2.2", Type: gosnmp.Integer, Value: int32(7)}) // impressions
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.8.2.2", Type: gosnmp.Integer, Value: int32(20000)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.9.2.2", Type: gosnmp.Integer, Value: int32(18500)})

			case normalizeOID(oidSubtreePrtAlert): // 1.3.6.1.2.1.43.18.1.1
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.18.1.1.2.2.1", Type: gosnmp.Integer, Value: int32(4)}) // warning
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.18.1.1.3.2.1", Type: gosnmp.Integer, Value: int32(3)}) // untrained
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.18.1.1.4.2.1", Type: gosnmp.Integer, Value: int32(9)}) // markerSupplies
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.18.1.1.7.2.1", Type: gosnmp.Integer, Value: int32(26)}) // markerTonerAlmostEmpty
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.18.1.1.8.2.1", Type: gosnmp.OctetString, Value: []byte("Toner Low Warning")})
			}
			return nil
		},
	}

	ctx := context.Background()
	res, err := DiscoverSNMPDynamic(ctx, mock, "192.168.1.50", 161)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic unexpected error: %v", err)
	}

	if res.DiscoveryMode != "SNMP_TABLE_WALK" {
		t.Errorf("expected discovery mode SNMP_TABLE_WALK, got %q", res.DiscoveryMode)
	}
	if res.SysDescr != "HP LaserJet Tank 1020w (Synthetic Test Fixture - Not Real Hardware Measurement)" {
		t.Errorf("expected sysDescr, got %q", res.SysDescr)
	}
	if res.PrinterName != "HP LaserJet Tank 1020w (Synthetic Test Fixture - Not Real Hardware Measurement)" {
		t.Errorf("expected printer name, got %q", res.PrinterName)
	}

	// Verify hrPrinter discovered at index "2"
	if len(res.Printers) != 1 {
		t.Fatalf("expected 1 printer device, got %d", len(res.Printers))
	}
	if res.Printers[0].Index != "2" || res.Printers[0].StatusDesc != "idle" {
		t.Errorf("expected printer index 2 idle, got index %s status %s", res.Printers[0].Index, res.Printers[0].StatusDesc)
	}

	// Verify Marker discovered at non-standard index "2.3" with valid page unit
	if len(res.Markers) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(res.Markers))
	}
	m := res.Markers[0]
	if m.Index != "2.3" {
		t.Errorf("expected marker index 2.3, got %s", m.Index)
	}
	if m.CounterUnit != 8 || !m.IsPageCounter || m.CounterUnitName != "sheets" {
		t.Errorf("expected marker counter unit sheets (8), got %d (%s, isPage=%v)", m.CounterUnit, m.CounterUnitName, m.IsPageCounter)
	}
	if m.LifeCount != 4521 {
		t.Errorf("expected marker life count 4521, got %d", m.LifeCount)
	}

	// Verify Supplies discovered
	if len(res.Supplies) != 2 {
		t.Fatalf("expected 2 supplies, got %d", len(res.Supplies))
	}
	SortSuppliesByIndex(res.Supplies)

	s1 := res.Supplies[0]
	if s1.Index != "2.1" || s1.TypeName != "toner" || s1.Level != 75 || s1.UnitName != "percent" {
		t.Errorf("unexpected supply 1: %+v", s1)
	}
	if s1.Percent == nil || *s1.Percent != 75.0 {
		t.Errorf("expected supply 1 percent 75, got %v", s1.Percent)
	}

	s2 := res.Supplies[1]
	if s2.Index != "2.2" || s2.TypeName != "opc" || s2.Level != 18500 {
		t.Errorf("unexpected supply 2: %+v", s2)
	}
	if s2.Percent == nil || *s2.Percent < 92.0 || *s2.Percent > 93.0 {
		t.Errorf("expected supply 2 percent ~92.5%%, got %v", s2.Percent)
	}

	// Verify Alerts discovered
	if len(res.Alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(res.Alerts))
	}
	a := res.Alerts[0]
	if a.SeverityName != "warning" || a.CodeName != "markerTonerAlmostEmpty" || a.Description != "Toner Low Warning" {
		t.Errorf("unexpected alert: %+v", a)
	}

	// Verify Cover discovered
	if len(res.Covers) != 1 {
		t.Fatalf("expected 1 cover, got %d", len(res.Covers))
	}
	if res.Covers[0].Description != "Top Access Door" || res.Covers[0].StatusDesc != "coverClosed" {
		t.Errorf("unexpected cover: %+v", res.Covers[0])
	}
}

func TestDynamicSNMP_InvalidCounterUnit(t *testing.T) {
	// Device reporting counter unit 4 (micrometers) instead of sheets/impressions
	mock := &mockDynamicSession{
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			normRoot := normalizeOID(rootOid)
			if normRoot == normalizeOID(oidSubtreePrtMarker) {
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.43.10.2.1.3.1.1",
					Type:  gosnmp.Integer,
					Value: int32(4), // micrometers (NOT page counter)
				})
				_ = walkFn(gosnmp.SnmpPDU{
					Name:  "1.3.6.1.2.1.43.10.2.1.4.1.1",
					Type:  gosnmp.Counter32,
					Value: uint32(987654),
				})
			}
			return nil
		},
	}

	ctx := context.Background()
	res, err := DiscoverSNMPDynamic(ctx, mock, "127.0.0.1", 161)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(res.Markers) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(res.Markers))
	}
	m := res.Markers[0]
	if m.IsPageCounter {
		t.Errorf("expected IsPageCounter to be false for micrometers (unit 4)")
	}
	if m.CounterUnitName != "micrometers" {
		t.Errorf("expected CounterUnitName 'micrometers', got %q", m.CounterUnitName)
	}
}

func TestDynamicSNMP_WalkFails_FallsBackToGet(t *testing.T) {
	// Simulates an agent that rejects Walk (GetNext) but answers Get on standard OIDs
	mock := &mockDynamicSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return &gosnmp.SnmpPacket{
				Variables: []gosnmp.SnmpPDU{
					{Name: oidSysDescr, Type: gosnmp.OctetString, Value: []byte("Fallback Printer Agent")},
					{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: int32(3)}, // idle
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00, 0x00}},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.Counter32, Value: uint32(300)},
					{Name: oidPrtMarkerSuppliesLevel1, Type: gosnmp.Integer, Value: int32(80)},
					{Name: oidPrtMarkerSuppliesMax1, Type: gosnmp.Integer, Value: int32(100)},
				},
			}, nil
		},
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			return errors.New("SNMP agent does not support GetNext / Walk")
		},
	}

	ctx := context.Background()
	res, err := DiscoverSNMPDynamic(ctx, mock, "127.0.0.1", 161)
	if err != nil {
		t.Fatalf("unexpected error on fallback: %v", err)
	}

	if res.DiscoveryMode != "SNMP_GET_FALLBACK" {
		t.Errorf("expected discovery mode SNMP_GET_FALLBACK, got %q", res.DiscoveryMode)
	}
	if len(res.Printers) != 1 || res.Printers[0].StatusDesc != "idle" {
		t.Errorf("expected fallback printer idle, got %+v", res.Printers)
	}
	if len(res.Markers) != 1 || res.Markers[0].LifeCount != 300 {
		t.Errorf("expected fallback marker 300, got %+v", res.Markers)
	}
	if len(res.Supplies) < 1 || res.Supplies[0].Level != 80 {
		t.Errorf("expected fallback toner 80, got %+v", res.Supplies)
	}
}

func TestDynamicSNMP_CompleteFailure(t *testing.T) {
	mock := &mockDynamicSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return nil, errors.New("network unreachable")
		},
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			return errors.New("network unreachable")
		},
	}

	ctx := context.Background()
	res, err := DiscoverSNMPDynamic(ctx, mock, "10.255.255.1", 161)
	if err == nil {
		t.Fatalf("expected error on complete failure, got nil")
	}
	if res.DiscoveryMode != "SNMP_FAILED" {
		t.Errorf("expected SNMP_FAILED, got %q", res.DiscoveryMode)
	}
}
