package printer

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

const (
	oidSysObjectID = "1.3.6.1.2.1.1.2.0"
	oidSysUpTime   = "1.3.6.1.2.1.1.3.0"
)

// emulatedSNMPState holds the mutable telemetry state for the in-process test responder.
type emulatedSNMPState struct {
	SysDescr        string   `json:"sys_descr"`
	HrPrinterStatus int      `json:"hr_printer_status"`
	ErrorByte0      byte     `json:"error_byte_0"`
	ErrorByte1      byte     `json:"error_byte_1"`
	MarkerLifeCount uint32   `json:"marker_life_count"`
	TonerLevel      int      `json:"toner_level"`
	TonerMax        int      `json:"toner_max"`
	DrumLevel       int      `json:"drum_level"`
	DrumMax         int      `json:"drum_max"`
	OmittedOIDs     []string `json:"omitted_oids,omitempty"`
}

func defaultTestState() emulatedSNMPState {
	return emulatedSNMPState{
		SysDescr:        "Xerox WorkCentre 3025 (Test Emulator)",
		HrPrinterStatus: 3, // idle
		ErrorByte0:      0x00,
		ErrorByte1:      0x00,
		MarkerLifeCount: 1000,
		TonerLevel:      85,
		TonerMax:        100,
		DrumLevel:       95,
		DrumMax:         100,
		OmittedOIDs:     nil,
	}
}

// testSNMPResponder runs an in-process RFC-compatible SNMPv2c responder on 127.0.0.1.
type testSNMPResponder struct {
	mu        sync.RWMutex
	conn      net.PacketConn
	port      int
	community string
	state     emulatedSNMPState
	stateFile string
	stopCh    chan struct{}
	stopped   sync.WaitGroup
}

func newTestSNMPResponder(t *testing.T, initialPort int, stateFile string) *testSNMPResponder {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", initialPort)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("failed to bind test SNMP UDP port %s: %v", addr, err)
	}

	udpAddr := conn.LocalAddr().(*net.UDPAddr)
	r := &testSNMPResponder{
		conn:      conn,
		port:      udpAddr.Port,
		community: "public",
		state:     defaultTestState(),
		stateFile: stateFile,
		stopCh:    make(chan struct{}),
	}

	if stateFile != "" {
		if data, err := os.ReadFile(stateFile); err == nil {
			_ = json.Unmarshal(data, &r.state)
		} else {
			r.saveStateLocked()
		}
	}

	r.stopped.Add(1)
	go r.serve()
	return r
}

func (r *testSNMPResponder) Port() int {
	return r.port
}

func (r *testSNMPResponder) UpdateState(fn func(*emulatedSNMPState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.state)
	if r.stateFile != "" {
		r.saveStateLocked()
	}
}

func (r *testSNMPResponder) saveStateLocked() {
	data, err := json.MarshalIndent(r.state, "", "  ")
	if err == nil {
		_ = os.WriteFile(r.stateFile, data, 0644)
	}
}

func (r *testSNMPResponder) Close() {
	close(r.stopCh)
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.stopped.Wait()
}

func (r *testSNMPResponder) serve() {
	defer r.stopped.Done()
	buf := make([]byte, 4096)
	g := &gosnmp.GoSNMP{Version: gosnmp.Version2c}

	for {
		select {
		case <-r.stopCh:
			return
		default:
		}

		_ = r.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, remoteAddr, err := r.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-r.stopCh:
				return
			default:
				continue
			}
		}

		packet, err := g.SnmpDecodePacket(buf[:n])
		if err != nil {
			continue
		}

		r.mu.RLock()
		community := r.community
		state := r.state
		r.mu.RUnlock()

		if packet.Community != community || packet.PDUType != gosnmp.GetRequest {
			continue
		}

		resp := &gosnmp.SnmpPacket{
			Version:   packet.Version,
			Community: packet.Community,
			PDUType:   gosnmp.GetResponse,
			RequestID: packet.RequestID,
			Error:     gosnmp.NoError,
			Variables: make([]gosnmp.SnmpPDU, 0, len(packet.Variables)),
		}

		for _, pdu := range packet.Variables {
			normOID := strings.TrimPrefix(pdu.Name, ".")

			omitted := false
			for _, o := range state.OmittedOIDs {
				if strings.TrimPrefix(o, ".") == normOID {
					omitted = true
					break
				}
			}

			if omitted {
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.NoSuchInstance,
					Value: nil,
				})
				continue
			}

			switch normOID {
			case normalizeOID(oidSysDescr):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.OctetString,
					Value: []byte(state.SysDescr),
				})
			case normalizeOID(oidHrPrinterStatus):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.HrPrinterStatus,
				})
			case normalizeOID(oidHrPrinterDetectedErrorState):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.OctetString,
					Value: []byte{state.ErrorByte0, state.ErrorByte1},
				})
			case normalizeOID(oidPrtMarkerLifeCount):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Counter32,
					Value: state.MarkerLifeCount,
				})
			case normalizeOID(oidPrtMarkerSuppliesLevel1):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.TonerLevel,
				})
			case normalizeOID(oidPrtMarkerSuppliesMax1):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.TonerMax,
				})
			case normalizeOID(oidPrtMarkerSuppliesLevel2):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.DrumLevel,
				})
			case normalizeOID(oidPrtMarkerSuppliesMax2):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.DrumMax,
				})
			default:
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.NoSuchInstance,
					Value: nil,
				})
			}
		}

		outBytes, err := resp.MarshalMsg()
		if err != nil {
			continue
		}
		_, _ = r.conn.WriteTo(outBytes, remoteAddr)
	}
}

