package printer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosnmp/gosnmp"
)

// Defect 1 & Item 1 Regression Test:
// In dynamic SNMP marker discovery, distinguish missing lifetime counts from actual zero values.
// Never construct a usable odometer from a missing map entry.
func TestRegression_SNMPMarker_DistinguishMissingFromZero(t *testing.T) {
	// Case 1A: SNMP table walk returns counter unit (sheets), but NO life count PDU
	mockMissingLife := &mockDynamicSession{
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			normRoot := normalizeOID(rootOid)
			if normRoot == normalizeOID(oidSubtreePrtMarker) {
				// Only prtMarkerCounterUnit (col 3) is returned for index 1.1; prtMarkerLifeCount (col 4) is absent
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.3.1.1", Type: gosnmp.Integer, Value: int32(8)}) // sheets
			}
			return nil
		},
	}

	markers, err := walkPrtMarkerTable(mockMissingLife)
	if err != nil {
		t.Fatalf("unexpected walk error: %v", err)
	}
	if len(markers) != 1 {
		t.Fatalf("expected 1 marker entry, got %d", len(markers))
	}
	if markers[0].HasLifeCount {
		t.Errorf("expected HasLifeCount = false when lifeCount PDU is missing from walk")
	}
	if markers[0].LifeCount != -1 {
		t.Errorf("expected LifeCount = -1 for missing count, got %d (must never default to 0)", markers[0].LifeCount)
	}

	// evaluateTelemetry must NOT construct a usable odometer from missing life count
	snmpRes := &SNMPProbeResult{Markers: markers}
	telem := evaluateTelemetry(nil, nil, snmpRes)
	if telem.HasSNMPMarkerCounter {
		t.Errorf("HasSNMPMarkerCounter must be false when life count is missing from map entry")
	}
	if telem.Rating == TelemetryRatingOdometerAvailable || telem.Rating == TelemetryRatingOdometerAndIPPSheetsAvailable {
		t.Errorf("Rating must not claim odometer available when life count is missing: %s", telem.Rating)
	}

	// Case 1B: Actual zero life count returned by printer (e.g. brand new printer or factory reset)
	mockZeroLife := &mockDynamicSession{
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			normRoot := normalizeOID(rootOid)
			if normRoot == normalizeOID(oidSubtreePrtMarker) {
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.3.1.1", Type: gosnmp.Integer, Value: int32(8)}) // sheets
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.4.1.1", Type: gosnmp.Counter32, Value: uint32(0)}) // actual 0 count
			}
			return nil
		},
	}

	markersZero, err := walkPrtMarkerTable(mockZeroLife)
	if err != nil {
		t.Fatalf("unexpected walk error: %v", err)
	}
	if len(markersZero) != 1 {
		t.Fatalf("expected 1 marker entry, got %d", len(markersZero))
	}
	if !markersZero[0].HasLifeCount {
		t.Errorf("expected HasLifeCount = true for actual zero value")
	}
	if markersZero[0].LifeCount != 0 {
		t.Errorf("expected LifeCount = 0, got %d", markersZero[0].LifeCount)
	}

	snmpResZero := &SNMPProbeResult{Markers: markersZero}
	telemZero := evaluateTelemetry(nil, nil, snmpResZero)
	if !telemZero.HasSNMPMarkerCounter {
		t.Errorf("HasSNMPMarkerCounter must be true when printer explicitly reports 0 life count")
	}
	if telemZero.SNMPMarkerLifeCount != 0 {
		t.Errorf("SNMPMarkerLifeCount must be 0, got %d", telemZero.SNMPMarkerLifeCount)
	}
	if telemZero.Rating != TelemetryRatingOdometerAvailable {
		t.Errorf("expected rating ODOMETER_AVAILABLE_UNVERIFIED_EJECTION, got %s", telemZero.Rating)
	}
}

