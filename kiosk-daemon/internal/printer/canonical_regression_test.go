package printer

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
	"xeroxgo/pkg/contracts"
)

func TestCanonicalIPP_LocalCUPSvsNativeIsolation_AndResolution(t *testing.T) {
	ctx := context.Background()

	// 1. Local CUPS IPP and native physical IPP must carry distinct origin tags.
	cupsRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("printer-state (enum) = idle\nprinter-is-accepting-jobs (boolean) = true\n"), nil
	}
	cupsRes, err := QueryIPPPrinterState(ctx, cupsRunner, "HPTank")
	if err != nil {
		t.Fatalf("QueryIPPPrinterState failed: %v", err)
	}
	if cupsRes.Source != CapabilitySourceCUPSQueue {
		t.Fatalf("expected CUPS query source %q, got %q", CapabilitySourceCUPSQueue, cupsRes.Source)
	}

	nativeRes, err := QueryNativeIPPPrinterState(ctx, cupsRunner, "ipp://10.42.0.210:631/ipp/print")
	if err != nil {
		t.Fatalf("QueryNativeIPPPrinterState failed: %v", err)
	}
	if nativeRes.Source != CapabilitySourceNativeIPP {
		t.Fatalf("expected native query source %q, got %q", CapabilitySourceNativeIPP, nativeRes.Source)
	}

	// 2. QueryNativeIPPPrinterState must refuse local CUPS queue URIs so CUPS is never disguised as physical IPP.
	for _, localURI := range []string{
		"ipp://localhost:631/printers/HPTank",
		"ipp://127.0.0.1:631/printers/HPTank",
		"",
		"usb://HP/LaserJet%20Tank%201020",
	} {
		res, err := QueryNativeIPPPrinterState(ctx, cupsRunner, localURI)
		if err == nil {
			t.Fatalf("expected QueryNativeIPPPrinterState(%q) to fail, got %+v", localURI, res)
		}
		if res.QueueAccessible || res.EndpointReachable {
			t.Fatalf("rejected native URI %q must not report accessible/reachable: %+v", localURI, res)
		}
	}

	// 3. ResolveNativeIPPEndpoint must fail closed when DNS-SD does not verify an IPP resource path (no /ipp/print guess)
	//    and must never fall back to a hardcoded demo printer IP.
	unverifiedDNSSDRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lpstat":
			return []byte("device for HPTank: dnssd://HPLaserJet._ipp._tcp.local/\n"), nil
		case "avahi-resolve":
			return []byte("HPLaserJet.local   10.42.0.210\n"), nil
		default:
			return nil, context.DeadlineExceeded
		}
	}
	resolved, err := ResolveNativeIPPEndpoint(ctx, unverifiedDNSSDRunner, "HPTank", "")
	if err == nil || resolved != "" {
		t.Fatalf("expected unverified DNS-SD path to fail closed, got %q (err=%v)", resolved, err)
	}

	// 4. Verified DNS-SD rp= path resolves cleanly via ippfind without hardcoding.
	verifiedDNSSDRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lpstat":
			return []byte("device for HPTank: dnssd://HPLaserJet._ipp._tcp.local/\n"), nil
		case "ippfind":
			return []byte("ipp://10.42.0.210:631/ipp/custom\n"), nil
		default:
			return nil, nil
		}
	}
	resolved, err = ResolveNativeIPPEndpoint(ctx, verifiedDNSSDRunner, "HPTank", "")
	if err != nil {
		t.Fatalf("expected verified DNS-SD resolution to succeed, got err: %v", err)
	}
	if !strings.Contains(resolved, "10.42.0.210:631/ipp/custom") {
		t.Fatalf("expected resolved URI with rp=ipp/custom, got %q", resolved)
	}
}