// -----------------------------------------------------------------------------
// Test 1: Healthy State
// -----------------------------------------------------------------------------
func TestSNMPEmulator_HealthyState(t *testing.T) {
	responder := newTestSNMPResponder(t, 0, "")
	defer responder.Close()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      responder.Port(),
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}

	ctx := context.Background()

	// 1. PollHealthConfig
	health := PollHealthConfig(ctx, cfg, 500)
	if !health.Online {
		t.Fatalf("expected Online=true, got false (error: %s)", health.ErrorMessage)
	}
	if health.State != "IDLE" {
		t.Errorf("expected State='IDLE', got %q", health.State)
	}
	if health.PaperTrayStatus != "OK" {
		t.Errorf("expected PaperTrayStatus='OK', got %q", health.PaperTrayStatus)
	}
	if health.CoverOpen {
		t.Errorf("expected CoverOpen=false, got true")
	}
	if health.PaperJam {
		t.Errorf("expected PaperJam=false, got true")
	}
	if health.OutputBinFull {
		t.Errorf("expected OutputBinFull=false, got true")
	}
	if health.TonerLevelPercent != 85 {
		t.Errorf("expected TonerLevelPercent=85, got %d", health.TonerLevelPercent)
	}
	if health.DrumLifePercent != 95 {
		t.Errorf("expected DrumLifePercent=95, got %d", health.DrumLifePercent)
	}
	if health.ErrorMessage != "" {
		t.Errorf("expected empty ErrorMessage, got %q", health.ErrorMessage)
	}
	if health.EstimatedSheets != 500 {
		t.Errorf("expected EstimatedSheets=500, got %d", health.EstimatedSheets)
	}

	// 2. PollHardwareTelemetry
	telem := PollHardwareTelemetry(ctx, cfg)
	if !telem.Online {
		t.Fatalf("expected telem.Online=true, got false (error: %s)", telem.ErrorMsg)
	}
	if telem.StatusState != "IDLE" {
		t.Errorf("expected telem.StatusState='IDLE', got %q", telem.StatusState)
	}
	if !telem.HasLife || telem.LifeCount != 1000 {
		t.Errorf("expected HasLife=true and LifeCount=1000, got HasLife=%v, LifeCount=%d", telem.HasLife, telem.LifeCount)
	}
	if !telem.HasFlags {
		t.Errorf("expected HasFlags=true, got false")
	}

	// 3. PollMarkerLifeCount
	life := PollMarkerLifeCount(ctx, cfg)
	if life != 1000 {
		t.Errorf("expected PollMarkerLifeCount=1000, got %d", life)
	}
}