// Item 2 Regression Test:
// When multiple markers exist, select valid counters deterministically.
// Preserve indexes and units. Do not depend on Go map iteration order.
func TestRegression_SNMPMarker_DeterministicCounterSelection(t *testing.T) {
	// Marker table with 3 markers provided in reverse index order:
	// "1.1": unit 11 (feet, non-page counter), life 5000
	// "1.2": unit 8 (sheets, valid page counter), life 1200
	// "1.3": unit 8 (sheets, valid page counter), life 3400
	markers := []SNMPMarker{
		{
			Index:           "1.3",
			CounterUnit:     8,
			CounterUnitName: "sheets",
			IsPageCounter:   true,
			HasLifeCount:    true,
			LifeCount:       3400,
		},
		{
			Index:           "1.1",
			CounterUnit:     11,
			CounterUnitName: "feet",
			IsPageCounter:   false,
			HasLifeCount:    true,
			LifeCount:       5000,
		},
		{
			Index:           "1.2",
			CounterUnit:     8,
			CounterUnitName: "sheets",
			IsPageCounter:   true,
			HasLifeCount:    true,
			LifeCount:       1200,
		},
	}

	snmpRes := &SNMPProbeResult{Markers: markers}
	telem := evaluateTelemetry(nil, nil, snmpRes)

	// Must deterministically select index "1.2" (the first valid page counter by index)
	if telem.SNMPMarkerIndex != "1.2" {
		t.Errorf("expected deterministic selection of index '1.2', got %q", telem.SNMPMarkerIndex)
	}
	if telem.SNMPMarkerLifeCount != 1200 {
		t.Errorf("expected life count 1200 from index 1.2, got %d", telem.SNMPMarkerLifeCount)
	}
	if telem.SNMPMarkerCounterUnit != "sheets" || !telem.SNMPMarkerUnitValid {
		t.Errorf("expected valid sheets counter unit, got %q (valid=%v)", telem.SNMPMarkerCounterUnit, telem.SNMPMarkerUnitValid)
	}

	// Verify deterministic sorting in dynamic table walk as well:
	mockWalkMulti := &mockDynamicSession{
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			normRoot := normalizeOID(rootOid)
			if normRoot == normalizeOID(oidSubtreePrtMarker) {
				// Feed PDUs in jumbled order
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.4.1.3", Type: gosnmp.Counter32, Value: uint32(3400)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.3.1.1", Type: gosnmp.Integer, Value: int32(11)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.4.1.1", Type: gosnmp.Counter32, Value: uint32(5000)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.3.1.2", Type: gosnmp.Integer, Value: int32(8)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.4.1.2", Type: gosnmp.Counter32, Value: uint32(1200)})
				_ = walkFn(gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.43.10.2.1.3.1.3", Type: gosnmp.Integer, Value: int32(8)})
			}
			return nil
		},
	}

	discovered, err := walkPrtMarkerTable(mockWalkMulti)
	if err != nil {
		t.Fatalf("unexpected walk error: %v", err)
	}
	if len(discovered) != 3 {
		t.Fatalf("expected 3 markers, got %d", len(discovered))
	}
	// Indices must be ordered strictly deterministically ("1.1", "1.2", "1.3")
	if discovered[0].Index != "1.1" || discovered[1].Index != "1.2" || discovered[2].Index != "1.3" {
		t.Errorf("markers must be sorted deterministically by row index, got [%s, %s, %s]",
			discovered[0].Index, discovered[1].Index, discovered[2].Index)
	}
}