func TestCanonicalSNMP_DynamicDiscovery_TargetedRecurringPoll_AndRediscovery(t *testing.T) {
	ctx := context.Background()

	var fullSupplyWalks atomic.Int32
	var targetedGets atomic.Int32
	var alertWalks atomic.Int32
	var simulateIndexShift atomic.Bool

	session := &mockDynamicSession{
		walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
			switch normalizeOID(rootOid) {
			case "1.3.6.1.2.1.25.3.2.1.5": // hrDeviceStatus
				return walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.25.3.2.1.5.2", Type: gosnmp.Integer, Value: 2})
			case oidSubtreeHRPrinter:
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.25.3.5.1.1.2", Type: gosnmp.Integer, Value: 3}) // idle
				return walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.25.3.5.1.2.2", Type: gosnmp.OctetString, Value: []byte{0x00}})
			case "1.3.6.1.2.1.43.8.2.1": // prtInputTable
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.8.2.1.9.2.3", Type: gosnmp.Integer, Value: 150}) // maxCapacity
				return walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.8.2.1.10.2.3", Type: gosnmp.Integer, Value: -2}) // unknown level != 0
			case oidSubtreePrtCover:
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.6.1.1.2.2.1", Type: gosnmp.OctetString, Value: []byte("Top Cover")})
				return walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.6.1.1.3.2.1", Type: gosnmp.Integer, Value: 4}) // doorClosed
			case oidSubtreePrtMarker:
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.10.2.1.3.2.1", Type: gosnmp.Integer, Value: 8}) // sheets
				return walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.10.2.1.4.2.1", Type: gosnmp.Counter32, Value: uint32(1234)})
			case oidSubtreePrtSupplies:
				fullSupplyWalks.Add(1)
				tonerIdx := "2.2"
				if simulateIndexShift.Load() {
					tonerIdx = "2.9"
				}
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.4." + tonerIdx, Type: gosnmp.Integer, Value: 3}) // toner
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.5." + tonerIdx, Type: gosnmp.Integer, Value: 3})
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.6." + tonerIdx, Type: gosnmp.OctetString, Value: []byte("Black Toner")})
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.7." + tonerIdx, Type: gosnmp.Integer, Value: 19}) // percent
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.8." + tonerIdx, Type: gosnmp.Integer, Value: 100})
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.9." + tonerIdx, Type: gosnmp.Integer, Value: 82})

				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.4.2.5", Type: gosnmp.Integer, Value: 9}) // opc/drum
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.5.2.5", Type: gosnmp.Integer, Value: 9})
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.6.2.5", Type: gosnmp.OctetString, Value: []byte("Imaging Drum")})
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.7.2.5", Type: gosnmp.Integer, Value: 19})
				_ = walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.8.2.5", Type: gosnmp.Integer, Value: 100})
				return walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.43.11.1.1.9.2.5", Type: gosnmp.Integer, Value: 0}) // 0% drum non-blocking
			case oidSubtreePrtAlert:
				alertWalks.Add(1)
				return nil
			}
			return nil
		},
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			targetedGets.Add(1)
			if simulateIndexShift.Load() {
				return &gosnmp.SnmpPacket{}, nil // trigger rediscovery when cached index vanishes
			}
			var vars []gosnmp.SnmpPDU
			for _, oid := range oids {
				n := normalizeOID(oid)
				switch n {
				case "1.3.6.1.2.1.25.3.2.1.5.2":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 2})
				case "1.3.6.1.2.1.25.3.5.1.1.2":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 3})
				case "1.3.6.1.2.1.25.3.5.1.2.2":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.OctetString, Value: []byte{0x00}})
				case "1.3.6.1.2.1.43.8.2.1.9.2.3":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 150})
				case "1.3.6.1.2.1.43.8.2.1.10.2.3":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: -2})
				case "1.3.6.1.2.1.43.11.1.1.9.2.2":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 82})
				case "1.3.6.1.2.1.43.11.1.1.8.2.2":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 100})
				case "1.3.6.1.2.1.43.11.1.1.9.2.5":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 0})
				case "1.3.6.1.2.1.43.11.1.1.8.2.5":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 100})
				case "1.3.6.1.2.1.43.6.1.1.3.2.1":
					vars = append(vars, gosnmp.SnmpPDU{Name: "." + n, Type: gosnmp.Integer, Value: 4})
				}
			}
			return &gosnmp.SnmpPacket{Variables: vars}, nil
		},
	}

	cfg := DefaultSNMPConfig()
	cfg.Target = "10.42.0.210"
	mon := NewSNMPMonitor(cfg, func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
		return session, nil
	})

	// Tick 1: full startup table discovery discovers non-.1.1 indexes (.2.2, .2.3, .2.5).
	obs1 := mon.Poll(ctx, 120)
	if !obs1.Available || obs1.HasBlockingFault {
		t.Fatalf("expected healthy available SNMP observation, got %+v", obs1)
	}
	if fullSupplyWalks.Load() != 1 {
		t.Fatalf("expected 1 startup supply table walk, got %d", fullSupplyWalks.Load())
	}
	if !obs1.TonerKnown || obs1.TonerPercent != 82 {
		t.Fatalf("expected dynamic toner index .2.2 => 82%% known, got %d (known=%v)", obs1.TonerPercent, obs1.TonerKnown)
	}
	if !obs1.InputTrayCapacityKnown || obs1.InputTrayCapacity != 150 || obs1.InputTrayLevelKnown {
		t.Fatalf("expected -2 input quantity to remain unknown with cap 150, got lvl=%d known=%v cap=%d",
			obs1.InputTrayLevel, obs1.InputTrayLevelKnown, obs1.InputTrayCapacity)
	}

	// Tick 2 & 3: recurring polls must use targeted GETs + bounded alert walk WITHOUT re-walking supplies table.
	_ = mon.Poll(ctx, 120)
	_ = mon.Poll(ctx, 120)
	if fullSupplyWalks.Load() != 1 {
		t.Fatalf("expected no recurring full supply walks after startup discovery, got %d", fullSupplyWalks.Load())
	}
	if targetedGets.Load() < 2 {
		t.Fatalf("expected recurring targeted GET polls, got %d", targetedGets.Load())
	}
	if alertWalks.Load() < 3 {
		t.Fatalf("expected alert walk on each cycle, got %d", alertWalks.Load())
	}

	// Simulate cached index invalidation -> triggers rediscovery safely.
	simulateIndexShift.Store(true)
	obsShift := mon.Poll(ctx, 120)
	if !obsShift.Available || fullSupplyWalks.Load() != 2 {
		t.Fatalf("expected rediscovery walk on index change (walks=%d, avail=%v)", fullSupplyWalks.Load(), obsShift.Available)
	}
}