// -----------------------------------------------------------------------------
// Test 2: Error-Bit Decoding (RFC 2790 Byte 0 and Byte 1)
// -----------------------------------------------------------------------------
func TestSNMPEmulator_ErrorBitDecoding(t *testing.T) {
	responder := newTestSNMPResponder(t, 0, "")
	defer responder.Close()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      responder.Port(),
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}

	ctx := context.Background()

	cases := []struct {
		name            string
		byte0           byte
		byte1           byte
		wantOnline      bool
		wantState       string
		wantPaperTray   string
		wantCoverOpen   bool
		wantPaperJam    bool
		wantOutputFull  bool
		errContains     []string
	}{
		{
			name:          "DoorOpen (0x08 in byte 0)",
			byte0:         0x08,
			byte1:         0x00,
			wantOnline:    true,
			wantState:     "ERROR",
			wantPaperTray: "OK",
			wantCoverOpen: true,
			errContains:   []string{"door open"},
		},
		{
			name:          "PaperJam (0x04 in byte 0)",
			byte0:         0x04,
			byte1:         0x00,
			wantOnline:    true,
			wantState:     "ERROR",
			wantPaperTray: "OK",
			wantPaperJam:  true,
			errContains:   []string{"paper jam"},
		},
		{
			name:          "NoPaper (0x40 in byte 0)",
			byte0:         0x40,
			byte1:         0x00,
			wantOnline:    true,
			wantState:     "ERROR",
			wantPaperTray: "EMPTY",
			errContains:   []string{"no paper"},
		},
		{
			name:          "LowPaper (0x80 in byte 0)",
			byte0:         0x80,
			byte1:         0x00,
			wantOnline:    true,
			wantState:     "IDLE", // LowPaper warning alone does not force state to ERROR
			wantPaperTray: "LOW",
			errContains:   []string{"low paper"},
		},
		{
			name:          "PrinterOffline (0x02 in byte 0)",
			byte0:         0x02,
			byte1:         0x00,
			wantOnline:    false,
			wantState:     "OFFLINE",
			wantPaperTray: "OK",
			errContains:   []string{"printer offline"},
		},
		{
			name:          "ServiceRequested (0x01 in byte 0)",
			byte0:         0x01,
			byte1:         0x00,
			wantOnline:    true,
			wantState:     "ERROR",
			wantPaperTray: "OK",
			errContains:   []string{"service requested"},
		},
		{
			name:           "OutputFull (0x08 in byte 1)",
			byte0:          0x00,
			byte1:          0x08,
			wantOnline:     true,
			wantState:      "ERROR",
			wantPaperTray:  "OK",
			wantOutputFull: true,
			errContains:    []string{"output bin full"},
		},
		{
			name:          "InputTrayEmpty (0x04 in byte 1)",
			byte0:         0x00,
			byte1:         0x04,
			wantOnline:    true,
			wantState:     "ERROR",
			wantPaperTray: "EMPTY",
			errContains:   []string{"input tray empty"},
		},
		{
			name:          "InputTrayMissing (0x80 in byte 1)",
			byte0:         0x00,
			byte1:         0x80,
			wantOnline:    true,
			wantState:     "IDLE", // InputTrayMissing sets PaperTrayStatus=EMPTY
			wantPaperTray: "EMPTY",
			errContains:   []string{"input tray missing"},
		},
		{
			name:          "OverdueMaintenance (0x02 in byte 1)",
			byte0:         0x00,
			byte1:         0x02,
			wantOnline:    true,
			wantState:     "IDLE",
			wantPaperTray: "OK",
			errContains:   []string{"maintenance overdue"},
		},
		{
			name:           "MultiFault: Jam + DoorOpen + OutputFull (byte0=0x0C, byte1=0x08)",
			byte0:          0x0C,
			byte1:          0x08,
			wantOnline:     true,
			wantState:      "ERROR",
			wantPaperTray:  "OK",
			wantCoverOpen:  true,
			wantPaperJam:   true,
			wantOutputFull: true,
			errContains:    []string{"door open", "paper jam", "output bin full"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			responder.UpdateState(func(s *emulatedSNMPState) {
				s.HrPrinterStatus = 3
				s.ErrorByte0 = tc.byte0
				s.ErrorByte1 = tc.byte1
			})

			health := PollHealthConfig(ctx, cfg, 100)
			if health.Online != tc.wantOnline {
				t.Errorf("Online: got %v, want %v", health.Online, tc.wantOnline)
			}
			if health.State != tc.wantState {
				t.Errorf("State: got %q, want %q", health.State, tc.wantState)
			}
			if health.PaperTrayStatus != tc.wantPaperTray {
				t.Errorf("PaperTrayStatus: got %q, want %q", health.PaperTrayStatus, tc.wantPaperTray)
			}
			if tc.wantCoverOpen && !health.CoverOpen {
				t.Errorf("expected CoverOpen=true")
			}
			if tc.wantPaperJam && !health.PaperJam {
				t.Errorf("expected PaperJam=true")
			}
			if tc.wantOutputFull && !health.OutputBinFull {
				t.Errorf("expected OutputBinFull=true")
			}
			for _, expectedText := range tc.errContains {
				if !strings.Contains(health.ErrorMessage, expectedText) {
					t.Errorf("ErrorMessage %q does not contain expected substring %q", health.ErrorMessage, expectedText)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Test 3: Toner and Drum Percentages
// -----------------------------------------------------------------------------
func TestSNMPEmulator_TonerAndDrumPercentages(t *testing.T) {
	responder := newTestSNMPResponder(t, 0, "")
	defer responder.Close()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      responder.Port(),
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}

	ctx := context.Background()

	cases := []struct {
		name        string
		tonerLevel  int
		tonerMax    int
		drumLevel   int
		drumMax     int
		wantToner   int
		wantDrum    int
	}{
		{
			name:       "100% full consumables",
			tonerLevel: 100,
			tonerMax:   100,
			drumLevel:  100,
			drumMax:    100,
			wantToner:  100,
			wantDrum:   100,
		},
		{
			name:       "Partial consumables",
			tonerLevel: 35,
			tonerMax:   100,
			drumLevel:  70,
			drumMax:    100,
			wantToner:  35,
			wantDrum:   70,
		},
		{
			name:       "Different max units scale",
			tonerLevel: 2500,
			tonerMax:   5000,
			drumLevel:  8000,
			drumMax:    10000,
			wantToner:  50,
			wantDrum:   80,
		},
		{
			name:       "RFC 3805 invalid/unknown level (-2)",
			tonerLevel: -2,
			tonerMax:   100,
			drumLevel:  50,
			drumMax:    100,
			wantToner:  0,
			wantDrum:   50,
		},
		{
			name:       "RFC 3805 non-positive max capacity (0)",
			tonerLevel: 50,
			tonerMax:   0,
			drumLevel:  50,
			drumMax:    100,
			wantToner:  0,
			wantDrum:   50,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			responder.UpdateState(func(s *emulatedSNMPState) {
				s.TonerLevel = tc.tonerLevel
				s.TonerMax = tc.tonerMax
				s.DrumLevel = tc.drumLevel
				s.DrumMax = tc.drumMax
			})

			health := PollHealthConfig(ctx, cfg, 0)
			if health.TonerLevelPercent != tc.wantToner {
				t.Errorf("TonerLevelPercent: got %d, want %d", health.TonerLevelPercent, tc.wantToner)
			}
			if health.DrumLifePercent != tc.wantDrum {
				t.Errorf("DrumLifePercent: got %d, want %d", health.DrumLifePercent, tc.wantDrum)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Test 4: Missing OIDs
// -----------------------------------------------------------------------------
func TestSNMPEmulator_MissingOIDs(t *testing.T) {
	responder := newTestSNMPResponder(t, 0, "")
	defer responder.Close()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      responder.Port(),
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}

	ctx := context.Background()

	t.Run("Omit marker life count returns -1 odometer", func(t *testing.T) {
		responder.UpdateState(func(s *emulatedSNMPState) {
			*s = defaultTestState()
			s.OmittedOIDs = []string{oidPrtMarkerLifeCount}
		})

		count := PollMarkerLifeCount(ctx, cfg)
		if count != -1 {
			t.Errorf("expected PollMarkerLifeCount=-1 for omitted OID, got %d", count)
		}

		telem := PollHardwareTelemetry(ctx, cfg)
		if telem.HasLife {
			t.Errorf("expected HasLife=false, got true")
		}
	})

	t.Run("Omit all metrics returns UNKNOWN state", func(t *testing.T) {
		responder.UpdateState(func(s *emulatedSNMPState) {
			*s = defaultTestState()
			s.OmittedOIDs = []string{
				oidHrPrinterStatus,
				oidHrPrinterDetectedErrorState,
				oidPrtMarkerLifeCount,
				oidPrtMarkerSuppliesLevel1,
				oidPrtMarkerSuppliesMax1,
				oidPrtMarkerSuppliesLevel2,
				oidPrtMarkerSuppliesMax2,
			}
		})

		health := PollHealthConfig(ctx, cfg, 200)
		if !health.Online {
			t.Errorf("expected Online=true when agent responds, got false")
		}
		if health.State != "UNKNOWN" {
			t.Errorf("expected State='UNKNOWN', got %q", health.State)
		}
		if health.PaperTrayStatus != "UNKNOWN" {
			t.Errorf("expected PaperTrayStatus='UNKNOWN', got %q", health.PaperTrayStatus)
		}
		if !strings.Contains(health.ErrorMessage, "no supported printer metrics") {
			t.Errorf("expected ErrorMessage to mention missing metrics, got %q", health.ErrorMessage)
		}
	})
}

// -----------------------------------------------------------------------------
// Test 5: Timeouts and Cancellation
// -----------------------------------------------------------------------------
func TestSNMPEmulator_TimeoutsAndCancellation(t *testing.T) {
	responder := newTestSNMPResponder(t, 0, "")
	defer responder.Close()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      responder.Port(),
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   0,
	}

	t.Run("Pre-canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		health := PollHealthConfig(ctx, cfg, 100)
		if health.Online {
			t.Errorf("expected Online=false for canceled context, got true")
		}
		if health.State != "OFFLINE" {
			t.Errorf("expected State='OFFLINE', got %q", health.State)
		}
		if !strings.Contains(health.ErrorMessage, "context canceled") {
			t.Errorf("expected ErrorMessage to mention 'context canceled', got %q", health.ErrorMessage)
		}

		telem := PollHardwareTelemetry(ctx, cfg)
		if telem.Online {
			t.Errorf("expected telem.Online=false for canceled context")
		}
	})

	t.Run("Unreachable port timeout", func(t *testing.T) {
		// Use an unbound loopback port with short timeout
		unreachableCfg := SNMPConfig{
			Target:    "127.0.0.1",
			Port:      19998,
			Community: "public",
			Timeout:   100 * time.Millisecond,
			Retries:   0,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		health := PollHealthConfig(ctx, unreachableCfg, 50)
		if health.Online {
			t.Errorf("expected Online=false for unreachable port, got true")
		}
		if health.State != "OFFLINE" {
			t.Errorf("expected State='OFFLINE', got %q", health.State)
		}
		if !strings.Contains(health.ErrorMessage, "SNMP GET request failed") {
			t.Errorf("expected ErrorMessage to mention request failure, got %q", health.ErrorMessage)
		}
	})
}

// -----------------------------------------------------------------------------
// Test 6: Restart Persistence
// -----------------------------------------------------------------------------
func TestSNMPEmulator_RestartPersistence(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "snmp_state.json")

	// 1. Initial responder instance
	responder1 := newTestSNMPResponder(t, 0, stateFile)
	port := responder1.Port()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}

	// Modify state in instance 1
	responder1.UpdateState(func(s *emulatedSNMPState) {
		s.MarkerLifeCount = 8888
		s.TonerLevel = 42
		s.HrPrinterStatus = 4 // printing
	})

	ctx := context.Background()
	life1 := PollMarkerLifeCount(ctx, cfg)
	if life1 != 8888 {
		t.Fatalf("expected PollMarkerLifeCount=8888, got %d", life1)
	}

	// Stop responder 1
	responder1.Close()

	// 2. Restart responder on the EXACT same port with the saved state file
	responder2 := newTestSNMPResponder(t, port, stateFile)
	defer responder2.Close()

	// Verify state persisted across restart
	life2 := PollMarkerLifeCount(ctx, cfg)
	if life2 != 8888 {
		t.Errorf("expected PollMarkerLifeCount to persist as 8888, got %d", life2)
	}

	telem2 := PollHardwareTelemetry(ctx, cfg)
	if telem2.StatusState != "PRINTING" {
		t.Errorf("expected StatusState='PRINTING' to persist, got %q", telem2.StatusState)
	}

	health2 := PollHealthConfig(ctx, cfg, 0)
	if health2.TonerLevelPercent != 42 {
		t.Errorf("expected TonerLevelPercent=42 to persist, got %d", health2.TonerLevelPercent)
	}
}

// -----------------------------------------------------------------------------
// Test 7: Deterministic Counter Updates
// -----------------------------------------------------------------------------
func TestSNMPEmulator_DeterministicCounterUpdates(t *testing.T) {
	responder := newTestSNMPResponder(t, 0, "")
	defer responder.Close()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      responder.Port(),
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}

	ctx := context.Background()

	for step := 1; step <= 5; step++ {
		increment := uint32(step * 10)
		responder.UpdateState(func(s *emulatedSNMPState) {
			s.MarkerLifeCount += increment
		})

		expected := int64(1000 + (step * (step + 1) / 2 * 10))
		count := PollMarkerLifeCount(ctx, cfg)
		if count != expected {
			t.Errorf("step %d: expected counter %d, got %d", step, expected, count)
		}
	}
}

// -----------------------------------------------------------------------------
// Test 8: Integration with actual D2 snmp_responder binary process
// -----------------------------------------------------------------------------
func buildOrFindSNMPResponder(t *testing.T) string {
	t.Helper()
	relPaths := []string{
		"../../../tools/printer-emulator",
		"../../tools/printer-emulator",
		"./tools/printer-emulator",
	}
	var emuDir string
	for _, p := range relPaths {
		if fi, err := os.Stat(filepath.Join(p, "snmp_responder.go")); err == nil && !fi.IsDir() {
			emuDir = p
			break
		}
	}
	if emuDir == "" {
		t.Fatalf("could not locate tools/printer-emulator directory")
	}

	binPath := filepath.Join(emuDir, "snmp_responder")
	srcPath := filepath.Join(emuDir, "snmp_responder.go")

	binInfo, binErr := os.Stat(binPath)
	srcInfo, srcErr := os.Stat(srcPath)
	if srcErr != nil {
		t.Fatalf("failed to stat snmp_responder.go: %v", srcErr)
	}

	if binErr != nil || srcInfo.ModTime().After(binInfo.ModTime()) {
		cmd := exec.Command("go", "build", "-o", "snmp_responder", "snmp_responder.go")
		cmd.Dir = emuDir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to compile snmp_responder: %v (output: %s)", err, string(out))
		}
	}

	absBin, err := filepath.Abs(binPath)
	if err != nil {
		t.Fatalf("failed to resolve absolute path for binary: %v", err)
	}
	return absBin
}

func TestSNMPEmulator_ActualBinaryProcess(t *testing.T) {
	binPath := buildOrFindSNMPResponder(t)

	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "snmp_state.json")
	pidFile := filepath.Join(tempDir, "snmp_responder.pid")
	portFile := filepath.Join(tempDir, "snmp_responder.port")

	// Start snmp_responder serve on temporary loopback UDP port (dynamic port 0)
	cmd := exec.Command(binPath, "serve",
		"-host", "127.0.0.1",
		"-port", "0",
		"-community", "public",
		"-state", stateFile,
		"-pid", pidFile,
		"-port-file", portFile,
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start snmp_responder binary process: %v", err)
	}

	// Guarantee cleanup of the OS process
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	}()

	// Wait for portFile to be written by the running daemon
	var boundPort int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile); err == nil && len(data) > 0 {
			pStr := strings.TrimSpace(string(data))
			if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
				boundPort = p
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if boundPort <= 0 {
		t.Fatalf("snmp_responder process did not write bound port within timeout")
	}

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      boundPort,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   1,
	}

	ctx := context.Background()

	// Wait for responder readiness
	ready := false
	for time.Now().Before(deadline) {
		if life := PollMarkerLifeCount(ctx, cfg); life >= 0 {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("snmp_responder on port %d was not ready within timeout", boundPort)
	}

	// 1. Verify healthy status through actual responder
	t.Run("ActualBinary_HealthyStatus", func(t *testing.T) {
		health := PollHealthConfig(ctx, cfg, 250)
		if !health.Online {
			t.Fatalf("expected Online=true, got false (error: %s)", health.ErrorMessage)
		}
		if health.State != "IDLE" {
			t.Errorf("expected State='IDLE', got %q", health.State)
		}
		if health.PaperTrayStatus != "OK" {
			t.Errorf("expected PaperTrayStatus='OK', got %q", health.PaperTrayStatus)
		}
		if health.PaperJam || health.CoverOpen || health.OutputBinFull {
			t.Errorf("expected no fault flags, got Jam=%v Cover=%v Output=%v", health.PaperJam, health.CoverOpen, health.OutputBinFull)
		}
		if health.TonerLevelPercent != 85 || health.DrumLifePercent != 95 {
			t.Errorf("expected Toner=85, Drum=95, got Toner=%d Drum=%d", health.TonerLevelPercent, health.DrumLifePercent)
		}

		telem := PollHardwareTelemetry(ctx, cfg)
		if !telem.Online || telem.StatusState != "IDLE" {
			t.Errorf("telem: Online=%v StatusState=%q", telem.Online, telem.StatusState)
		}
		if !telem.HasLife || telem.LifeCount != 1000 {
			t.Errorf("expected LifeCount=1000, got HasLife=%v LifeCount=%d", telem.HasLife, telem.LifeCount)
		}

		life := PollMarkerLifeCount(ctx, cfg)
		if life != 1000 {
			t.Errorf("expected PollMarkerLifeCount=1000, got %d", life)
		}
	})

	// 2. Verify jam bit update through actual responder
	t.Run("ActualBinary_JamBit", func(t *testing.T) {
		setCmd := exec.Command(binPath, "set", "-state", stateFile, "-set-error", "jammed")
		if out, err := setCmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to update state with snmp_responder set: %v (output: %s)", err, string(out))
		}

		time.Sleep(50 * time.Millisecond)

		health := PollHealthConfig(ctx, cfg, 250)
		if health.State != "ERROR" {
			t.Errorf("expected State='ERROR' after jam, got %q", health.State)
		}
		if !health.PaperJam {
			t.Errorf("expected PaperJam=true after jam bit set")
		}
		if !strings.Contains(health.ErrorMessage, "paper jam") {
			t.Errorf("expected ErrorMessage to contain 'paper jam', got %q", health.ErrorMessage)
		}

		telem := PollHardwareTelemetry(ctx, cfg)
		if !telem.Flags.Jammed {
			t.Errorf("expected telem.Flags.Jammed=true")
		}
	})

	// 3. Verify deterministic counter update through actual responder
	t.Run("ActualBinary_CounterUpdate", func(t *testing.T) {
		setCmd := exec.Command(binPath, "set", "-state", stateFile, "-counter-inc", "150")
		if out, err := setCmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to increment counter: %v (output: %s)", err, string(out))
		}

		time.Sleep(50 * time.Millisecond)

		life := PollMarkerLifeCount(ctx, cfg)
		if life != 1150 {
			t.Errorf("expected PollMarkerLifeCount=1150, got %d", life)
		}

		telem := PollHardwareTelemetry(ctx, cfg)
		if telem.LifeCount != 1150 {
			t.Errorf("expected telem.LifeCount=1150, got %d", telem.LifeCount)
		}
	})

	// 4. Verify unavailable / omitted OID through actual responder
	t.Run("ActualBinary_UnavailableOID", func(t *testing.T) {
		setCmd := exec.Command(binPath, "set", "-state", stateFile, "-omit-oid", oidPrtMarkerLifeCount)
		if out, err := setCmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to omit OID: %v (output: %s)", err, string(out))
		}

		time.Sleep(50 * time.Millisecond)

		life := PollMarkerLifeCount(ctx, cfg)
		if life != -1 {
			t.Errorf("expected PollMarkerLifeCount=-1 for omitted OID, got %d", life)
		}

		telem := PollHardwareTelemetry(ctx, cfg)
		if telem.HasLife {
			t.Errorf("expected telem.HasLife=false for omitted OID, got true")
		}
	})
}

