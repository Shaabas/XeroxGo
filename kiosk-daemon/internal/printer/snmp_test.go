package printer

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gosnmp/gosnmp"
)

type fakeSnmpSession struct {
	getFunc   func(oids []string) (*gosnmp.SnmpPacket, error)
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

func (f *fakeSnmpSession) Close() error {
	f.closed = true
	if f.closeFunc != nil {
		return f.closeFunc()
	}
	return nil
}

func createValidPacket(status int, errorByte byte, markerCount int64) *gosnmp.SnmpPacket {
	return &gosnmp.SnmpPacket{
		Error: gosnmp.NoError,
		Variables: []gosnmp.SnmpPDU{
			{
				Name:  oidHrPrinterStatus,
				Type:  gosnmp.Integer,
				Value: status,
			},
			{
				Name:  oidHrPrinterDetectedErrorState,
				Type:  gosnmp.OctetString,
				Value: []byte{errorByte},
			},
			{
				Name:  oidPrtMarkerLifeCount,
				Type:  gosnmp.Counter32,
				Value: uint32(markerCount),
			},
		},
	}
}

func TestDecodeErrorBitmask(t *testing.T) {
	t.Run("empty_input", func(t *testing.T) {
		flags := DecodeErrorBitmask([]byte{})
		expected := PrinterErrorFlags{}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("zero_byte", func(t *testing.T) {
		flags := DecodeErrorBitmask([]byte{0x00})
		expected := PrinterErrorFlags{}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("individual_bits", func(t *testing.T) {
		tests := []struct {
			name     string
			input    byte
			expected PrinterErrorFlags
		}{
			{"LowPaper", 0x80, PrinterErrorFlags{LowPaper: true}},
			{"NoPaper", 0x40, PrinterErrorFlags{NoPaper: true}},
			{"LowToner", 0x20, PrinterErrorFlags{LowToner: true}},
			{"NoToner", 0x10, PrinterErrorFlags{NoToner: true}},
			{"DoorOpen", 0x08, PrinterErrorFlags{DoorOpen: true}},
			{"Jammed", 0x04, PrinterErrorFlags{Jammed: true}},
			{"Offline", 0x02, PrinterErrorFlags{Offline: true}},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				flags := DecodeErrorBitmask([]byte{tc.input})
				if flags != tc.expected {
					t.Errorf("input 0x%02X: got %+v, want %+v", tc.input, flags, tc.expected)
				}
			})
		}
	})

	t.Run("all_seven_bits", func(t *testing.T) {
		allBits := byte(0x80 | 0x40 | 0x20 | 0x10 | 0x08 | 0x04 | 0x02)
		flags := DecodeErrorBitmask([]byte{allBits})
		expected := PrinterErrorFlags{
			LowPaper: true,
			NoPaper:  true,
			LowToner: true,
			NoToner:  true,
			DoorOpen: true,
			Jammed:   true,
			Offline:  true,
		}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("unrelated_low_order_bits", func(t *testing.T) {
		flags := DecodeErrorBitmask([]byte{0x01})
		expected := PrinterErrorFlags{}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("multiple_bytes_inspects_first_byte_only", func(t *testing.T) {
		flags := DecodeErrorBitmask([]byte{0x80, 0x08, 0x04})
		expected := PrinterErrorFlags{LowPaper: true}
		if flags != expected {
			t.Errorf("got %+v, want %+v", flags, expected)
		}
	})

	t.Run("input_slice_unchanged", func(t *testing.T) {
		orig := []byte{0x80, 0x20, 0x04}
		copyBuf := make([]byte, len(orig))
		copy(copyBuf, orig)

		_ = DecodeErrorBitmask(orig)

		if !bytes.Equal(orig, copyBuf) {
			t.Errorf("input slice modified: got %v, want %v", orig, copyBuf)
		}
	})
}

func TestPollHealthStatusMapping(t *testing.T) {
	tests := []struct {
		name      string
		statusVal int
		wantState string
		wantErr   bool
	}{
		{"IDLE", 3, "IDLE", false},
		{"PRINTING", 4, "PRINTING", false},
		{"WARMING_UP", 5, "WARMING_UP", false},
		{"ERROR", 6, "ERROR", true},
		{"OFFLINE", 7, "OFFLINE", true},
		{"unrecognized_1", 1, "ERROR", true},
		{"unrecognized_2", 2, "ERROR", true},
		{"unrecognized_99", 99, "ERROR", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recordedOIDs []string
			session := &fakeSnmpSession{
				getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
					recordedOIDs = append([]string(nil), oids...)
					return createValidPacket(tc.statusVal, 0x00, 1000), nil
				},
			}

			factory := func(ctx context.Context) (snmpSession, error) {
				return session, nil
			}

			ctx := context.Background()
			health := pollHealthWithFactory(ctx, 200, factory)

			if health.State != tc.wantState {
				t.Errorf("State = %s, want %s", health.State, tc.wantState)
			}
			if tc.wantState == "OFFLINE" && health.Online {
				t.Errorf("printer reported OFFLINE must have Online=false")
			}
			if tc.wantState != "OFFLINE" && !health.Online {
				t.Errorf("printer with state %s should have Online=true", tc.wantState)
			}
			if tc.wantErr && health.ErrorMessage == "" {
				t.Errorf("expected non-empty ErrorMessage for state %s", tc.wantState)
			}
			if !tc.wantErr && health.ErrorMessage != "" {
				t.Errorf("expected empty ErrorMessage for healthy state, got %q", health.ErrorMessage)
			}

			// Verify OIDs and order
			expectedOIDs := []string{
				oidHrPrinterStatus,
				oidHrPrinterDetectedErrorState,
				oidPrtMarkerLifeCount,
			}
			if !reflect.DeepEqual(recordedOIDs, expectedOIDs) {
				t.Errorf("queried OIDs = %v, want %v", recordedOIDs, expectedOIDs)
			}
		})
	}
}

func TestPollHealthErrorBitmask(t *testing.T) {
	tests := []struct {
		name         string
		statusVal    int
		errorByte    byte
		wantOnline   bool
		wantState    string
		wantTray     string
		wantCover    bool
		wantJam      bool
		wantMsgWords []string
	}{
		{
			name:         "low_paper",
			statusVal:    3,
			errorByte:    0x80,
			wantOnline:   true,
			wantState:    "IDLE",
			wantTray:     "LOW",
			wantCover:    false,
			wantJam:      false,
			wantMsgWords: []string{"low paper"},
		},
		{
			name:         "no_paper",
			statusVal:    3,
			errorByte:    0x40,
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "EMPTY",
			wantCover:    false,
			wantJam:      false,
			wantMsgWords: []string{"no paper"},
		},
		{
			name:         "low_toner",
			statusVal:    3,
			errorByte:    0x20,
			wantOnline:   true,
			wantState:    "IDLE",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantMsgWords: []string{"low toner"},
		},
		{
			name:         "no_toner",
			statusVal:    3,
			errorByte:    0x10,
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantMsgWords: []string{"no toner"},
		},
		{
			name:         "door_open",
			statusVal:    3,
			errorByte:    0x08,
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    true,
			wantJam:      false,
			wantMsgWords: []string{"door open"},
		},
		{
			name:         "paper_jam",
			statusVal:    3,
			errorByte:    0x04,
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      true,
			wantMsgWords: []string{"paper jam"},
		},
		{
			name:         "offline_bit",
			statusVal:    3,
			errorByte:    0x02,
			wantOnline:   false,
			wantState:    "OFFLINE",
			wantTray:     "OK",
			wantCover:    false,
			wantJam:      false,
			wantMsgWords: []string{"offline"},
		},
		{
			name:         "door_open_and_jam",
			statusVal:    3,
			errorByte:    0x08 | 0x04,
			wantOnline:   true,
			wantState:    "ERROR",
			wantTray:     "OK",
			wantCover:    true,
			wantJam:      true,
			wantMsgWords: []string{"door open", "paper jam"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := &fakeSnmpSession{
				getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
					return createValidPacket(tc.statusVal, tc.errorByte, 2500), nil
				},
			}
			factory := func(ctx context.Context) (snmpSession, error) {
				return session, nil
			}

			health := pollHealthWithFactory(context.Background(), 250, factory)

			if health.Online != tc.wantOnline {
				t.Errorf("Online = %v, want %v", health.Online, tc.wantOnline)
			}
			if health.State != tc.wantState {
				t.Errorf("State = %s, want %s", health.State, tc.wantState)
			}
			if health.PaperTrayStatus != tc.wantTray {
				t.Errorf("PaperTrayStatus = %s, want %s", health.PaperTrayStatus, tc.wantTray)
			}
			if health.CoverOpen != tc.wantCover {
				t.Errorf("CoverOpen = %v, want %v", health.CoverOpen, tc.wantCover)
			}
			if health.PaperJam != tc.wantJam {
				t.Errorf("PaperJam = %v, want %v", health.PaperJam, tc.wantJam)
			}

			for _, word := range tc.wantMsgWords {
				if !strings.Contains(strings.ToLower(health.ErrorMessage), word) {
					t.Errorf("ErrorMessage %q does not contain expected %q", health.ErrorMessage, word)
				}
			}
		})
	}
}

func TestPollHealthUsesEstimatedSheets(t *testing.T) {
	session := &fakeSnmpSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return createValidPacket(3, 0x00, 100), nil
		},
	}
	factory := func(ctx context.Context) (snmpSession, error) {
		return session, nil
	}

	health := pollHealthWithFactory(context.Background(), 173, factory)
	if health.EstimatedSheets != 173 {
		t.Errorf("EstimatedSheets = %d, want 173", health.EstimatedSheets)
	}
}

func TestPollHealthRequestFailure(t *testing.T) {
	session := &fakeSnmpSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			return nil, errors.New("network timeout")
		},
	}
	factory := func(ctx context.Context) (snmpSession, error) {
		return session, nil
	}

	health := pollHealthWithFactory(context.Background(), 150, factory)

	if health.Online {
		t.Error("expected Online = false on request failure")
	}
	if health.State != "OFFLINE" {
		t.Errorf("State = %s, want OFFLINE", health.State)
	}
	if health.EstimatedSheets != 150 {
		t.Errorf("EstimatedSheets = %d, want 150", health.EstimatedSheets)
	}
	if !strings.Contains(health.ErrorMessage, "network timeout") {
		t.Errorf("expected ErrorMessage to contain 'network timeout', got %q", health.ErrorMessage)
	}
	if !session.closed {
		t.Error("expected fake session to be closed")
	}
}

