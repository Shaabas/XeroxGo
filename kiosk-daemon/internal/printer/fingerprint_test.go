package printer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

func TestRunFingerprint_EndToEndWithDiscrepancyAndRedaction(t *testing.T) {
	cupsRawOutput := `"/usr/share/cups/ipptool/get-printer-attributes.test":
    status-code = successful-ok
    printer-name (nameWithoutLanguage) = HPTank
    printer-make-and-model (textWithoutLanguage) = HP LaserJet Tank 1020w
    printer-state (enum) = idle
    printer-is-accepting-jobs (boolean) = true
    document-format-supported (1setOf mimeMediaType) = application/pdf,application/postscript
    media-supported (1setOf keyword) = iso_a4_210x297mm,na_letter_8.5x11in
    sides-supported (1setOf keyword) = one-sided,two-sided-long-edge
    color-supported (boolean) = false
    print-color-mode-supported (1setOf keyword) = monochrome
`

	remoteIPPRawOutput := `"/usr/share/cups/ipptool/get-printer-attributes.test":
    status-code = successful-ok
    printer-name (nameWithoutLanguage) = HP LaserJet Tank 1020w (Serial: VNC3K89123)
    printer-make-and-model (textWithoutLanguage) = HP LaserJet Tank 1020w
    printer-info (textWithoutLanguage) = Hardware MAC 00:1A:2B:3C:4D:5E on 192.168.1.50
    printer-state (enum) = idle
    printer-is-accepting-jobs (boolean) = true
    printer-state-reasons (1setOf keyword) = none
    document-format-supported (1setOf mimeMediaType) = image/pwg-raster,image/urf,application/PCLm
    media-supported (1setOf keyword) = iso_a4_210x297mm
    sides-supported (1setOf keyword) = one-sided
    color-supported (boolean) = false
    print-color-mode-supported (1setOf keyword) = monochrome
    job-creation-attributes-supported (1setOf keyword) = job-impressions-completed
    marker-names (1setOf nameWithoutLanguage) = Black Toner
    marker-types (1setOf keyword) = toner
    marker-colors (1setOf nameWithoutLanguage) = #000000
    marker-levels (1setOf integer) = 88
`

	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmdLine := name + " " + strings.Join(args, " ")
		if name == "lpstat" {
			if len(args) >= 2 && args[0] == "-v" {
				return []byte("device for HPTank: dnssd://HP%20LaserJet%20Tank%201020w._ipp._tcp.local/?uuid=381v6a\n"), nil
			}
			if len(args) >= 2 && args[0] == "-p" {
				return []byte("printer HPTank is idle. enabled since Sat Nov 21 00:00:00 2026\n"), nil
			}
		}
		if name == "ippfind" {
			return []byte("ipp://192.168.1.50:631/ipp/print\n"), nil
		}
		if name == "ipptool" {
			if strings.Contains(cmdLine, "localhost") {
				return []byte(cupsRawOutput), nil
			}
			return []byte(remoteIPPRawOutput), nil
		}
		return nil, errors.New("unknown command")
	}

	mockSNMPSessionFactory := func(cfg SNMPConfig) (DynamicSNMPSession, error) {
		return &mockDynamicSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return &gosnmp.SnmpPacket{
					Variables: []gosnmp.SnmpPDU{
						{Name: "1.3.6.1.2.1.1.1.0", Type: gosnmp.OctetString, Value: []byte("HP LaserJet Tank 1020w SNMP Agent SN:VNC3K89123")},
					},
				}, nil
			},
			walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
				normRoot := normalizeOID(rootOid)
				switch normRoot {
				case normalizeOID(oidSubtreeHRPrinter):
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.25.3.5.1.1.1", Type: gosnmp.Integer, Value: int32(3)}) // idle
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.25.3.5.1.2.1", Type: gosnmp.OctetString, Value: []byte{0x00, 0x00}})
				case normalizeOID(oidSubtreePrtMarker):
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.3.1.1", Type: gosnmp.Integer, Value: int32(8)}) // sheets
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.4.1.1", Type: gosnmp.Counter32, Value: uint32(1042)})
				case normalizeOID(oidSubtreePrtSupplies):
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.4.1.1", Type: gosnmp.Integer, Value: int32(3)})
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.5.1.1", Type: gosnmp.Integer, Value: int32(3)}) // toner
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.6.1.1", Type: gosnmp.OctetString, Value: []byte("Black Toner Reload Tank")})
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.7.1.1", Type: gosnmp.Integer, Value: int32(19)}) // percent
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.8.1.1", Type: gosnmp.Integer, Value: int32(100)})
					_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.11.1.1.9.1.1", Type: gosnmp.Integer, Value: int32(88)})
				}
				return nil
			},
		}, nil
	}

	cfg := FingerprintConfig{
		QueueName:     "HPTank",
		SNMPCommunity: "secret-community-99",
		Redact:        true,
	}

	ctx := context.Background()
	rep, err := RunFingerprint(ctx, mockRunner, mockSNMPSessionFactory, cfg)
	if err != nil {
		t.Fatalf("RunFingerprint error: %v", err)
	}

	// 1. Verify Target resolution
	if rep.Targets.QueueName != "HPTank" {
		t.Errorf("expected queue HPTank, got %q", rep.Targets.QueueName)
	}
	if !rep.Targets.DNSSDResolved {
		t.Errorf("expected DNSSDResolved = true")
	}

	// 2. Verify Duplex Discrepancy Detection
	var duplexComp *FeatureComparison
	for i := range rep.Comparisons {
		if strings.Contains(rep.Comparisons[i].Feature, "Duplex (2-sided Long Edge)") {
			duplexComp = &rep.Comparisons[i]
			break
		}
	}
	if duplexComp == nil {
		t.Fatalf("expected Duplex comparison entry")
	}
	if !duplexComp.Discrepancy {
		t.Errorf("expected duplex discrepancy between CUPS (SUPPORTED) and remote IPP (UNSUPPORTED)")
	}
	if duplexComp.CUPSQueue != CapabilitySupported || duplexComp.RemoteIPP != CapabilityUnsupported {
		t.Errorf("expected CUPS SUPPORTED and Remote UNSUPPORTED, got CUPS=%s, Remote=%s", duplexComp.CUPSQueue, duplexComp.RemoteIPP)
	}

	// 3. Verify Telemetry Summary
	if rep.Telemetry.Rating != TelemetryRatingVerifiedOdometer {
		t.Errorf("expected rating VERIFIED_ODOMETER_ONLY, got %s", rep.Telemetry.Rating)
	}
	if !rep.Telemetry.SafeForPhysicalCompletion {
		t.Errorf("expected SafeForPhysicalCompletion = true due to verified SNMP sheet counter")
	}
	if rep.Telemetry.SNMPMarkerLifeCount != 1042 || rep.Telemetry.SNMPMarkerCounterUnit != "sheets" {
		t.Errorf("unexpected marker counter: %d %s", rep.Telemetry.SNMPMarkerLifeCount, rep.Telemetry.SNMPMarkerCounterUnit)
	}

	// 4. Verify Redaction
	if rep.Targets.SNMPCommunity != "[REDACTED_COMMUNITY]" {
		t.Errorf("expected community redacted, got %q", rep.Targets.SNMPCommunity)
	}
	if strings.Contains(rep.Targets.RemoteIPPURI, "192.168.1.50") {
		t.Errorf("expected IP redacted from RemoteIPPURI, got %q", rep.Targets.RemoteIPPURI)
	}
	if strings.Contains(rep.RemoteIPP.PrinterName, "VNC3K89123") {
		t.Errorf("expected serial redacted from PrinterName, got %q", rep.RemoteIPP.PrinterName)
	}
	if strings.Contains(rep.RemoteIPP.PrinterInfo, "00:1A:2B:3C:4D:5E") {
		t.Errorf("expected MAC redacted from PrinterInfo, got %q", rep.RemoteIPP.PrinterInfo)
	}
	if strings.Contains(rep.SNMP.SysDescr, "VNC3K89123") {
		t.Errorf("expected serial redacted from SysDescr, got %q", rep.SNMP.SysDescr)
	}

	// 5. Test JSON Formatting
	jsonBytes, err := FormatJSON(rep)
	if err != nil {
		t.Fatalf("FormatJSON error: %v", err)
	}
	var unmarshaled map[string]any
	if err := json.Unmarshal(jsonBytes, &unmarshaled); err != nil {
		t.Fatalf("failed to unmarshal formatted JSON: %v", err)
	}

	// 6. Test Human-Readable Formatting
	humanOutput := FormatHumanReadable(rep)
	if !strings.Contains(humanOutput, "XeroxGo — Printer Capability Fingerprint Report") {
		t.Errorf("human output missing title")
	}
	if !strings.Contains(humanOutput, "CAPABILITY CONSENSUS & DISCREPANCIES") {
		t.Errorf("human output missing consensus section")
	}
	if !strings.Contains(humanOutput, "VERIFIED_ODOMETER_ONLY") {
		t.Errorf("human output missing telemetry rating")
	}
	if !strings.Contains(humanOutput, "SUPPLIES (DYNAMIC SNMP DISCOVERY)") {
		t.Errorf("human output missing supplies section")
	}
}