// Item 3 Regression Test:
// Strengthen credential and identifier sanitization: public/private IPv4, IPv6, DNS-SD UUIDs, URI query secrets,
// raw evidence and errors. Never serialize URL credentials, even with -redact=false.
func TestRegression_Credentials_NeverSerializedAndSanitization(t *testing.T) {
	// Case 3A: With Redact = false, URL userinfo and SNMP community MUST NEVER be serialized
	cfgUnredacted := FingerprintConfig{
		RemoteIPPURI:  "ipp://admin:secretpass123@8.8.8.8:631/ipp/print",
		SNMPHost:      "8.8.8.8",
		SNMPPort:      161,
		SNMPCommunity: "my_secret_snmp_community",
		Redact:        false,
	}

	mockSNMPFact := func(cfg SNMPConfig) (DynamicSNMPSession, error) {
		return &mockDynamicSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return nil, errors.New("mock snmp get failed")
			},
			walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
				return errors.New("mock snmp walk failed")
			},
		}, nil
	}
	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("mock runner failed")
	}

	repUnredacted, err := RunFingerprint(context.Background(), mockRunner, mockSNMPFact, cfgUnredacted)
	if err != nil {
		t.Fatalf("RunFingerprint error: %v", err)
	}

	if strings.Contains(repUnredacted.Targets.RemoteIPPURI, "secretpass123") || strings.Contains(repUnredacted.Targets.RemoteIPPURI, "admin:") {
		t.Fatalf("CRITICAL SECURITY DEFECT: URL credentials leaked into Targets.RemoteIPPURI with Redact=false: %s", repUnredacted.Targets.RemoteIPPURI)
	}
	if repUnredacted.Targets.SNMPCommunity != "[REDACTED_COMMUNITY]" {
		t.Fatalf("Targets.SNMPCommunity must be '[REDACTED_COMMUNITY]' even when Redact=false, got %q", repUnredacted.Targets.SNMPCommunity)
	}

	jsonBytes, err := FormatJSON(repUnredacted)
	if err != nil {
		t.Fatalf("FormatJSON failed: %v", err)
	}
	jsonStr := string(jsonBytes)
	if strings.Contains(jsonStr, "secretpass123") {
		t.Fatalf("CRITICAL SECURITY DEFECT: URL password leaked into JSON output with Redact=false!")
	}
	if strings.Contains(jsonStr, "my_secret_snmp_community") {
		t.Fatalf("CRITICAL SECURITY DEFECT: SNMP community leaked into JSON output with Redact=false!")
	}

	// Case 3B: With Redact = true, test comprehensive sanitization:
	// Public IPv4 (8.8.8.8), private IPv4 (10.0.1.25), IPv6 (2001:db8::1), UUIDs, query secrets, nested evidence, errors
	repToSanitize := &FingerprintReport{
		Targets: TargetConfig{
			SNMPHost:       "8.8.8.8",
			RemoteIPPURI:   "ipp://user:pass@8.8.8.8:631/ipp/print?token=tok_secret123&uuid=123e4567-e89b-12d3-a456-426614174000",
			QueueDeviceURI: "dnssd://Printer._ipp._tcp.local/?uuid=381v6a-123&auth_token=super_auth_token",
			SNMPCommunity:  "snmp_secret_comm",
			DNSSDService:   "Printer uuid=381v6a-123 at 10.0.1.25",
		},
		CUPSQueue: &QueueProbeResult{
			DeviceURI:   "ipp://10.0.1.25:631/ipp/print?key=secret_api_key",
			ProbeError:  "CUPS error dialing 8.8.8.8:631 password=my_db_pass serial=SN1234567",
			QueueStatus: "printer idle on 10.0.1.25 MAC: 00:11:22:33:44:55",
		},
		RemoteIPP: &IPPProbeResult{
			TargetURI:   "ipp://8.8.8.8:631/ipp/print?secret=my_secret_token",
			PrinterName: "Printer SN:VNC98765432",
			PrinterInfo: "Located at 2001:db8::1 token:abc123secret",
			ProbeError:  "ipptool failed for ipp://user:pass@8.8.8.8:631 with community: public_comm",
			Alerts:      []string{"Jam near serial SN:VNC98765432 at 10.0.1.25 and IPv6 2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
		},
		SNMP: &SNMPProbeResult{
			TargetHost: "8.8.8.8",
			SysDescr:   "Device SN:VNC55555555 MAC 00:aa:bb:cc:dd:ee IPv6 fe80::1",
			ProbeError: "SNMP timeout on 8.8.8.8:161 password: pass456secret",
		},
		Telemetry: TelemetrySummary{
			Warnings: []string{"Telemetry error on host 8.8.8.8 serial SN:VNC12345678"},
		},
		RawEvidence: map[string]any{
			"public_ip":   "8.8.8.8",
			"private_ip":  "10.0.1.25",
			"ipv6":        "2001:db8::1",
			"credentials": "token=my_secret_api_token password=hidden_pass",
			"nested": map[string]any{
				"ip":        "192.168.1.99",
				"uuid":      "123e4567-e89b-12d3-a456-426614174000",
				"mac":       "11:22:33:44:55:66",
				"serial_no": "serial=ABC998877",
			},
		},
	}

	SanitizeReport(repToSanitize)

	sanitizedJSON, err := FormatJSON(repToSanitize)
	if err != nil {
		t.Fatalf("FormatJSON failed: %v", err)
	}
	sanitizedStr := string(sanitizedJSON)

	forbiddenStrings := []string{
		"8.8.8.8",
		"10.0.1.25",
		"192.168.1.99",
		"2001:db8::1",
		"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
		"fe80::1",
		"123e4567-e89b-12d3-a456-426614174000",
		"381v6a-123",
		"secretpass123",
		"super_auth_token",
		"secret_api_key",
		"my_db_pass",
		"my_secret_token",
		"abc123secret",
		"pass456secret",
		"my_secret_api_token",
		"hidden_pass",
		"SN1234567",
		"VNC98765432",
		"VNC55555555",
		"ABC998877",
		"00:11:22:33:44:55",
		"00:aa:bb:cc:dd:ee",
		"11:22:33:44:55:66",
	}

	for _, forbidden := range forbiddenStrings {
		if strings.Contains(sanitizedStr, forbidden) {
			t.Errorf("SanitizeReport leaked sensitive identifier %q into report: %s", forbidden, sanitizedStr)
		}
	}
}

