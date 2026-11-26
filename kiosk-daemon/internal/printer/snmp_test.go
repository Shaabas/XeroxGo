package printer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

type fakeSnmpSession struct {
	getFunc   func(oids []string) (*gosnmp.SnmpPacket, error)
	walkFunc  func(rootOid string, walkFn gosnmp.WalkFunc) error
	closeFunc func() error
	closed    bool
	gotOIDs   []string
}

func (f *fakeSnmpSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	f.gotOIDs = append([]string(nil), oids...)
	if f.getFunc != nil {
		return f.getFunc(oids)
	}
	return nil, errors.New("getFunc not implemented")
}

func (f *fakeSnmpSession) Walk(rootOid string, walkFn gosnmp.WalkFunc) error {
	if f.walkFunc != nil {
		return f.walkFunc(rootOid, walkFn)
	}
	return nil
}

func (f *fakeSnmpSession) Close() error {
	f.closed = true
	if f.closeFunc != nil {
		return f.closeFunc()
	}
	return nil
}

func floatPtrTest(v float64) *float64 {
	return &v
}

func TestDecodeErrorBitmask(t *testing.T) {
	t.Run("empty_slice_returns_zero_flags", func(t *testing.T) {
		flags := DecodeErrorBitmask(nil)
		if flags != (PrinterErrorFlags{}) {
			t.Errorf("expected empty flags for nil input, got %+v", flags)
		}
		flags = DecodeErrorBitmask([]byte{})
		if flags != (PrinterErrorFlags{}) {
			t.Errorf("expected empty flags for empty slice, got %+v", flags)
		}
	})

	t.Run("individual_byte0_bits", func(t *testing.T) {
		tests := []struct {
			name   string
			b0     byte
			verify func(PrinterErrorFlags) bool
		}{
			{"LowPaper_0x80", 0x80, func(f PrinterErrorFlags) bool { return f.LowPaper }},
			{"NoPaper_0x40", 0x40, func(f PrinterErrorFlags) bool { return f.NoPaper }},
			{"LowToner_0x20", 0x20, func(f PrinterErrorFlags) bool { return f.LowToner }},
			{"NoToner_0x10", 0x10, func(f PrinterErrorFlags) bool { return f.NoToner }},
			{"DoorOpen_0x08", 0x08, func(f PrinterErrorFlags) bool { return f.DoorOpen }},
			{"Jammed_0x04", 0x04, func(f PrinterErrorFlags) bool { return f.Jammed }},
			{"Offline_0x02", 0x02, func(f PrinterErrorFlags) bool { return f.Offline }},
			{"ServiceRequested_0x01", 0x01, func(f PrinterErrorFlags) bool { return f.ServiceRequested }},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				flags := DecodeErrorBitmask([]byte{tc.b0})
				if !tc.verify(flags) {
					t.Errorf("expected flag for 0x%02X to be true, got %+v", tc.b0, flags)
				}
			})
		}
	})

	t.Run("individual_byte1_bits", func(t *testing.T) {
		tests := []struct {
			name   string
			b1     byte
			verify func(PrinterErrorFlags) bool
		}{
			{"InputTrayMissing_0x80", 0x80, func(f PrinterErrorFlags) bool { return f.InputTrayMissing }},
			{"OutputTrayMissing_0x40", 0x40, func(f PrinterErrorFlags) bool { return f.OutputTrayMissing }},
			{"MarkerSupplyMissing_0x20", 0x20, func(f PrinterErrorFlags) bool { return f.MarkerSupplyMissing }},
			{"OutputNearFull_0x10", 0x10, func(f PrinterErrorFlags) bool { return f.OutputNearFull }},
			{"OutputFull_0x08", 0x08, func(f PrinterErrorFlags) bool { return f.OutputFull }},
			{"InputTrayEmpty_0x04", 0x04, func(f PrinterErrorFlags) bool { return f.InputTrayEmpty }},
			{"OverduePreventMaint_0x02", 0x02, func(f PrinterErrorFlags) bool { return f.OverduePreventMaint }},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				flags := DecodeErrorBitmask([]byte{0x00, tc.b1})
				if !tc.verify(flags) {
					t.Errorf("expected flag for byte1 0x%02X to be true, got %+v", tc.b1, flags)
				}
			})
		}
	})

	t.Run("combined_bits_and_immutability", func(t *testing.T) {
		orig := []byte{0x40 | 0x04, 0x08 | 0x04} // NoPaper + Jammed, OutputFull + InputTrayEmpty
		copyBuf := append([]byte(nil), orig...)

		flags := DecodeErrorBitmask(orig)
		if !flags.NoPaper || !flags.Jammed || !flags.OutputFull || !flags.InputTrayEmpty {
			t.Errorf("missing expected combined flags: %+v", flags)
		}
		if flags.DoorOpen || flags.LowPaper || flags.Offline {
			t.Errorf("unexpected extra flags set: %+v", flags)
		}

		_ = DecodeErrorBitmask(orig)
		if !bytes.Equal(orig, copyBuf) {
			t.Errorf("input slice modified: got %v, want %v", orig, copyBuf)
		}
	})
}