func TestPollHealthMalformedResponses(t *testing.T) {
	tests := []struct {
		name      string
		pkt       *gosnmp.SnmpPacket
		errSubstr string
	}{
		{
			name:      "nil_packet",
			pkt:       nil,
			errSubstr: "nil",
		},
		{
			name: "snmp_error_status",
			pkt: &gosnmp.SnmpPacket{
				Error:      gosnmp.NoSuchName,
				ErrorIndex: 1,
			},
			errSubstr: "SNMP error status",
		},
		{
			name: "missing_status_oid",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00}},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.Counter32, Value: uint32(10)},
				},
			},
			errSubstr: "missing required OID",
		},
		{
			name: "missing_error_state_oid",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: 3},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.Counter32, Value: uint32(10)},
				},
			},
			errSubstr: "missing required OID",
		},
		{
			name: "missing_marker_counter_oid",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: 3},
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00}},
				},
			},
			errSubstr: "missing required OID",
		},
		{
			name: "wrong_pdu_type_status",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterStatus, Type: gosnmp.OctetString, Value: []byte("3")},
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00}},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.Counter32, Value: uint32(10)},
				},
			},
			errSubstr: "invalid PDU type",
		},
		{
			name: "wrong_pdu_type_error_state",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: 3},
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.Integer, Value: 0},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.Counter32, Value: uint32(10)},
				},
			},
			errSubstr: "invalid PDU type",
		},
		{
			name: "wrong_pdu_type_marker_counter",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: 3},
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00}},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.OctetString, Value: []byte("100")},
				},
			},
			errSubstr: "invalid PDU type",
		},
		{
			name: "invalid_marker_counter_negative",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: 3},
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00}},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.Integer, Value: -5},
				},
			},
			errSubstr: "negative marker life count",
		},
		{
			name: "invalid_status_representation",
			pkt: &gosnmp.SnmpPacket{
				Error: gosnmp.NoError,
				Variables: []gosnmp.SnmpPDU{
					{Name: oidHrPrinterStatus, Type: gosnmp.Integer, Value: "invalid_string"},
					{Name: oidHrPrinterDetectedErrorState, Type: gosnmp.OctetString, Value: []byte{0x00}},
					{Name: oidPrtMarkerLifeCount, Type: gosnmp.Counter32, Value: uint32(10)},
				},
			},
			errSubstr: "cannot parse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := &fakeSnmpSession{
				getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
					return tc.pkt, nil
				},
			}
			factory := func(ctx context.Context) (snmpSession, error) {
				return session, nil
			}

			health := pollHealthWithFactory(context.Background(), 250, factory)

			if health.Online {
				t.Errorf("expected Online = false for malformed response case %s", tc.name)
			}
			if health.State != "OFFLINE" {
				t.Errorf("expected State = OFFLINE for case %s, got %s", tc.name, health.State)
			}
			if !strings.Contains(strings.ToLower(health.ErrorMessage), strings.ToLower(tc.errSubstr)) {
				t.Errorf("ErrorMessage %q does not contain %q", health.ErrorMessage, tc.errSubstr)
			}
		})
	}
}

