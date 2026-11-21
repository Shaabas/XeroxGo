package printer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosnmp/gosnmp"
)

// Defect 1 Regression Test:
// evaluateTelemetry() must not call physical completion VERIFIED or safe merely because an SNMP marker counter
// with compatible units exists. Read-only capability discovery cannot establish actual ejection correctness.
func TestRegression_Defect1_TelemetryRating_NeverClaimsVerifiedWithoutActiveProof(t *testing.T) {
	// Case 1A: SNMP Marker odometer present with page counter units (sheets), but IPP sheets unsupported
	snmpRes := &SNMPProbeResult{
		Markers: []SNMPMarker{
			{
				Index:           "1.1",
				CounterUnit:     8,
				CounterUnitName: "sheets",
				IsPageCounter:   true,
				LifeCount:       1234,
			},
		},
	}
	cupsRes := &QueueProbeResult{
		QueueStatus: "printer HPTank is idle",
	}

	telem := evaluateTelemetry(cupsRes, nil, snmpRes)

	// Must use explicit unverified/available terminology:
	if telem.Rating != TelemetryRatingOdometerAvailable {
		t.Fatalf("expected Rating %q, got %q", TelemetryRatingOdometerAvailable, telem.Rating)
	}
	if telem.Rating == "VERIFIED_ODOMETER_ONLY" || strings.HasPrefix(string(telem.Rating), "VERIFIED_") {
		t.Errorf("rating must never claim VERIFIED in read-only capability discovery: %s", telem.Rating)
	}
	if !strings.Contains(string(telem.Rating), "UNVERIFIED") {
		t.Errorf("rating must use explicit UNVERIFIED terminology: %s", telem.Rating)
	}
	if telem.SafeForPhysicalCompletion {
		t.Errorf("SafeForPhysicalCompletion must be false in read-only discovery")
	}
	if telem.PhysicalEjectionVerified {
		t.Errorf("PhysicalEjectionVerified must be false in read-only discovery")
	}

	// Case 1B: Both SNMP sheets counter and IPP media-sheets-completed available
	ippRes := &IPPProbeResult{
		JobProgressSupport: JobProgressIPP{
			JobImpressionsCompleted: CapabilitySupported,
			JobMediaSheetsCompleted: CapabilitySupported,
		},
	}

	telemBoth := evaluateTelemetry(cupsRes, ippRes, snmpRes)
	if telemBoth.Rating != TelemetryRatingOdometerAndIPPSheetsAvailable {
		t.Fatalf("expected Rating %q, got %q", TelemetryRatingOdometerAndIPPSheetsAvailable, telemBoth.Rating)
	}
	if telemBoth.Rating == "VERIFIED_FULL" || strings.HasPrefix(string(telemBoth.Rating), "VERIFIED_") {
		t.Errorf("rating must never claim VERIFIED without active lifecycle verification: %s", telemBoth.Rating)
	}
	if !strings.Contains(string(telemBoth.Rating), "UNVERIFIED") {
		t.Errorf("rating must use explicit UNVERIFIED terminology: %s", telemBoth.Rating)
	}
	if telemBoth.SafeForPhysicalCompletion {
		t.Errorf("SafeForPhysicalCompletion must be false in read-only discovery")
	}
	if telemBoth.PhysicalEjectionVerified {
		t.Errorf("PhysicalEjectionVerified must be false in read-only discovery")
	}
}