// Item 4 Regression Test:
// DNS-SD hostname resolution must not imply a verified IPP endpoint.
// Verify the actual service URI/path or report remote IPP as UNKNOWN and request an explicit override.
func TestRegression_DNSSD_HostnameResolutionMustNotImplyVerifiedIPP(t *testing.T) {
	// Case 4A: avahi-resolve only resolves hostname to IP. Service URI/path is unverified!
	mockRunnerHostOnly := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" && len(args) >= 2 && args[0] == "-v" {
			return []byte("device for HPTank: dnssd://HPLaserJet._ipp._tcp.local/\n"), nil
		}
		if name == "avahi-resolve" {
			return []byte("HPLaserJet.local   192.168.1.65\n"), nil
		}
		return nil, errors.New("command failed")
	}

	cfg := FingerprintConfig{
		QueueName: "HPTank",
		Redact:    false,
	}

	rep, err := RunFingerprint(context.Background(), mockRunnerHostOnly, nil, cfg)
	if err != nil {
		t.Fatalf("RunFingerprint error: %v", err)
	}

	// Must NOT set a manufactured remote IPP URI like "ipp://192.168.1.65:631" or "ipp://192.168.1.65:631/ipp/print"
	if rep.Targets.RemoteIPPURI != "" {
		t.Errorf("RemoteIPPURI must be empty when actual service URI/path is unverified, got %q", rep.Targets.RemoteIPPURI)
	}

	// Remote IPP probe must report UNKNOWN and explicitly request override
	if rep.RemoteIPP == nil {
		t.Fatalf("expected RemoteIPP report, got nil")
	}
	if !strings.Contains(rep.RemoteIPP.ProbeError, "actual IPP service URI/path is unverified") {
		t.Errorf("expected ProbeError to state unverified path, got: %s", rep.RemoteIPP.ProbeError)
	}
	if !strings.Contains(rep.RemoteIPP.ProbeError, "--ipp-uri override") {
		t.Errorf("expected ProbeError to request explicit --ipp-uri override, got: %s", rep.RemoteIPP.ProbeError)
	}
	if rep.RemoteIPP.PDFSupport != CapabilityUnknown || rep.RemoteIPP.A4Support != CapabilityUnknown {
		t.Errorf("remote capabilities must be UNKNOWN, got PDF=%s A4=%s", rep.RemoteIPP.PDFSupport, rep.RemoteIPP.A4Support)
	}

	// Hostname resolution CAN still be used as fallback for SNMP
	if rep.Targets.SNMPHost != "192.168.1.65" {
		t.Errorf("expected SNMP target host 192.168.1.65 resolved from DNS-SD host, got %q", rep.Targets.SNMPHost)
	}

	// Case 4B: When ippfind successfully discovers and verifies the actual service URI/path
	mockRunnerVerifiedPath := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" && len(args) >= 2 && args[0] == "-v" {
			return []byte("device for HPTank: dnssd://HPLaserJet._ipp._tcp.local/\n"), nil
		}
		if name == "ippfind" {
			return []byte("ipp://192.168.1.65:631/ipp/print\n"), nil
		}
		if name == "ipptool" {
			return []byte(`status-code = successful-ok
printer-name = "HPLaserJet"
document-format-supported = "application/pdf"
`), nil
		}
		return nil, errors.New("command failed")
	}

	repVerified, err := RunFingerprint(context.Background(), mockRunnerVerifiedPath, nil, cfg)
	if err != nil {
		t.Fatalf("RunFingerprint error: %v", err)
	}

	if repVerified.Targets.RemoteIPPURI != "ipp://192.168.1.65:631/ipp/print" {
		t.Errorf("expected verified URI 'ipp://192.168.1.65:631/ipp/print', got %q", repVerified.Targets.RemoteIPPURI)
	}
	if repVerified.RemoteIPP.PDFSupport != CapabilitySupported {
		t.Errorf("expected PDFSupport = SUPPORTED on verified endpoint, got %s", repVerified.RemoteIPP.PDFSupport)
	}
}