// -----------------------------------------------------------------------------
// Helper to start the compiled snmp_responder daemon process for live UDP tests
// -----------------------------------------------------------------------------
func startTestSNMPProcess(t *testing.T) (string, int, string, func()) {
	t.Helper()
	binPath := buildOrFindSNMPResponder(t)
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "snmp_state.json")
	pidFile := filepath.Join(tempDir, "snmp_responder.pid")
	portFile := filepath.Join(tempDir, "snmp_responder.port")

	cmd := exec.Command(binPath, "serve",
		"-host", "127.0.0.1",
		"-port", "0",
		"-community", "public",
		"-state", stateFile,
		"-pid", pidFile,
		"-port-file", portFile,
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start snmp_responder process: %v", err)
	}

	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	}

	var boundPort int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile); err == nil && len(data) > 0 {
			pStr := strings.TrimSpace(string(data))
			if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
				boundPort = p
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if boundPort <= 0 {
		stop()
		t.Fatalf("snmp_responder process did not write bound port within timeout")
	}

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      boundPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   0,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ready := false
	for time.Now().Before(deadline) {
		sess, err := NewRealDynamicSNMPSession(ctx, cfg)
		if err == nil {
			if pkt, err := sess.Get([]string{oidSysDescr}); err == nil && pkt != nil && len(pkt.Variables) > 0 {
				_ = sess.Close()
				ready = true
				break
			}
			_ = sess.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		stop()
		t.Fatalf("snmp_responder on port %d was not ready within timeout", boundPort)
	}

	return binPath, boundPort, stateFile, stop
}

// -----------------------------------------------------------------------------
// Test 9: Dynamic Table Walk Discovery over real UDP
// -----------------------------------------------------------------------------
func TestSNMPEmulator_DynamicWalk_Discovery(t *testing.T) {
	_, port, _, stop := startTestSNMPProcess(t)
	defer stop()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}

	ctx := context.Background()
	session, err := NewRealDynamicSNMPSession(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create real dynamic session: %v", err)
	}
	defer session.Close()

	res, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed: %v", err)
	}

	if res.DiscoveryMode != "SNMP_TABLE_WALK" {
		t.Fatalf("expected DiscoveryMode='SNMP_TABLE_WALK', got %q", res.DiscoveryMode)
	}
	if len(res.Printers) != 1 {
		t.Fatalf("expected 1 printer, got %d", len(res.Printers))
	}
	if res.Printers[0].Index != "1" || res.Printers[0].Status != 3 {
		t.Errorf("unexpected printer: %+v", res.Printers[0])
	}
	if len(res.Covers) != 1 || res.Covers[0].Status != 4 {
		t.Errorf("unexpected cover: %+v", res.Covers)
	}
	if len(res.Markers) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(res.Markers))
	}
	marker := res.Markers[0]
	if marker.Index != "1.1" || marker.CounterUnit != 8 || !marker.IsPageCounter || marker.LifeCount != 1000 {
		t.Errorf("unexpected marker: %+v", marker)
	}
	if len(res.Supplies) != 2 {
		t.Fatalf("expected 2 supplies, got %d", len(res.Supplies))
	}
	if res.Supplies[0].Type != 3 || res.Supplies[0].Level != 85 {
		t.Errorf("unexpected toner supply: %+v", res.Supplies[0])
	}
	if res.Supplies[1].Type != 15 || res.Supplies[1].Level != 95 {
		t.Errorf("unexpected drum supply: %+v", res.Supplies[1])
	}
	if res.SysDescr == "" {
		t.Errorf("expected non-empty sysDescr")
	}
}