// TestCanonicalEvaluator_HPTank1020wRecordedFixture encodes the recorded HP LaserJet Tank 1020w
// multi-source hardware observation evidence fixture (non-live test fixture):
//   - CUPS local IPP: media-jam-error
//   - Physical Native IPP: media-empty-report (must block despite -report severity suffix)
//   - SNMP: hrDeviceStatus=5 (down), hrPrinterStatus=1 (other), error flags=0x40 (noPaper),
//     critical alert sev=3 group=8 code=801 "TRAY EMPTY", input capacity=150, input level=-2 (unknown),
//     toner .1.1 = 93%, drum .1.5 = 0% unverified
func TestCanonicalEvaluator_HPTank1020wRecordedFixture(t *testing.T) {
	now := time.Now().UTC()
	floatPtr := func(v float64) *float64 { return &v }

	hpSNMPProbe := &SNMPProbeResult{
		TargetHost: "10.42.0.210",
		Printers: []SNMPPrinterDevice{
			{
				Index:        "1",
				DeviceStatus: 5, // hrDeviceStatus down
				Status:       1, // hrPrinterStatus other
				ErrorFlags: PrinterErrorFlags{
					NoPaper: true, // 0x40
				},
			},
		},
		Inputs: []SNMPInputTray{
			{
				Index:         "1.1",
				Name:          "Tray 1",
				MaxCapacity:   150,
				CurrentLevel:  -2, // unknown in RFC 3805 (NOT zero)
				LevelKnown:    false,
				CapacityKnown: true,
			},
		},
		Supplies: []SNMPSupply{
			{Index: "1.1", Description: "Black Cartridge", Class: 3, Type: 3, Unit: 19, MaxCapacity: 100, Level: 93, Percent: floatPtr(93)},
			{Index: "1.5", Description: "Imaging Drum", Class: 3, Type: 9, Unit: 0, MaxCapacity: 0, Level: -2, Percent: nil},
		},
		Alerts: []SNMPAlert{
			{Index: "1", Severity: 3, Training: 4, Group: 8, Code: 801, Description: "TRAY EMPTY"},
		},
	}
	snmpObs := EvaluateSNMPProbe(hpSNMPProbe, 50, now)
	if !snmpObs.HasBlockingFault || snmpObs.InputTrayLevelKnown || snmpObs.InputTrayCapacity != 150 || snmpObs.DrumKnown {
		t.Fatalf("unexpected HP Tank 1020w SNMP interpretation: %+v", snmpObs)
	}

	cupsHealth := parseCUPSHealthOutput([]byte("printer HPTank disabled since ...\n\tmedia-jam-error\n"), 50, now)
	cupsHealth.UpdatedAt = now
	cupsValid := true

	nativeIPP := ParseIPPPrinterStateWithSource(
		[]byte("printer-state (enum) = idle\nprinter-is-accepting-jobs (boolean) = true\nprinter-state-reasons (keyword) = media-empty-report\n"),
		CapabilitySourceNativeIPP,
		"HPTank",
		"ipp://10.42.0.210:631/ipp/print",
	)
	nativeIPP.EndpointReachable = true
	nativeIPP.QueueAccessible = true
	nativeIPP.ObservedAt = now

	res := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
		CUPS:             cupsHealth,
		CUPSValid:        &cupsValid,
		CUPSQueried:      true,
		IPP:              nativeIPP,
		RequireNativeIPP: true,
		SNMP:             snmpObs,
		SNMPQueried:      true,
		RequireSNMP:      true,
		RequireIPP:       true,
		RequireCUPS:      true,
		EvalTime:         now,
		MaxSourceAge:     DefaultMaxSourceAge,
	})

	if res.EvaluatedHealth != contracts.EvaluatedHealthFault {
		t.Fatalf("expected FAULT on HP Tank 1020w fixture, got %s", res.EvaluatedHealth)
	}
	if res.IsReady() {
		t.Fatal("expected IsReady() == false on HP Tank 1020w fault fixture")
	}
	if !res.PaperJam || res.PaperTrayStatus != "EMPTY" {
		t.Fatalf("expected paper_jam=true and PaperTrayStatus=EMPTY, got jam=%v tray=%s", res.PaperJam, res.PaperTrayStatus)
	}
	if !res.TonerLevelKnown || res.TonerLevelPercent != 93 {
		t.Fatalf("expected known toner 93%%, got %d (known=%v)", res.TonerLevelPercent, res.TonerLevelKnown)
	}
	if res.DrumLifeKnown {
		t.Fatalf("expected unverified drum to remain unknown, got %d (known=%v)", res.DrumLifePercent, res.DrumLifeKnown)
	}
	if res.InputTrayLevelKnown || !res.InputTrayCapacityKnown || res.InputTrayCapacity != 150 {
		t.Fatalf("expected input capacity 150 known and level unknown (!= 0), got cap=%d/%v lvl=%d/%v",
			res.InputTrayCapacity, res.InputTrayCapacityKnown, res.InputTrayLevel, res.InputTrayLevelKnown)
	}

	// Contrast: low toner + 0% / unknown drum alone on healthy device MUST NOT block printing.
	healthyProbe := &SNMPProbeResult{
		TargetHost: "10.42.0.210",
		Printers: []SNMPPrinterDevice{
			{Index: "1", DeviceStatus: 2, Status: 3},
		},
		Supplies: []SNMPSupply{
			{Index: "1.1", Description: "Black Cartridge", Class: 3, Type: 3, Unit: 19, MaxCapacity: 100, Level: 8, Percent: floatPtr(8)},
			{Index: "1.5", Description: "Imaging Drum", Class: 3, Type: 9, Unit: 19, MaxCapacity: 100, Level: 0, Percent: floatPtr(0)},
		},
	}
	healthyObs := EvaluateSNMPProbe(healthyProbe, 50, now)
	healthyCUPS := contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: now}
	healthyIPP := IPPPrinterStateResult{
		Source: CapabilitySourceNativeIPP, Queue: "HPTank", TargetURI: "ipp://10.42.0.210:631/ipp/print",
		State: IPPPrinterStateIdle, AcceptingJobs: AcceptingJobsAccepting,
		EndpointReachable: true, QueueAccessible: true, ObservedAt: now,
	}
	resHealthy := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
		CUPS:             healthyCUPS,
		CUPSValid:        &cupsValid,
		CUPSQueried:      true,
		IPP:              healthyIPP,
		RequireNativeIPP: true,
		SNMP:             healthyObs,
		SNMPQueried:      true,
		RequireSNMP:      true,
		RequireIPP:       true,
		RequireCUPS:      true,
		EvalTime:         now,
	})
	if resHealthy.EvaluatedHealth != contracts.EvaluatedHealthHealthy || !resHealthy.IsReady() {
		t.Fatalf("expected low toner + 0%% drum alone to stay HEALTHY, got %s faults=%v unknown=%v",
			resHealthy.EvaluatedHealth, resHealthy.BlockingFaults, resHealthy.UnknownConditions)
	}
	if len(resHealthy.NonblockingWarnings) == 0 {
		t.Fatalf("expected low-toner warning to be preserved, got empty warnings")
	}

	// Freshness expiry (> 6s) transitions readiness to UNVERIFIED.
	resStale := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
		CUPS:             healthyCUPS,
		CUPSValid:        &cupsValid,
		CUPSQueried:      true,
		IPP:              healthyIPP,
		RequireNativeIPP: true,
		SNMP:             healthyObs,
		SNMPQueried:      true,
		RequireSNMP:      true,
		RequireIPP:       true,
		RequireCUPS:      true,
		EvalTime:         now.Add(7 * time.Second),
		MaxSourceAge:     DefaultMaxSourceAge,
	})
	if resStale.EvaluatedHealth != contracts.EvaluatedHealthUnverified || resStale.IsReady() {
		t.Fatalf("expected stale >6s evidence to become UNVERIFIED, got %s", resStale.EvaluatedHealth)
	}
}