// Item 5 Regression Test:
// Replace the self-contained permissions and fixture-label tests with tests that exercise actual
// production diagnostic behavior.
func TestRegression_DiagnosticBehavior_PermissionsAndFixtureLabeling(t *testing.T) {
	// Case 5A: Exercise production SaveReportFile ensuring restrictive 0600 file and 0700 dir permissions
	tmpDir := t.TempDir()
	outReportPath := filepath.Join(tmpDir, "nested", "diag_report.json")

	testPayload := []byte(`{"status":"diagnostic_test_payload"}`)

	// Call production SaveReportFile function
	if err := SaveReportFile(outReportPath, testPayload); err != nil {
		t.Fatalf("production SaveReportFile failed: %v", err)
	}

	// Verify directory permissions
	dirInfo, err := os.Stat(filepath.Dir(outReportPath))
	if err != nil {
		t.Fatalf("Stat dir failed: %v", err)
	}
	dirPerm := dirInfo.Mode().Perm()
	if dirPerm != 0700 {
		t.Errorf("expected directory permissions 0700, got %o", dirPerm)
	}

	// Verify file permissions
	fileInfo, err := os.Stat(outReportPath)
	if err != nil {
		t.Fatalf("Stat file failed: %v", err)
	}
	filePerm := fileInfo.Mode().Perm()
	if filePerm != 0600 {
		t.Errorf("expected file permissions 0600, got %o", filePerm)
	}

	// Verify existing file with wide permissions is clamped down to 0600
	wideFilePath := filepath.Join(tmpDir, "wide.json")
	if err := os.WriteFile(wideFilePath, []byte("pre-existing"), 0666); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := SaveReportFile(wideFilePath, testPayload); err != nil {
		t.Fatalf("SaveReportFile failed: %v", err)
	}
	wideInfo, _ := os.Stat(wideFilePath)
	if wideInfo.Mode().Perm() != 0600 {
		t.Errorf("SaveReportFile failed to restrict pre-existing wide file: got %o, want 0600", wideInfo.Mode().Perm())
	}

	// Case 5B: Exercise actual production diagnostic pipeline and formatting for synthetic fixture labeling
	cupsRaw := `status-code = successful-ok
printer-make-and-model = "HP LaserJet Tank 1020w (Synthetic Test Fixture - Not Real Hardware Measurement)"
printer-name = "HPTank (Synthetic Test Fixture)"
document-format-supported = "application/pdf"
`
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			return []byte("device for HPTank: ipp://10.0.0.1/ipp/print\n"), nil
		}
		if name == "ipptool" {
			return []byte(cupsRaw), nil
		}
		return nil, errors.New("command failed")
	}

	cfg := FingerprintConfig{
		QueueName: "HPTank",
		Redact:    false,
	}

	rep, err := RunFingerprint(context.Background(), runner, nil, cfg)
	if err != nil {
		t.Fatalf("RunFingerprint error: %v", err)
	}

	// Check production structured JSON and human-readable formatters
	humanOutput := FormatHumanReadable(rep)
	jsonBytes, err := FormatJSON(rep)
	if err != nil {
		t.Fatalf("FormatJSON error: %v", err)
	}
	jsonOutput := string(jsonBytes)

	expectedLabel := "HP LaserJet Tank 1020w (Synthetic Test Fixture - Not Real Hardware Measurement)"

	if !strings.Contains(humanOutput, expectedLabel) {
		t.Errorf("production human-readable output missing synthetic label %q: %s", expectedLabel, humanOutput)
	}
	if !strings.Contains(jsonOutput, expectedLabel) {
		t.Errorf("production JSON output missing synthetic label %q: %s", expectedLabel, jsonOutput)
	}
}