func TestRunFingerprint_MixedSupport_SNMPFails(t *testing.T) {
	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			return []byte("device for Queue1: ipp://10.0.0.99/ipp/print\n"), nil
		}
		if name == "ipptool" {
			return []byte(`status-code = successful-ok
printer-name = "Queue1"
document-format-supported = "application/pdf"
media-supported = "iso_a4_210x297mm"
`), nil
		}
		return nil, errors.New("command failed")
	}

	// SNMP completely fails (e.g. printer has SNMP disabled)
	failingSNMPFactory := func(cfg SNMPConfig) (DynamicSNMPSession, error) {
		return nil, errors.New("SNMP UDP connection timed out")
	}

	cfg := FingerprintConfig{
		QueueName: "Queue1",
		Redact:    true,
	}

	ctx := context.Background()
	rep, err := RunFingerprint(ctx, mockRunner, failingSNMPFactory, cfg)
	if err != nil {
		t.Fatalf("unexpected fatal error: %v", err)
	}

	if rep.SNMP == nil || rep.SNMP.DiscoveryMode != "SNMP_FAILED" {
		t.Errorf("expected SNMP DiscoveryMode SNMP_FAILED, got %+v", rep.SNMP)
	}

	// Without SNMP odometer and without IPP job-media-sheets, rating must be QUEUE_ONLY
	if rep.Telemetry.Rating != TelemetryRatingQueueOnly {
		t.Errorf("expected TelemetryRatingQueueOnly, got %s", rep.Telemetry.Rating)
	}
	if rep.Telemetry.SafeForPhysicalCompletion {
		t.Errorf("expected SafeForPhysicalCompletion = false")
	}
}