// -----------------------------------------------------------------------------
// Test 10: Alternate Indexes & Task 1 Fingerprint Discovery
// -----------------------------------------------------------------------------
func TestSNMPEmulator_AlternateIndexes_FingerprintDiscovery(t *testing.T) {
	binPath, port, stateFile, stop := startTestSNMPProcess(t)
	defer stop()

	// Configure alternate printer index 2, marker index 2.3, supply indexes 2.1 and 2.2
	setCmd := exec.Command(binPath, "set",
		"-state", stateFile,
		"-printer-index", "2",
		"-marker-index", "2.3",
		"-supply-index", "2.1",
		"-drum-index", "2.2",
	)
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to set alternate indexes: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	ctx := context.Background()

	// 1. Verify that fixed-index .1.1 queries fail on alternate index configuration
	life := PollMarkerLifeCount(ctx, cfg)
	if life != -1 {
		t.Errorf("expected PollMarkerLifeCount=-1 for fixed index .1.1 when device is at .2.3, got %d", life)
	}

	// 2. Verify that dynamic table walk discovers the alternate indexes
	session, err := NewRealDynamicSNMPSession(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create real dynamic session: %v", err)
	}
	defer session.Close()

	res, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed: %v", err)
	}

	if res.DiscoveryMode != "SNMP_TABLE_WALK" {
		t.Fatalf("expected DiscoveryMode='SNMP_TABLE_WALK', got %q", res.DiscoveryMode)
	}
	if len(res.Printers) != 1 || res.Printers[0].Index != "2" {
		t.Fatalf("expected discovered printer index '2', got %+v", res.Printers)
	}
	if len(res.Markers) != 1 || res.Markers[0].Index != "2.3" {
		t.Fatalf("expected discovered marker index '2.3', got %+v", res.Markers)
	}
	if res.Markers[0].LifeCount != 1000 || !res.Markers[0].IsPageCounter {
		t.Errorf("unexpected marker: %+v", res.Markers[0])
	}
	if len(res.Supplies) != 2 || res.Supplies[0].Index != "2.1" || res.Supplies[1].Index != "2.2" {
		t.Fatalf("expected supplies at 2.1 and 2.2, got %+v", res.Supplies)
	}

	// 3. Verify that Task 1 RunFingerprint discovers alternate indexes dynamically over real UDP
	realFactory := func(snmpCfg SNMPConfig) (DynamicSNMPSession, error) {
		return NewRealDynamicSNMPSession(context.Background(), snmpCfg)
	}
	fpCfg := FingerprintConfig{
		SNMPHost:      "127.0.0.1",
		SNMPPort:      port,
		SNMPCommunity: "public",
		SNMPTimeout:   2 * time.Second,
		Redact:        false,
	}

	rep, err := RunFingerprint(ctx, nil, realFactory, fpCfg)
	if err != nil {
		t.Fatalf("RunFingerprint failed: %v", err)
	}
	if rep.SNMP == nil || rep.SNMP.DiscoveryMode != "SNMP_TABLE_WALK" {
		t.Fatalf("expected SNMP_TABLE_WALK in report, got %+v", rep.SNMP)
	}
	if len(rep.SNMP.Markers) != 1 || rep.SNMP.Markers[0].Index != "2.3" {
		t.Fatalf("expected fingerprint report to discover alternate marker index 2.3, got %+v", rep.SNMP.Markers)
	}
	if !rep.Telemetry.HasSNMPMarkerCounter || rep.Telemetry.SNMPMarkerLifeCount != 1000 {
		t.Errorf("expected HasSNMPMarkerCounter=true and lifeCount=1000, got %+v", rep.Telemetry)
	}
	if rep.Telemetry.SNMPMarkerCounterUnit != "sheets" {
		t.Errorf("expected SNMPMarkerCounterUnit='sheets', got %q", rep.Telemetry.SNMPMarkerCounterUnit)
	}
	// Verify Requirement 11: available counter is NEVER verified physical ejection
	if rep.Telemetry.PhysicalEjectionVerified || rep.Telemetry.SafeForPhysicalCompletion {
		t.Errorf("expected PhysicalEjectionVerified=false and SafeForPhysicalCompletion=false for read-only capability fingerprint")
	}
}