func TestPollHealthCanceledContext(t *testing.T) {
	sessionCreated := false
	factory := func(ctx context.Context) (snmpSession, error) {
		sessionCreated = true
		return &fakeSnmpSession{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	health := pollHealthWithFactory(ctx, 180, factory)

	if sessionCreated {
		t.Error("expected session not to be created when context is already canceled")
	}
	if health.Online {
		t.Error("expected Online = false for canceled context")
	}
	if health.State != "OFFLINE" {
		t.Errorf("State = %s, want OFFLINE", health.State)
	}
	if !strings.Contains(health.ErrorMessage, "canceled") {
		t.Errorf("expected ErrorMessage to contain 'canceled', got %q", health.ErrorMessage)
	}
}

func TestPollHealthConnectFailure(t *testing.T) {
	factory := func(ctx context.Context) (snmpSession, error) {
		return nil, errors.New("connection refused")
	}

	health := pollHealthWithFactory(context.Background(), 210, factory)

	if health.Online {
		t.Error("expected Online = false on connection failure")
	}
	if health.State != "OFFLINE" {
		t.Errorf("State = %s, want OFFLINE", health.State)
	}
	if !strings.Contains(health.ErrorMessage, "connection refused") {
		t.Errorf("expected ErrorMessage to contain 'connection refused', got %q", health.ErrorMessage)
	}
}

func TestPollHealthClosesSession(t *testing.T) {
	session := &fakeSnmpSession{
		getFunc: func(oids []string) (*gosnmp.SnmpPacket, error) {
			// Return malformed packet
			return nil, errors.New("corrupt response")
		},
	}
	factory := func(ctx context.Context) (snmpSession, error) {
		return session, nil
	}

	_ = pollHealthWithFactory(context.Background(), 250, factory)

	if !session.closed {
		t.Error("expected session to be closed even on malformed/error response")
	}
}