func TestRunFingerprint_LiveTestResponderIntegration(t *testing.T) {
	// Spins up in-process test SNMP responder on 127.0.0.1 (from snmp_emulator_test.go)
	responder := newTestSNMPResponder(t, 0, "")
	defer responder.Close()

	responder.UpdateState(func(s *emulatedSNMPState) {
		s.SysDescr = "XeroxGo In-Process Live Test Responder"
		s.MarkerLifeCount = 7788
		s.TonerLevel = 94
		s.TonerMax = 100
	})

	cfg := FingerprintConfig{
		SNMPHost:      "127.0.0.1",
		SNMPPort:      responder.Port(),
		SNMPCommunity: "public",
		SNMPTimeout:   500 * time.Millisecond,
		Redact:        false, // keep 127.0.0.1 unredacted for verification
	}

	// Use real UDP session factory pointing to the live local test responder
	realFactory := func(snmpCfg SNMPConfig) (DynamicSNMPSession, error) {
		return NewRealDynamicSNMPSession(context.Background(), snmpCfg)
	}

	ctx := context.Background()
	rep, err := RunFingerprint(ctx, nil, realFactory, cfg)
	if err != nil {
		t.Fatalf("RunFingerprint against live test responder failed: %v", err)
	}

	if rep.SNMP == nil {
		t.Fatalf("expected SNMP probe result, got nil")
	}
	if rep.SNMP.SysDescr != "XeroxGo In-Process Live Test Responder" {
		t.Errorf("expected sysDescr from test responder, got %q", rep.SNMP.SysDescr)
	}
	if len(rep.SNMP.Markers) != 1 || rep.SNMP.Markers[0].LifeCount != 7788 {
		t.Errorf("expected marker life count 7788, got %+v", rep.SNMP.Markers)
	}
	if len(rep.SNMP.Supplies) < 1 || rep.SNMP.Supplies[0].Level != 94 {
		t.Errorf("expected toner level 94, got %+v", rep.SNMP.Supplies)
	}
}