// -----------------------------------------------------------------------------
// Test 11: Counter Units (sheets=8 vs impressions=7 vs hours=11 vs feet=16)
// -----------------------------------------------------------------------------
func TestSNMPEmulator_CounterUnits(t *testing.T) {
	binPath, port, stateFile, stop := startTestSNMPProcess(t)
	defer stop()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	ctx := context.Background()

	cases := []struct {
		name          string
		unitArg       string
		wantUnit      int64
		wantUnitName  string
		wantIsPage    bool
	}{
		{"impressions(7)", "7", 7, "impressions", true},
		{"hours(11)", "11", 11, "hours", false},
		{"feet(16)", "16", 16, "feet", false},
		{"sheets(8)", "8", 8, "sheets", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setCmd := exec.Command(binPath, "set", "-state", stateFile, "-marker-unit", tc.unitArg)
			if out, err := setCmd.CombinedOutput(); err != nil {
				t.Fatalf("failed to set marker unit: %v (output: %s)", err, string(out))
			}
			time.Sleep(50 * time.Millisecond)

			session, err := NewRealDynamicSNMPSession(ctx, cfg)
			if err != nil {
				t.Fatalf("failed to create session: %v", err)
			}
			defer session.Close()

			res, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
			if err != nil {
				t.Fatalf("DiscoverSNMPDynamic failed: %v", err)
			}
			if len(res.Markers) == 0 {
				t.Fatalf("no markers discovered")
			}
			m := res.Markers[0]
			if m.CounterUnit != tc.wantUnit {
				t.Errorf("CounterUnit: got %d, want %d", m.CounterUnit, tc.wantUnit)
			}
			if m.CounterUnitName != tc.wantUnitName {
				t.Errorf("CounterUnitName: got %q, want %q", m.CounterUnitName, tc.wantUnitName)
			}
			if m.IsPageCounter != tc.wantIsPage {
				t.Errorf("IsPageCounter: got %v, want %v", m.IsPageCounter, tc.wantIsPage)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Test 12: Absent Lifetime Counts
// -----------------------------------------------------------------------------
func TestSNMPEmulator_AbsentLifetimeCounts(t *testing.T) {
	binPath, port, stateFile, stop := startTestSNMPProcess(t)
	defer stop()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	ctx := context.Background()

	// 1. Omit marker life count OID
	setCmd := exec.Command(binPath, "set", "-state", stateFile, "-marker-omit-life", "true")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to omit marker life: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	session, err := NewRealDynamicSNMPSession(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer session.Close()

	res, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed: %v", err)
	}
	if len(res.Markers) == 0 {
		t.Fatalf("expected marker row to still be discovered for unit")
	}
	if res.Markers[0].HasLifeCount {
		t.Errorf("expected HasLifeCount=false when life count OID is omitted")
	}
	if res.Markers[0].LifeCount != -1 {
		t.Errorf("expected LifeCount=-1 when life count OID is omitted, got %d", res.Markers[0].LifeCount)
	}

	// 2. Restore marker life count
	setCmd2 := exec.Command(binPath, "set", "-state", stateFile, "-marker-omit-life", "false")
	if out, err := setCmd2.CombinedOutput(); err != nil {
		t.Fatalf("failed to restore marker life: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	res2, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed on second check: %v", err)
	}
	if !res2.Markers[0].HasLifeCount || res2.Markers[0].LifeCount != 1000 {
		t.Errorf("expected HasLifeCount=true and LifeCount=1000 after restore, got %+v", res2.Markers[0])
	}
}

// -----------------------------------------------------------------------------
// Test 13: Missing Supplies
// -----------------------------------------------------------------------------
func TestSNMPEmulator_MissingSupplies(t *testing.T) {
	binPath, port, stateFile, stop := startTestSNMPProcess(t)
	defer stop()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	ctx := context.Background()

	// 1. Clear supplies to simulate printer with no readable supply tables
	setCmd := exec.Command(binPath, "set", "-state", stateFile, "-clear-supplies")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to clear supplies: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	session, err := NewRealDynamicSNMPSession(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer session.Close()

	res, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed: %v", err)
	}
	if len(res.Supplies) != 0 {
		t.Errorf("expected 0 supplies after clear-supplies, got %d", len(res.Supplies))
	}
	// Printer status is still healthy/idle and table walk succeeded
	if res.DiscoveryMode != "SNMP_TABLE_WALK" {
		t.Errorf("expected DiscoveryMode='SNMP_TABLE_WALK', got %q", res.DiscoveryMode)
	}

	// 2. Verify independent health polling works when supplies are missing
	health := PollHealthConfig(ctx, cfg, 100)
	if !health.Online || health.State != "IDLE" {
		t.Errorf("expected printer to remain Online and IDLE, got Online=%v State=%q", health.Online, health.State)
	}

	// 3. Restore supplies
	setCmd2 := exec.Command(binPath, "set", "-state", stateFile, "-restore-supplies")
	if out, err := setCmd2.CombinedOutput(); err != nil {
		t.Fatalf("failed to restore supplies: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	res2, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed on restore: %v", err)
	}
	if len(res2.Supplies) != 2 {
		t.Errorf("expected 2 supplies after restore, got %d", len(res2.Supplies))
	}
}

// -----------------------------------------------------------------------------
// Test 14: Printer Restart and Hardware Faults
// -----------------------------------------------------------------------------
func TestSNMPEmulator_RestartAndHardwareFaults(t *testing.T) {
	binPath, port, stateFile, stop := startTestSNMPProcess(t)
	defer stop()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	ctx := context.Background()

	// 1. Simulate printer restart with counter update
	setCmd := exec.Command(binPath, "set", "-state", stateFile, "-restart", "-counter", "500")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to simulate restart: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	session, err := NewRealDynamicSNMPSession(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer session.Close()

	res, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed: %v", err)
	}
	if len(res.Markers) == 0 || res.Markers[0].LifeCount != 500 {
		t.Errorf("expected marker life count 500 after reset, got %+v", res.Markers)
	}
	if res.SysUpTime == "" {
		t.Errorf("expected sysUpTime to be reported after restart")
	}

	// 2. Simulate hardware fault: paper jam + door open
	setCmd2 := exec.Command(binPath, "set", "-state", stateFile, "-set-error", "jammed,door_open")
	if out, err := setCmd2.CombinedOutput(); err != nil {
		t.Fatalf("failed to set faults: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	res2, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed on fault query: %v", err)
	}
	if len(res2.Printers) == 0 {
		t.Fatalf("no printer discovered")
	}
	flags := res2.Printers[0].ErrorFlags
	if !flags.Jammed || !flags.DoorOpen {
		t.Errorf("expected Jammed=true and DoorOpen=true, got Jammed=%v Door=%v", flags.Jammed, flags.DoorOpen)
	}
	if len(res2.Covers) > 0 && res2.Covers[0].Status != 3 {
		t.Errorf("expected cover status=3 (open), got %d", res2.Covers[0].Status)
	}

	// 3. Clear faults
	setCmd3 := exec.Command(binPath, "set", "-state", stateFile, "-clear-all-errors")
	if out, err := setCmd3.CombinedOutput(); err != nil {
		t.Fatalf("failed to clear errors: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	res3, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed after error clear: %v", err)
	}
	if res3.Printers[0].ErrorFlags.Jammed || res3.Printers[0].ErrorFlags.DoorOpen {
		t.Errorf("expected all error flags cleared, got %+v", res3.Printers[0].ErrorFlags)
	}
	if len(res3.Covers) > 0 && res3.Covers[0].Status != 4 {
		t.Errorf("expected cover status=4 (closed), got %d", res3.Covers[0].Status)
	}
}

// -----------------------------------------------------------------------------
// Test 15: Synthetic Fixture Labeling & Physical Ejection Proof Prohibition
// -----------------------------------------------------------------------------
func TestSNMPEmulator_SyntheticLabeling_NoEjectionProof(t *testing.T) {
	_, port, _, stop := startTestSNMPProcess(t)
	defer stop()

	ctx := context.Background()

	realFactory := func(snmpCfg SNMPConfig) (DynamicSNMPSession, error) {
		return NewRealDynamicSNMPSession(context.Background(), snmpCfg)
	}
	fpCfg := FingerprintConfig{
		SNMPHost:      "127.0.0.1",
		SNMPPort:      port,
		SNMPCommunity: "public",
		SNMPTimeout:   2 * time.Second,
		Redact:        false,
	}

	rep, err := RunFingerprint(ctx, nil, realFactory, fpCfg)
	if err != nil {
		t.Fatalf("RunFingerprint failed: %v", err)
	}

	// Requirement 11:
	// Verify that available telemetry does not assert physical ejection verification
	if rep.Telemetry.PhysicalEjectionVerified || rep.Telemetry.SafeForPhysicalCompletion {
		t.Errorf("safety violation: synthetic marker availability must never claim PhysicalEjectionVerified=true or SafeForPhysicalCompletion=true")
	}
	if rep.Telemetry.Rating == "Physical Completion Verified" {
		t.Errorf("safety violation: capability fingerprint must not claim Physical Completion Verified")
	}

	// Verify that human readable output contains sanitized and non-misleading status
	human := FormatHumanReadable(rep)
	if strings.Contains(human, "PHYSICAL COMPLETION VERIFIED") {
		t.Errorf("human output incorrectly claims physical completion verified")
	}
	if !strings.Contains(human, "TELEMETRY EVALUATION") {
		t.Errorf("human output missing telemetry evaluation section")
	}
}

// -----------------------------------------------------------------------------
// Test 16: GETBULK handling over real UDP
// -----------------------------------------------------------------------------
func TestSNMPEmulator_GetBulk_RealUDP(t *testing.T) {
	_, port, _, stop := startTestSNMPProcess(t)
	defer stop()

	client := &gosnmp.GoSNMP{
		Target:    "127.0.0.1",
		Port:      uint16(port),
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
	// Non-repeater: sysDescr -> returns sysObjectID
	// Repeater: prtMarkerTable -> returns up to 3 successors
	resp, err := client.GetBulk([]string{oidSysDescr, oidSubtreePrtMarker}, 1, 3)
	if err != nil {
		t.Fatalf("client.GetBulk failed: %v", err)
	}
	if len(resp.Variables) < 2 {
		t.Fatalf("expected at least 2 variables, got %d", len(resp.Variables))
	}
	if strings.TrimPrefix(resp.Variables[0].Name, ".") != normalizeOID(oidSysObjectID) {
		t.Errorf("expected first variable to be sysObjectID, got %s", resp.Variables[0].Name)
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

	// 4. Excessive MaxRepetitions (e.g. 50000) - bounded response
	respExcessive, err := client.GetBulk([]string{oidSubtreePrtMarker}, 0, 50000)
	if err != nil {
		t.Fatalf("GetBulk with excessive MaxRepetitions failed: %v", err)
	}
	if len(respExcessive.Variables) > 128 {
		t.Errorf("response exceeded bounded limit: got %d, max 128", len(respExcessive.Variables))
	}
	if len(respExcessive.Variables) == 0 {
		t.Errorf("expected non-empty response for excessive maxRepetitions query")
	}
}

// -----------------------------------------------------------------------------
// Test 17: Explicit Zero Uptime Preservation & Simulated Restart
// -----------------------------------------------------------------------------
func TestSNMPEmulator_ZeroUptimeAndRestart(t *testing.T) {
	binPath, port, stateFile, stop := startTestSNMPProcess(t)
	defer stop()

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      port,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   0,
	}
	ctx := context.Background()

	// 1. Explicitly configure sysUpTime=0
	setCmd := exec.Command(binPath, "set", "-state", stateFile, "-uptime", "0")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to set uptime 0: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	session, err := NewRealDynamicSNMPSession(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer session.Close()

	// Direct GET query for sysUpTime (1.3.6.1.2.1.1.3.0)
	resp, err := session.Get([]string{oidSysUpTime})
	if err != nil {
		t.Fatalf("session.Get(oidSysUpTime) failed: %v", err)
	}
	if len(resp.Variables) != 1 {
		t.Fatalf("expected 1 PDU for sysUpTime, got %d", len(resp.Variables))
	}
	pdu := resp.Variables[0]
	if pdu.Type != gosnmp.TimeTicks {
		t.Errorf("expected TimeTicks type, got %v", pdu.Type)
	}
	if pdu.Value.(uint32) != 0 {
		t.Errorf("expected sysUpTime=0 to be preserved, got %v", pdu.Value)
	}

	// Verify DiscoverSNMPDynamic reports "0s"
	res, err := DiscoverSNMPDynamic(ctx, session, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("DiscoverSNMPDynamic failed: %v", err)
	}
	if res.SysUpTime != "0s" {
		t.Errorf("expected DiscoverSNMPDynamic SysUpTime='0s', got %q", res.SysUpTime)
	}

	// 2. Simulate printer restart without explicit uptime flag -> defaults to uptime 0
	restartCmd := exec.Command(binPath, "set", "-state", stateFile, "-restart")
	if out, err := restartCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to simulate restart: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	resp2, err := session.Get([]string{oidSysUpTime})
	if err != nil {
		t.Fatalf("session.Get(oidSysUpTime) after restart failed: %v", err)
	}
	if resp2.Variables[0].Value.(uint32) != 0 {
		t.Errorf("expected sysUpTime=0 on restart, got %v", resp2.Variables[0].Value)
	}

	// 3. Set non-zero uptime and verify update
	setNonZero := exec.Command(binPath, "set", "-state", stateFile, "-uptime", "12345")
	if out, err := setNonZero.CombinedOutput(); err != nil {
		t.Fatalf("failed to set non-zero uptime: %v (output: %s)", err, string(out))
	}
	time.Sleep(50 * time.Millisecond)

	resp3, err := session.Get([]string{oidSysUpTime})
	if err != nil {
		t.Fatalf("session.Get(oidSysUpTime) after non-zero set failed: %v", err)
	}
	if resp3.Variables[0].Value.(uint32) != 12345 {
		t.Errorf("expected sysUpTime=12345, got %v", resp3.Variables[0].Value)
	}
}