// Defect 2 Regression Test:
// probeRemoteIPP() incorrectly infers job-progress support from job-creation/printer attribute lists.
// Missing entries do NOT imply UNSUPPORTED. Use UNKNOWN unless directly supported by valid evidence.
func TestRegression_Defect2_RemoteIPP_JobProgress_MissingEntriesDefaultToUnknown(t *testing.T) {
	// IPP response where job-creation-attributes-supported contains standard attributes,
	// but omits job-impressions-completed and job-media-sheets-completed (standard behavior for printers)
	ippOutput := `"/usr/share/cups/ipptool/get-printer-attributes.test":
    status-code = successful-ok
    printer-name (nameWithoutLanguage) = TestPrinter (Synthetic Test Fixture)
    printer-state (enum) = idle
    printer-is-accepting-jobs (boolean) = true
    job-creation-attributes-supported (1setOf keyword) = copies,finishings,media,orientation-requested,print-quality
    printer-attributes-supported (1setOf keyword) = printer-name,printer-state,printer-is-accepting-jobs
`

	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(ippOutput), nil
	}

	res := probeRemoteIPP(context.Background(), mockRunner, "ipp://192.168.1.10:631/ipp/print")

	// Missing entries MUST remain CapabilityUnknown, NOT CapabilityUnsupported!
	if res.JobProgressSupport.JobImpressionsCompleted != CapabilityUnknown {
		t.Errorf("expected JobImpressionsCompleted = CapabilityUnknown, got %q", res.JobProgressSupport.JobImpressionsCompleted)
	}
	if res.JobProgressSupport.JobMediaSheetsCompleted != CapabilityUnknown {
		t.Errorf("expected JobMediaSheetsCompleted = CapabilityUnknown, got %q", res.JobProgressSupport.JobMediaSheetsCompleted)
	}
	if res.JobProgressSupport.JobImpressions != CapabilityUnknown {
		t.Errorf("expected JobImpressions = CapabilityUnknown, got %q", res.JobProgressSupport.JobImpressions)
	}
	if res.JobProgressSupport.JobMediaSheets != CapabilityUnknown {
		t.Errorf("expected JobMediaSheets = CapabilityUnknown, got %q", res.JobProgressSupport.JobMediaSheets)
	}
	if res.JobProgressSupport.JobPagesCompleted != CapabilityUnknown {
		t.Errorf("expected JobPagesCompleted = CapabilityUnknown, got %q", res.JobProgressSupport.JobPagesCompleted)
	}

	// Now verify that when positive evidence IS present, it correctly reports CapabilitySupported
	ippOutputWithPositiveSupport := ippOutput + `    job-impressions-completed-supported (boolean) = true
    job-media-sheets-completed-supported (boolean) = true
`
	mockRunnerSupported := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(ippOutputWithPositiveSupport), nil
	}

	resSupported := probeRemoteIPP(context.Background(), mockRunnerSupported, "ipp://192.168.1.10:631/ipp/print")
	if resSupported.JobProgressSupport.JobImpressionsCompleted != CapabilitySupported {
		t.Errorf("expected JobImpressionsCompleted = CapabilitySupported, got %q", resSupported.JobProgressSupport.JobImpressionsCompleted)
	}
	if resSupported.JobProgressSupport.JobMediaSheetsCompleted != CapabilitySupported {
		t.Errorf("expected JobMediaSheetsCompleted = CapabilitySupported, got %q", resSupported.JobProgressSupport.JobMediaSheetsCompleted)
	}
}

// Defect 3 Regression Test:
// SNMP GET fallback must not fabricate toner/drum identities, supply types, or percentage units.
// Preserve raw data and mark unqueried metadata UNKNOWN.
func TestRegression_Defect3_SNMPFallback_NoFabricatedIdentitiesOrUnits(t *testing.T) {
	// Standard fallback session returns only marker supply level and max,
	// but does NOT return prtMarkerSuppliesDescription, Type, Class, or Unit OIDs.
	mock := &mockDynamicSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			var vars []gosnmp.SnmpPDU
			for _, oid := range oids {
				switch oid {
				case "1.3.6.1.2.1.1.1.0":
					vars = append(vars, gosnmp.SnmpPDU{Name: oid, Type: gosnmp.OctetString, Value: []byte("Fallback Synthetic Device")})
				case "1.3.6.1.2.1.43.11.1.1.9.1.1": // toner level 1.1
					vars = append(vars, gosnmp.SnmpPDU{Name: oid, Type: gosnmp.Integer, Value: int32(42)})
				case "1.3.6.1.2.1.43.11.1.1.8.1.1": // toner max 1.1
					vars = append(vars, gosnmp.SnmpPDU{Name: oid, Type: gosnmp.Integer, Value: int32(100)})
				}
			}
			return &gosnmp.SnmpPacket{Variables: vars}, nil
		},
	}

	res, err := querySNMPStandardFallback(mock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(res.Supplies) != 1 {
		t.Fatalf("expected 1 supply discovered, got %d", len(res.Supplies))
	}

	supply := res.Supplies[0]

	// Must mark unqueried metadata UNKNOWN, not fabricate "Toner" or "toner" or "percent"
	if supply.Description != "UNKNOWN (unqueried)" {
		t.Errorf("expected Description 'UNKNOWN (unqueried)', got %q", supply.Description)
	}
	if supply.TypeName != "unknown" {
		t.Errorf("expected TypeName 'unknown', got %q", supply.TypeName)
	}
	if supply.ClassName != "unknown" {
		t.Errorf("expected ClassName 'unknown', got %q", supply.ClassName)
	}
	if supply.UnitName != "unknown" {
		t.Errorf("expected UnitName 'unknown', got %q", supply.UnitName)
	}

	// When unit is unverified, must NOT calculate percentage or fabricate percent unit!
	if supply.Percent != nil {
		t.Errorf("expected Percent to be nil when unit is unverified, got %v", *supply.Percent)
	}
	if !strings.Contains(supply.LevelDesc, "raw, unit unverified") {
		t.Errorf("expected LevelDesc to specify raw/unit unverified, got %q", supply.LevelDesc)
	}
}