func TestDynamicSNMP_StatusAndErrorBitmaskMapping(t *testing.T) {
	now := time.Now().UTC()

	statusTests := []struct {
		statusVal int
		wantState string
		wantFault bool
	}{
		{3, "IDLE", false},
		{4, "PRINTING", false},
		{5, "WARMING_UP", false},
		{1, "UNKNOWN", false},
		{2, "UNKNOWN", false},
	}

	for _, tc := range statusTests {
		probe := &SNMPProbeResult{
			TargetHost: "10.0.0.10",
			Printers: []SNMPPrinterDevice{{
				Index:        "1",
				DeviceStatus: 2,
				Status:       tc.statusVal,
			}},
		}
		obs := EvaluateSNMPProbe(probe, 200, now)
		if obs.Health.State != tc.wantState {
			t.Errorf("statusVal=%d State = %s, want %s", tc.statusVal, obs.Health.State, tc.wantState)
		}
		if obs.HasBlockingFault != tc.wantFault {
			t.Errorf("statusVal=%d HasBlockingFault = %v, want %v", tc.statusVal, obs.HasBlockingFault, tc.wantFault)
		}
	}

	bitTests := []struct {
		name         string
		errorBytes   []byte
		wantTray     string
		wantCover    bool
		wantJam      bool
		wantBinFull  bool
		wantFault    bool
	}{
		{"low_paper", []byte{0x80}, "LOW", false, false, false, false},
		{"no_paper", []byte{0x40}, "EMPTY", false, false, false, true},
		{"low_toner", []byte{0x20}, "OK", false, false, false, false},
		{"no_toner", []byte{0x10}, "OK", false, false, false, true},
		{"door_open", []byte{0x08}, "OK", true, false, false, true},
		{"paper_jam", []byte{0x04}, "OK", false, true, false, true},
		{"service_requested", []byte{0x01}, "OK", false, false, false, true},
		{"output_bin_full", []byte{0x00, 0x08}, "OK", false, false, true, true},
		{"input_tray_empty_byte1", []byte{0x00, 0x04}, "EMPTY", false, false, false, true},
		{"door_open_and_jam", []byte{0x08 | 0x04}, "OK", true, true, false, true},
	}

	for _, tc := range bitTests {
		t.Run(tc.name, func(t *testing.T) {
			flags := DecodeErrorBitmask(tc.errorBytes)
			probe := &SNMPProbeResult{
				TargetHost: "10.0.0.10",
				Printers: []SNMPPrinterDevice{{
					Index:        "1",
					DeviceStatus: 2,
					Status:       3,
					ErrorRaw:     tc.errorBytes,
					ErrorFlags:   flags,
				}},
			}
			obs := EvaluateSNMPProbe(probe, 250, now)
			if obs.Health.PaperTrayStatus != tc.wantTray {
				t.Errorf("PaperTrayStatus = %s, want %s", obs.Health.PaperTrayStatus, tc.wantTray)
			}
			if obs.Health.CoverOpen != tc.wantCover {
				t.Errorf("CoverOpen = %v, want %v", obs.Health.CoverOpen, tc.wantCover)
			}
			if obs.Health.PaperJam != tc.wantJam {
				t.Errorf("PaperJam = %v, want %v", obs.Health.PaperJam, tc.wantJam)
			}
			if obs.Health.OutputBinFull != tc.wantBinFull {
				t.Errorf("OutputBinFull = %v, want %v", obs.Health.OutputBinFull, tc.wantBinFull)
			}
			if obs.HasBlockingFault != tc.wantFault {
				t.Errorf("HasBlockingFault = %v, want %v", obs.HasBlockingFault, tc.wantFault)
			}
		})
	}
}