func TestCanonicalFingerprint_ReusesCanonicalReaders(t *testing.T) {
	ctx := context.Background()
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lpstat":
			if len(args) > 0 && args[0] == "-v" {
				return []byte("device for HPTank: ipp://10.42.0.210:631/ipp/print\n"), nil
			}
			return []byte("printer HPTank is idle. enabled since now\n"), nil
		case "ipptool":
			return []byte("printer-state (enum) = idle\nprinter-is-accepting-jobs (boolean) = true\ndocument-format-supported (mimeMediaType) = application/pdf\n"), nil
		default:
			return nil, nil
		}
	}

	cupsObs := probeCUPSQueue(ctx, runner, "HPTank")
	if cupsObs == nil || cupsObs.ProbeError != "" || cupsObs.PrinterState != IPPPrinterStateIdle || cupsObs.DeviceURI != "ipp://10.42.0.210:631/ipp/print" {
		t.Fatalf("expected clean CUPS probe observation via canonical reader, got %+v", cupsObs)
	}

	nativeObs := probeRemoteIPP(ctx, runner, cupsObs.DeviceURI)
	if nativeObs == nil || nativeObs.ProbeError != "" || nativeObs.PrinterState != IPPPrinterStateIdle || nativeObs.TargetURI != "ipp://10.42.0.210:631/ipp/print" {
		t.Fatalf("expected clean remote IPP probe via canonical reader, got %+v", nativeObs)
	}
}