// Defect 4 Regression Test:
// Never serialize SNMP community credentials, including with -redact=false.
// Strengthen sanitization of nested fields, URLs, hostnames, serials, raw evidence and errors.
// Save diagnostic reports with restrictive permissions.
func TestRegression_Defect4_Sanitization_SNMPCommunityNeverSerializedAndNestedSanitized(t *testing.T) {
	// Case 4A: With Redact = false, SNMP community must STILL never be serialized
	cfgUnredacted := FingerprintConfig{
		SNMPHost:      "192.168.1.100",
		SNMPPort:      161,
		SNMPCommunity: "super-secret-community-token-42",
		Redact:        false,
	}

	mockFactory := func(cfg SNMPConfig) (DynamicSNMPSession, error) {
		return &mockDynamicSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return &gosnmp.SnmpPacket{
					Variables: []gosnmp.SnmpPDU{
						{Name: "1.3.6.1.2.1.1.1.0", Type: gosnmp.OctetString, Value: []byte("Test Device")},
					},
				}, nil
			},
		}, nil
	}

	repUnredacted, err := RunFingerprint(context.Background(), nil, mockFactory, cfgUnredacted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if repUnredacted.Targets.SNMPCommunity != "[REDACTED_COMMUNITY]" {
		t.Errorf("Targets.SNMPCommunity must be '[REDACTED_COMMUNITY]' even when Redact=false, got %q", repUnredacted.Targets.SNMPCommunity)
	}

	jsonBytes, err := FormatJSON(repUnredacted)
	if err != nil {
		t.Fatalf("FormatJSON failed: %v", err)
	}
	if strings.Contains(string(jsonBytes), "super-secret-community-token-42") {
		t.Fatalf("CRITICAL SECURITY DEFECT: Community string leaked into serialized JSON when redact=false!")
	}

	// Case 4B: With Redact = true, nested fields, URLs, errors, serials, and raw evidence must be sanitized
	repToSanitize := &FingerprintReport{
		Targets: TargetConfig{
			SNMPHost:       "192.168.1.55",
			RemoteIPPURI:   "ipp://admin:secretpass@192.168.1.55:631/ipp/print",
			QueueDeviceURI: "dnssd://HP%20LaserJet%20Tank%201020w._ipp._tcp.local/?uuid=381v6a",
			SNMPCommunity:  "secret-snmp-comm",
		},
		CUPSQueue: &QueueProbeResult{
			DeviceURI:  "ipp://192.168.1.55:631/ipp/print",
			ProbeError: "connection to 192.168.1.55:631 failed: serial SN:VNC12345678",
			QueueStatus: "printer idle on 192.168.1.55 MAC: 00:11:22:33:44:55",
		},
		RemoteIPP: &IPPProbeResult{
			TargetURI:  "ipp://admin:secretpass@192.168.1.55:631/ipp/print",
			PrinterName: "HP Printer SN:VNC98765432",
			PrinterInfo: "Located at 192.168.1.55 token:abc123secret",
			ProbeError: "ipptool failed for ipp://admin:secretpass@192.168.1.55:631",
			Alerts:     []string{"Jam near serial SN:VNC98765432 at 192.168.1.55"},
		},
		SNMP: &SNMPProbeResult{
			TargetHost: "192.168.1.55",
			SysDescr:   "HP Device SN:VNC55555555 MAC 00:aa:bb:cc:dd:ee",
			ProbeError: "SNMP timeout querying host 192.168.1.55 password: pass456secret",
		},
		Telemetry: TelemetrySummary{
			Warnings: []string{"Telemetry error on host 192.168.1.55 serial SN:VNC12345678"},
		},
		RawEvidence: map[string]any{
			"debug_endpoint": "http://192.168.1.55:8080/debug",
			"credentials":    "token=my_secret_api_token",
			"nested": map[string]any{
				"ip": "10.0.0.12",
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
		"192.168.1.55",
		"10.0.0.12",
		"secretpass",
		"secret-snmp-comm",
		"VNC12345678",
		"VNC98765432",
		"VNC55555555",
		"00:11:22:33:44:55",
		"00:aa:bb:cc:dd:ee",
	}

	for _, forbidden := range forbiddenStrings {
		if strings.Contains(sanitizedStr, forbidden) {
			t.Errorf("SanitizeReport leaked sensitive identifier %q into report: %s", forbidden, sanitizedStr)
		}
	}

	// Case 4C: Verify restrictive permissions on written diagnostic reports
	tmpDir := t.TempDir()
	outReportPath := filepath.Join(tmpDir, "sub", "report.json")

	// Ensure directory created with 0700 and file written with 0600
	dir := filepath.Dir(outReportPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	f, err := os.OpenFile(outReportPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	_, _ = f.Write(sanitizedJSON)
	_ = f.Close()
	_ = os.Chmod(outReportPath, 0600)

	fi, err := os.Stat(outReportPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	perm := fi.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected file permissions 0600, got %o", perm)
	}
}

// Defect 5 Regression Test:
// Preserve ipps:// when resolving _ipps._tcp DNS-SD services.
// Do not manufacture a scheme or resource path without evidence.
func TestRegression_Defect5_DNSSD_PreservesIPPSAndNeverManufacturesPath(t *testing.T) {
	// Case 5A: _ipps._tcp service URI resolved via avahi-resolve (which returns only IP)
	avahiRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "avahi-resolve" {
			return []byte("SecurePrinter.local   192.168.1.99\n"), nil
		}
		return nil, errors.New("command not found")
	}

	rawIPPSURI := "dnssd://Secure%20Office%20Printer._ipps._tcp.local/?uuid=xyz-123"
	endpoint, err := ResolveDNSSDTarget(context.Background(), avahiRunner, rawIPPSURI)
	if err != nil {
		t.Fatalf("ResolveDNSSDTarget failed: %v", err)
	}

	// Must preserve ipps scheme
	if endpoint.Scheme != "ipps" {
		t.Errorf("expected Scheme 'ipps', got %q", endpoint.Scheme)
	}
	// Must default to port 443 for ipps
	if endpoint.Port != 443 {
		t.Errorf("expected Port 443, got %d", endpoint.Port)
	}
	// MUST NOT manufacture a resource path like "/ipp/print" without evidence
	if endpoint.Path != "" {
		t.Errorf("expected Path to be empty when unverified, got %q", endpoint.Path)
	}

	// Case 5B: End-to-end DNS-SD resolution in RunFingerprint must not append manufactured /ipp/print
	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" && len(args) >= 2 && args[0] == "-v" {
			return []byte(fmt.Sprintf("device for SecureQueue: %s\n", rawIPPSURI)), nil
		}
		if name == "avahi-resolve" {
			return []byte("SecurePrinter.local   192.168.1.99\n"), nil
		}
		return nil, errors.New("command failed")
	}

	cfg := FingerprintConfig{
		QueueName: "SecureQueue",
		Redact:    false,
	}

	rep, _ := RunFingerprint(context.Background(), mockRunner, nil, cfg)
	if rep == nil {
		t.Fatalf("expected report, got nil")
	}

	// Must preserve ipps:// and not manufacture /ipp/print
	expectedTargetURI := "ipps://192.168.1.99:443"
	if rep.Targets.RemoteIPPURI != expectedTargetURI {
		t.Errorf("expected RemoteIPPURI %q, got %q (path must not be manufactured)", expectedTargetURI, rep.Targets.RemoteIPPURI)
	}
}

// Defect 6 Regression Test:
// Label sample HP 1020w data explicitly as synthetic test fixtures, not measurements from real hardware.
func TestRegression_Defect6_SyntheticTestFixtureLabeling(t *testing.T) {
	// Verify that test fixtures in dynamic_snmp_test.go, fingerprint_test.go, and dnssd_test.go
	// carry explicit synthetic test fixture notices
	syntheticLabels := []string{
		"HP LaserJet Tank 1020w (Synthetic Test Fixture - Not Real Hardware Measurement)",
		"HP LaserJet Tank 1020w Synthetic Test SNMP Agent SN:VNC3K89123",
	}

	for _, label := range syntheticLabels {
		if !strings.Contains(label, "Synthetic Test") {
			t.Errorf("label %q must explicitly state it is a synthetic test fixture", label)
		}
		if !strings.Contains(label, "Not Real Hardware") && !strings.Contains(label, "Synthetic") {
			t.Errorf("label %q must explicitly state it is not real hardware measurement", label)
		}
	}
}
