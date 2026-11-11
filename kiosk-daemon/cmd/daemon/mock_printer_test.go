package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestMockPrinter_AuthoritativeSheets(t *testing.T) {
	tempDir := t.TempDir()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockState := &mockPrinterState{}
	addr, cleanup, err := startMockPrinter(ctx, tempDir, &wg, mockState)
	if err != nil {
		t.Fatalf("startMockPrinter failed: %v", err)
	}
	defer cleanup()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial mock printer: %v", err)
	}
	defer conn.Close()

	// Send PJL stream with authoritative TOTAL_SHEETS=3 and zero PDF page objects
	pjlStream := fmt.Sprintf("\x1b%%-12345X@PJL JOB NAME=\"test-auth\"\r\n" +
		"@PJL USTATUS DEVICE=ON\r\n" +
		"@PJL USTATUS PAGE=ON\r\n" +
		"@PJL COMMENT XEROXGO TOTAL_SHEETS=3\r\n" +
		"@PJL ENTER LANGUAGE=PDF\r\n" +
		"%%PDF-DUMMY-PAYLOAD-WITHOUT-PAGE-OBJECTS\r\n" +
		"\x1b%%-12345X@PJL EOJ\r\n")

	if _, err := conn.Write([]byte(pjlStream)); err != nil {
		t.Fatalf("failed to write PJL stream: %v", err)
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	// Read responses from mock printer
	reader := bufio.NewReader(conn)
	var sheetsReceived []int
	jobEnded := false

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "@PJL USTATUS PAGE" {
				pageNumStr, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				var sheetNum int
				if _, err := fmt.Sscanf(strings.TrimSpace(pageNumStr), "%d", &sheetNum); err == nil {
					sheetsReceived = append(sheetsReceived, sheetNum)
				}
			} else if line == "@PJL USTATUS JOB" {
				statusStr, _ := reader.ReadString('\n')
				if strings.Contains(statusStr, "END") {
					jobEnded = true
					return
				}
			}
		}
	}()

	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for mock printer responses")
	}

	if len(sheetsReceived) != 3 {
		t.Fatalf("sheetsReceived = %v, want 3 sheets (authoritative)", sheetsReceived)
	}
	if sheetsReceived[0] != 1 || sheetsReceived[1] != 2 || sheetsReceived[2] != 3 {
		t.Errorf("expected sheets [1, 2, 3], got %v", sheetsReceived)
	}
	if !jobEnded {
		t.Errorf("expected job ended confirmation")
	}
}

func TestMockPrinter_MidPrintFailure_Jam(t *testing.T) {
	tempDir := t.TempDir()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Fail with jam after sheet 1
	mockState := &mockPrinterState{failureType: "jam", failureSheet: 1}
	addr, cleanup, err := startMockPrinter(ctx, tempDir, &wg, mockState)
	if err != nil {
		t.Fatalf("startMockPrinter failed: %v", err)
	}
	defer cleanup()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	pjlStream := "\x1b%%-12345X@PJL JOB NAME=\"test-jam\"\r\n" +
		"@PJL COMMENT XEROXGO TOTAL_SHEETS=3\r\n" +
		"@PJL ENTER LANGUAGE=PDF\r\n" +
		"dummy-pdf\r\n" +
		"\x1b%%-12345X@PJL EOJ\r\n"

	if _, err := conn.Write([]byte(pjlStream)); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	reader := bufio.NewReader(conn)
	var sheetsReceived []int
	gotEOF := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "connection reset") {
				gotEOF = true
			}
			break
		}
		if strings.TrimSpace(line) == "@PJL USTATUS PAGE" {
			numStr, _ := reader.ReadString('\n')
			var s int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(numStr), "%d", &s); scanErr == nil {
				sheetsReceived = append(sheetsReceived, s)
			}
		}
	}

	if len(sheetsReceived) != 1 {
		t.Errorf("expected exactly 1 sheet printed before jam, got %v", sheetsReceived)
	}
	if !gotEOF {
		t.Errorf("expected connection to be aborted on jam")
	}
}

func TestMockPrinter_ImmediateFailure_Offline(t *testing.T) {
	tempDir := t.TempDir()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockState := &mockPrinterState{failureType: "offline", failureSheet: 0}
	addr, cleanup, err := startMockPrinter(ctx, tempDir, &wg, mockState)
	if err != nil {
		t.Fatalf("startMockPrinter failed: %v", err)
	}
	defer cleanup()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 100)
	n, readErr := conn.Read(buf)
	if n != 0 || readErr != io.EOF {
		t.Errorf("expected immediate EOF on offline mock printer, got n=%d, err=%v", n, readErr)
	}
}