// Item 6 Regression Test:
// Ensure missing or failed IPP observations never yield unsupported or verified-progress claims without valid evidence.
func TestRegression_IPP_FailedObservationsNeverYieldUnsupportedOrVerifiedProgress(t *testing.T) {
	// Case 6A: IPP probe completely fails (command execution error / timeout)
	failingRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("connection refused dialing IPP endpoint: 631")
	}

	resFailed := probeRemoteIPP(context.Background(), failingRunner, "ipp://192.168.1.50:631/ipp/print")

	if resFailed.ProbeError == "" {
		t.Errorf("expected ProbeError to be set on failed IPP query")
	}
	// Capabilities MUST remain CapabilityUnknown, NOT CapabilityUnsupported!
	if resFailed.PDFSupport != CapabilityUnknown {
		t.Errorf("expected PDFSupport = CapabilityUnknown on failure, got %s", resFailed.PDFSupport)
	}
	if resFailed.PWGRasterSupport != CapabilityUnknown {
		t.Errorf("expected PWGRasterSupport = CapabilityUnknown on failure, got %s", resFailed.PWGRasterSupport)
	}
	if resFailed.A4Support != CapabilityUnknown {
		t.Errorf("expected A4Support = CapabilityUnknown on failure, got %s", resFailed.A4Support)
	}
	if resFailed.ColorSupport != CapabilityUnknown {
		t.Errorf("expected ColorSupport = CapabilityUnknown on failure, got %s", resFailed.ColorSupport)
	}
	if resFailed.JobProgressSupport.JobImpressionsCompleted != CapabilityUnknown {
		t.Errorf("expected JobImpressionsCompleted = CapabilityUnknown on failure, got %s", resFailed.JobProgressSupport.JobImpressionsCompleted)
	}
	if resFailed.JobProgressSupport.JobMediaSheetsCompleted != CapabilityUnknown {
		t.Errorf("expected JobMediaSheetsCompleted = CapabilityUnknown on failure, got %s", resFailed.JobProgressSupport.JobMediaSheetsCompleted)
	}

	// evaluateTelemetry with failed IPP must never claim progress or verified completion
	telemFailed := evaluateTelemetry(nil, resFailed, nil)
	if telemFailed.IPPJobProgressSupported || telemFailed.IPPMediaSheetsSupported {
		t.Errorf("telemetry must not claim IPP progress supported when probe failed")
	}
	if telemFailed.SafeForPhysicalCompletion || telemFailed.PhysicalEjectionVerified {
		t.Errorf("telemetry must not claim physical completion safe/verified on failed probe")
	}
	if telemFailed.Rating != TelemetryRatingAbsent {
		t.Errorf("expected rating ABSENT, got %s", telemFailed.Rating)
	}

	// Case 6B: IPP returns client-error status (e.g. client-error-not-found)
	errorStatusRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(`"/usr/share/cups/ipptool/get-printer-attributes.test":
    status-code = client-error-not-found
`), nil
	}

	resStatusErr := probeRemoteIPP(context.Background(), errorStatusRunner, "ipp://192.168.1.50:631/ipp/print")
	if resStatusErr.ProbeError == "" {
		t.Errorf("expected ProbeError on client-error status")
	}
	if resStatusErr.PDFSupport != CapabilityUnknown || resStatusErr.A4Support != CapabilityUnknown {
		t.Errorf("capabilities must remain UNKNOWN on error status response")
	}
	if resStatusErr.JobProgressSupport.JobImpressionsCompleted != CapabilityUnknown {
		t.Errorf("job progress must remain UNKNOWN on error status response")
	}

	// Case 6C: IPP response is successful-ok, but document-format-supported is omitted entirely
	missingFormatsRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(`"/usr/share/cups/ipptool/get-printer-attributes.test":
    status-code = successful-ok
    printer-name = "TestPrinter"
    media-supported = "iso_a4_210x297mm"
`), nil
	}

	resMissing := probeRemoteIPP(context.Background(), missingFormatsRunner, "ipp://192.168.1.50:631/ipp/print")
	// PDF support was omitted, so it must be CapabilityUnknown (NOT CapabilityUnsupported)
	if resMissing.PDFSupport != CapabilityUnknown {
		t.Errorf("missing document-format-supported must remain CapabilityUnknown, got %s", resMissing.PDFSupport)
	}
	// Media was provided, so A4 is CapabilitySupported
	if resMissing.A4Support != CapabilitySupported {
		t.Errorf("A4Support must be CapabilitySupported, got %s", resMissing.A4Support)
	}
}