func TestDynamicSNMP_MonitorErrorAndSessionLifecycle(t *testing.T) {
	t.Run("uses_estimated_sheets", func(t *testing.T) {
		session := &fakeSnmpSession{
			walkFunc: func(rootOid string, walkFn gosnmp.WalkFunc) error {
				if normalizeOID(rootOid) == oidSubtreeHRPrinter {
					return walkFn(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.25.3.5.1.1.2", Type: gosnmp.Integer, Value: 3})
				}
				return nil
			},
		}
		mon := NewSNMPMonitor(DefaultSNMPConfig(), func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return session, nil
		})
		obs := mon.Poll(context.Background(), 173)
		if !obs.Available || obs.Health.EstimatedSheets != 173 {
			t.Fatalf("expected Available=true and EstimatedSheets=173, got %+v", obs)
		}
		if !session.closed {
			t.Fatal("expected SNMP session closed after Poll")
		}
	})

	t.Run("connect_and_context_failures_fail_closed", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		monCancel := NewSNMPMonitor(DefaultSNMPConfig(), func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return &fakeSnmpSession{}, nil
		})
		obsCancel := monCancel.Poll(cancelCtx, 180)
		if obsCancel.Available || obsCancel.Health.Online || obsCancel.Health.State != "OFFLINE" {
			t.Fatalf("expected unavailable OFFLINE on canceled context, got %+v", obsCancel)
		}

		monErr := NewSNMPMonitor(DefaultSNMPConfig(), func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return nil, errors.New("connection refused")
		})
		obsErr := monErr.Poll(context.Background(), 210)
		if obsErr.Available || obsErr.Health.Online || !strings.Contains(obsErr.Error, "connection refused") {
			t.Fatalf("expected unavailable connection refused error, got %+v", obsErr)
		}
	})

	t.Run("closes_session_even_on_corrupt_get", func(t *testing.T) {
		session := &fakeSnmpSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return nil, errors.New("corrupt response")
			},
		}
		mon := NewSNMPMonitor(DefaultSNMPConfig(), func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return session, nil
		})
		obs := mon.Poll(context.Background(), 250)
		if obs.Available {
			t.Fatalf("expected Available=false on empty/corrupt SNMP responses")
		}
		if !session.closed {
			t.Error("expected session closed after failure")
		}
	})
}

func TestPollHardwareTelemetry(t *testing.T) {
	ctx := context.Background()

	t.Run("valid_telemetry_with_jam_and_lifecount", func(t *testing.T) {
		packet := &gosnmp.SnmpPacket{
			Error: gosnmp.NoError,
			Variables: []gosnmp.SnmpPDU{
				{
					Name:  oidHrPrinterStatus,
					Type:  gosnmp.Integer,
					Value: 3, // IDLE
				},
				{
					Name:  oidHrPrinterDetectedErrorState,
					Type:  gosnmp.OctetString,
					Value: []byte{0x04}, // JAMMED (bit 5)
				},
				{
					Name:  oidPrtMarkerLifeCount,
					Type:  gosnmp.Counter32,
					Value: uint32(68544),
				},
			},
		}

		session := &fakeSnmpSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				return packet, nil
			},
		}
		factory := func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return session, nil
		}

		telem := pollHardwareTelemetryWithFactory(ctx, SNMPConfig{}, factory)
		if !telem.Online {
			t.Fatal("expected telemetry Online = true")
		}
		if telem.StatusState != "IDLE" {
			t.Errorf("StatusState = %q, want IDLE", telem.StatusState)
		}
		if !telem.HasFlags {
			t.Fatal("expected HasFlags = true")
		}
		if !telem.Flags.Jammed {
			t.Errorf("expected Flags.Jammed = true for 0x04")
		}
		if !telem.HasLife {
			t.Fatal("expected HasLife = true")
		}
		if telem.LifeCount != 68544 {
			t.Errorf("LifeCount = %d, want 68544", telem.LifeCount)
		}
		if !session.closed {
			t.Error("expected session to be closed")
		}
	})

	t.Run("fallback_when_lifecount_unsupported", func(t *testing.T) {
		fallbackPacket := &gosnmp.SnmpPacket{
			Error: gosnmp.NoError,
			Variables: []gosnmp.SnmpPDU{
				{
					Name:  oidHrPrinterStatus,
					Type:  gosnmp.Integer,
					Value: 4, // PRINTING
				},
				{
					Name:  oidHrPrinterDetectedErrorState,
					Type:  gosnmp.OctetString,
					Value: []byte{0x08}, // DoorOpen (bit 4, 0x08)
				},
			},
		}

		callCount := 0
		session := &fakeSnmpSession{
			getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
				callCount++
				if len(oids) == 3 {
					return nil, errors.New("no such name")
				}
				return fallbackPacket, nil
			},
		}
		factory := func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return session, nil
		}

		telem := pollHardwareTelemetryWithFactory(ctx, SNMPConfig{}, factory)
		if !telem.Online {
			t.Fatal("expected telemetry Online = true via fallback")
		}
		if telem.StatusState != "PRINTING" {
			t.Errorf("StatusState = %q, want PRINTING", telem.StatusState)
		}
		if !telem.Flags.DoorOpen {
			t.Errorf("expected Flags.DoorOpen = true")
		}
		if telem.HasLife {
			t.Error("expected HasLife = false on fallback")
		}
		if callCount != 2 {
			t.Errorf("expected 2 calls (initial + fallback), got %d", callCount)
		}
	})

	t.Run("canceled_context", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()

		telem := pollHardwareTelemetryWithFactory(cancelCtx, SNMPConfig{}, func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return &fakeSnmpSession{}, nil
		})
		if telem.Online {
			t.Error("expected Online = false for canceled context")
		}
	})

	t.Run("connection_failure", func(t *testing.T) {
		telem := pollHardwareTelemetryWithFactory(ctx, SNMPConfig{}, func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
			return nil, errors.New("network unreachable")
		})
		if telem.Online {
			t.Error("expected Online = false on connection error")
		}
	})
}