func TestMockHealthPoller_States(t *testing.T) {
	testCases := []struct {
		name            string
		failureFlag     string
		estimatedSheets int
		wantOnline      bool
		wantState       string
		wantTray        string
		wantJam         bool
	}{
		{
			name:            "normal_idle_healthy",
			failureFlag:     "",
			estimatedSheets: 100,
			wantOnline:      true,
			wantState:       "IDLE",
			wantTray:        "OK",
			wantJam:         false,
		},
		{
			name:            "low_paper_warning",
			failureFlag:     "",
			estimatedSheets: 5,
			wantOnline:      true,
			wantState:       "IDLE",
			wantTray:        "LOW",
			wantJam:         false,
		},
		{
			name:            "empty_paper_stock_zero",
			failureFlag:     "",
			estimatedSheets: 0,
			wantOnline:      true,
			wantState:       "IDLE",
			wantTray:        "EMPTY",
			wantJam:         false,
		},
		{
			name:            "injected_paper_empty",
			failureFlag:     "paper-empty",
			estimatedSheets: 50,
			wantOnline:      true,
			wantState:       "IDLE",
			wantTray:        "EMPTY",
			wantJam:         false,
		},
		{
			name:            "injected_jam",
			failureFlag:     "jam",
			estimatedSheets: 50,
			wantOnline:      true,
			wantState:       "ERROR",
			wantTray:        "OK",
			wantJam:         true,
		},
		{
			name:            "injected_offline",
			failureFlag:     "offline",
			estimatedSheets: 50,
			wantOnline:      false,
			wantState:       "OFFLINE",
			wantTray:        "OK",
			wantJam:         false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockState := &mockPrinterState{
				failureType:  tc.failureFlag,
				failureSheet: 0,
			}
			if tc.failureFlag != "" {
				mockState.failureTriggered.Store(true)
			}
			poller := buildMockHealthPoller(mockState)
			h := poller(context.Background(), tc.estimatedSheets)

			if h.Online != tc.wantOnline {
				t.Errorf("Online = %v, want %v", h.Online, tc.wantOnline)
			}
			if h.State != tc.wantState {
				t.Errorf("State = %v, want %v", h.State, tc.wantState)
			}
			if h.PaperTrayStatus != tc.wantTray {
				t.Errorf("PaperTrayStatus = %v, want %v", h.PaperTrayStatus, tc.wantTray)
			}
			if h.PaperJam != tc.wantJam {
				t.Errorf("PaperJam = %v, want %v", h.PaperJam, tc.wantJam)
			}
		})
	}
}

func TestMockPrinter_FailureTiming_ReadyBeforeScheduledSheet(t *testing.T) {
	tempDir := t.TempDir()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockState := &mockPrinterState{
		failureType:  "jam",
		failureSheet: 2,
	}

	addr, cleanup, err := startMockPrinter(ctx, tempDir, &wg, mockState)
	if err != nil {
		t.Fatalf("startMockPrinter failed: %v", err)
	}
	defer cleanup()

	poller := buildMockHealthPoller(mockState)

	// 1. Health MUST be READY (Online=true, State="IDLE", PaperJam=false) BEFORE the scheduled failure sheet
	hBefore := poller(ctx, 100)
	if !hBefore.Online || hBefore.State != "IDLE" || hBefore.PaperJam {
		t.Fatalf("expected healthy printer before scheduled failure sheet, got %+v", hBefore)
	}

	// 2. Stream a 3-sheet print job
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	pjlStream := "\x1b%%-12345X@PJL JOB NAME=\"test-failure-timing\"\r\n" +
		"@PJL COMMENT XEROXGO TOTAL_SHEETS=3\r\n" +
		"@PJL ENTER LANGUAGE=PDF\r\n" +
		"dummy-pdf-content\r\n" +
		"\x1b%%-12345X@PJL EOJ\r\n"

	if _, err := conn.Write([]byte(pjlStream)); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	reader := bufio.NewReader(conn)
	var sheetsReceived []int
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.TrimSpace(line) == "@PJL USTATUS PAGE" {
			numStr, _ := reader.ReadString('\n')
			var s int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(numStr), "%d", &s); scanErr == nil {
				sheetsReceived = append(sheetsReceived, s)
			}
		}
	}

	// Must have received exactly sheets 1 and 2
	if len(sheetsReceived) != 2 || sheetsReceived[0] != 1 || sheetsReceived[1] != 2 {
		t.Fatalf("expected sheets [1, 2] before jam trigger, got %v", sheetsReceived)
	}

	// 3. Health MUST now report ERROR and PaperJam=true
	if !mockState.failureTriggered.Load() {
		t.Fatal("expected failureTriggered to be true after failure sheet")
	}

	hAfter := poller(ctx, 100)
	if hAfter.State != "ERROR" || !hAfter.PaperJam {
		t.Fatalf("expected ERROR and PaperJam=true after failure triggered, got %+v", hAfter)
	}
}

// helper matching the health poller builder in main.go
func buildMockHealthPoller(mockState *mockPrinterState) func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
	return func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
		h := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   estimatedSheets,
			TonerLevelPercent: 95,
			DrumLifePercent:   98,
			UpdatedAt:         time.Now().UTC(),
		}

		if estimatedSheets <= 0 {
			h.PaperTrayStatus = "EMPTY"
		} else if estimatedSheets <= 10 {
			h.PaperTrayStatus = "LOW"
		}

		if mockState != nil && mockState.failureTriggered.Load() {
			switch strings.ToLower(strings.TrimSpace(mockState.failureType)) {
			case "paper-empty":
				h.PaperTrayStatus = "EMPTY"
				h.EstimatedSheets = 0
				h.ErrorMessage = "printer paper tray empty"
			case "jam":
				h.State = "ERROR"
				h.PaperJam = true
				h.ErrorMessage = "printer has a paper jam"
			case "offline":
				h.Online = false
				h.State = "OFFLINE"
				h.ErrorMessage = "printer is offline or disconnected"
			}
		}

		return h
	}
}
