package printer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestPrintPDFWithCUPS_Validation(t *testing.T) {
	validCmd := contracts.PrintJobCommand{
		JobID:       "job-123",
		Copies:      1,
		TotalPages:  1,
		TotalSheets: 1,
		Duplex:      contracts.DuplexSimplex,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("request id is DCPT310-1 (1 file(s))"), nil
	}

	tests := []struct {
		name    string
		ctx     context.Context
		queue   string
		cmd     contracts.PrintJobCommand
		pdfPath string
		wantErr string
	}{
		{
			name:    "nil context",
			ctx:     nil,
			queue:   "DCPT310",
			cmd:     validCmd,
			pdfPath: "/tmp/test.pdf",
			wantErr: "context cannot be nil",
		},
		{
			name:    "empty queue",
			ctx:     context.Background(),
			queue:   "",
			cmd:     validCmd,
			pdfPath: "/tmp/test.pdf",
			wantErr: "queue name cannot be empty",
		},
		{
			name:    "empty pdf path",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     validCmd,
			pdfPath: "",
			wantErr: "pdf path cannot be empty",
		},
		{
			name:    "empty job id",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     contracts.PrintJobCommand{Copies: 1, TotalSheets: 1},
			pdfPath: "/tmp/test.pdf",
			wantErr: "job_id cannot be empty",
		},
		{
			name:    "zero copies",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     contracts.PrintJobCommand{JobID: "j1", Copies: 0, TotalSheets: 1},
			pdfPath: "/tmp/test.pdf",
			wantErr: "copies must be between 1 and 999",
		},
		{
			name:    "zero sheets",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     contracts.PrintJobCommand{JobID: "j1", Copies: 1, TotalSheets: 0},
			pdfPath: "/tmp/test.pdf",
			wantErr: "total_sheets must be >= 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := PrintPDFWithCUPSCustom(tc.ctx, runner, tc.queue, tc.cmd, tc.pdfPath, nil, 10*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPrintPDFWithCUPS_Success(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	lpstatCalls := 0
	var executedCommands []string

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		cmdStr := name + " " + strings.Join(args, " ")
		executedCommands = append(executedCommands, cmdStr)

		switch name {
		case "lp":
			return []byte("request id is DCPT310-42 (1 file(s))\n"), nil
		case "lpstat":
			if len(args) > 1 && args[1] == "completed" {
				return []byte("DCPT310-42 username 1024 Tue Nov 3 20:30:00 2026\n\tAlerts: job-completed-successfully\n\tqueued for DCPT310\n"), nil
			}
			lpstatCalls++
			if lpstatCalls == 1 {
				return []byte("DCPT310-42 username 1024 Tue Nov 3 20:30:00 2026\n"), nil
			}
			// Second call: job finished, no longer active
			return []byte(""), nil
		default:
			return nil, errors.New("unexpected command: " + name)
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-xyz",
		Copies:      2,
		TotalPages:  4,
		TotalSheets: 4,
		Duplex:      contracts.DuplexLongEdge,
	}

	var completedSheets []int
	onPage := func(sheets int) {
		mu.Lock()
		completedSheets = append(completedSheets, sheets)
		mu.Unlock()
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/var/kiosk/job-xyz.pdf", onPage, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(executedCommands) < 2 {
		t.Fatalf("expected at least 2 commands executed, got %d", len(executedCommands))
	}

	if !strings.Contains(executedCommands[0], "lp -d DCPT310 -n 2 -t job-xyz -o sides=two-sided-long-edge /var/kiosk/job-xyz.pdf") {
		t.Errorf("unexpected lp invocation: %s", executedCommands[0])
	}

	// Without hardware SNMP odometer or IPP sheets completed counter,
	// CUPS completion must NOT fabricate full physical sheet progress.
	if len(completedSheets) != 0 {
		t.Errorf("expected no synthetic physical sheet progress without hardware proof, got %v", completedSheets)
	}
}

func TestPrintPDFWithCUPS_Success_IPP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	ippCalls := 0
	var completedSheets []int

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is HL-L5210DN-101 (1 file(s))\n"), nil
		case "ipptool":
			ippCalls++
			if ippCalls == 1 {
				// 1st poll: processing, 2 sheets completed
				return []byte(`
job-id (integer) = 101
job-state (enum) = processing
job-state-reasons (keyword) = none
job-media-sheets-completed (integer) = 2
`), nil
			}
			// 2nd poll: confirmed completed
			return []byte(`
job-id (integer) = 101
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
job-media-sheets-completed (integer) = 4
`), nil
		default:
			return nil, errors.New("unexpected command: " + name)
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-ipp-1",
		Copies:      1,
		TotalPages:  8,
		TotalSheets: 4,
		Duplex:      contracts.DuplexLongEdge,
	}

	onPage := func(sheets int) {
		mu.Lock()
		completedSheets = append(completedSheets, sheets)
		mu.Unlock()
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "HL-L5210DN", cmd, "/var/kiosk/job-ipp.pdf", onPage, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Without hardware odometer proof, unverified CUPS IPP sheet counters
	// must NOT enter the onPage callback.
	if len(completedSheets) != 0 {
		t.Errorf("expected no physical sheet progress without hardware odometer proof, got %v", completedSheets)
	}
}

func TestPrintPDFWithCUPS_Success_IPP_WithHardwareOdometer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	ippCalls := 0
	var completedSheets []int

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is HL-L5210DN-102 (1 file(s))\n"), nil
		case "ipptool":
			ippCalls++
			if ippCalls == 1 {
				return []byte(`
job-id (integer) = 102
job-state (enum) = processing
job-state-reasons (keyword) = none
job-media-sheets-completed (integer) = 2
`), nil
			}
			return []byte(`
job-id (integer) = 102
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
job-media-sheets-completed (integer) = 4
`), nil
		default:
			return nil, errors.New("unexpected command: " + name)
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-ipp-2",
		Copies:      1,
		TotalPages:  8,
		TotalSheets: 4,
		Duplex:      contracts.DuplexLongEdge,
	}

	onPage := func(sheets int) {
		mu.Lock()
		completedSheets = append(completedSheets, sheets)
		mu.Unlock()
	}

	var odoCalls int32
	poller := func(ctx context.Context) HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		var life int64 = 5000
		if c > 1 {
			life += int64((c - 1) * 2)
		}
		return HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	hooks := &CUPSPrintHooks{
		TelemetryPoller: poller,
	}

	err := PrintPDFWithCUPSHooksCustom(ctx, runner, "HL-L5210DN", cmd, "/var/kiosk/job-ipp.pdf", onPage, 10*time.Millisecond, hooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(completedSheets) == 0 {
		t.Fatal("expected physical sheet progress driven by hardware odometer")
	}
	last := completedSheets[len(completedSheets)-1]
	if last != 4 {
		t.Errorf("expected final completed sheet count 4, got %d (all: %v)", last, completedSheets)
	}
}

func TestPrintPDFWithCUPS_LPError(t *testing.T) {
	ctx := context.Background()
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("lp: error - unknown printer"), errors.New("exit status 1")
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-err",
		Copies:      1,
		TotalSheets: 1,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/test.pdf", nil, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "failed to submit job to CUPS queue") {
		t.Fatalf("expected lp error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_QueueStoppedError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is DCPT310-55 (1 file(s))\n"), nil
		case "lpstat":
			if len(args) > 0 && args[0] == "-p" {
				return []byte("printer DCPT310 is stopped; reason: Filter failed"), nil
			}
			return []byte("DCPT310-55 username 1024 Tue Nov 3 20:30:00 2026\n"), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-fail",
		Copies:      1,
		TotalSheets: 1,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/var/kiosk/job-fail.pdf", nil, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "stopped/disabled") {
		t.Fatalf("expected stopped/disabled error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_Aborted_FilterFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is HPTank1020w-202 (1 file(s))\n"), nil
		case "ipptool":
			return []byte(`
job-id (integer) = 202
job-state (enum) = aborted
job-state-reasons (keyword) = filter-failed
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-abort",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "HPTank1020w", cmd, "/tmp/abort.pdf", nil, 5*time.Millisecond)
	if err == nil || (!strings.Contains(err.Error(), "aborted") && !strings.Contains(err.Error(), "stopped/disabled")) {
		t.Fatalf("expected aborted error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_Canceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is DCPT310-88 (1 file(s))\n"), nil
		case "ipptool":
			return []byte(`
job-id (integer) = 88
job-state (enum) = canceled
job-state-reasons (keyword) = job-canceled-by-user
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-canceled",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/canceled.pdf", nil, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_ProcessingStopped_Recovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var pollCount int
	var mu sync.Mutex

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is DCPT310-77 (1 file(s))\n"), nil
		case "ipptool":
			pollCount++
			if pollCount == 1 {
				// 1st poll: out of paper (recoverable pause)
				return []byte(`
job-id (integer) = 77
job-state (enum) = processing-stopped
job-state-reasons (keyword) = media-empty
`), nil
			}
			if pollCount == 2 {
				// 2nd poll: paper refilled, resumed processing
				return []byte(`
job-id (integer) = 77
job-state (enum) = processing
job-state-reasons (keyword) = none
`), nil
			}
			// 3rd poll: finished successfully
			return []byte(`
job-id (integer) = 77
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-recover",
		Copies:      1,
		TotalSheets: 3,
		Duplex:      contracts.DuplexSimplex,
	}

	var completedSheets int
	onPage := func(sheets int) {
		completedSheets = sheets
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/recover.pdf", onPage, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("expected successful recovery and print completion, got %v", err)
	}

	// Without hardware odometer or IPP sheets counter, recovering and completing in CUPS
	// must NOT fabricate full physical sheet progress.
	if completedSheets != 0 {
		t.Fatalf("expected 0 synthetic completed sheets without odometer, got %d", completedSheets)
	}
}

func TestPrintPDFWithCUPS_DisappearedJob_Unknown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is DCPT310-999 (1 file(s))\n"), nil
		case "lpstat":
			// Disappears from queue without appearing in completed
			return []byte(""), nil
		case "ipptool":
			// IPP returns not found
			return []byte("status-code = client-error-not-found (Job #999 does not exist.)"), errors.New("exit status 1")
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-ghost",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	var completedCalled bool
	onPage := func(sheets int) {
		completedCalled = true
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/ghost.pdf", onPage, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "state unknown") {
		t.Fatalf("expected state unknown error, got %v", err)
	}

	if completedCalled {
		t.Fatal("onPage must NEVER be called when job disappears without confirmation")
	}
}

func TestPrintPDFWithCUPS_NoSyntheticHalfwayProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	pollCount := 0
	var sheetsReported []int

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is DCPT310-333 (1 file(s))\n"), nil
		case "ipptool":
			pollCount++
			if pollCount <= 3 {
				// While processing without hardware sheet counter, job-media-sheets-completed is 0
				return []byte(`
job-id (integer) = 333
job-state (enum) = processing
job-state-reasons (keyword) = none
job-media-sheets-completed (integer) = 0
`), nil
			}
			// Finally completes
			return []byte(`
job-id (integer) = 333
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-nosynthetic",
		Copies:      1,
		TotalSheets: 10,
		Duplex:      contracts.DuplexSimplex,
	}

	onPage := func(sheets int) {
		mu.Lock()
		sheetsReported = append(sheetsReported, sheets)
		mu.Unlock()
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/test.pdf", onPage, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Ensure onPage was NOT called with halfway progress (5)
	for _, s := range sheetsReported {
		if s == 5 {
			t.Fatalf("found synthetic halfway progress of 5 sheets in %v", sheetsReported)
		}
	}

	// Without hardware odometer or IPP sheets counter, ensure onPage was NOT called
	// with synthetic halfway progress (5) or synthetic completion (10).
	if len(sheetsReported) != 0 {
		t.Fatalf("expected no synthetic progress reported without odometer, got %v", sheetsReported)
	}
}

func TestPrintPDFWithCUPS_TransientPollErrors_Recovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var lpCalls atomic.Int32
	var pollErrors atomic.Int32

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			lpCalls.Add(1)
			return []byte("request id is DCPT310-444 (1 file(s))\n"), nil
		case "lpstat":
			// First 2 polls fail with transient error
			if pollErrors.Add(1) <= 2 {
				return nil, errors.New("lpstat: temporary socket connection error")
			}
			if len(args) > 1 && args[1] == "completed" {
				return []byte("DCPT310-444 username 1024 Tue Nov 3 20:30:00 2026\n\tAlerts: job-completed-successfully\n\tqueued for DCPT310\n"), nil
			}
			return []byte(""), nil
		default:
			return nil, errors.New("unknown command: " + name)
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-transient",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/transient.pdf", nil, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("expected recovery from transient errors, got %v", err)
	}

	// Crucial: lp must be submitted exactly once, preventing duplicate prints
	if count := lpCalls.Load(); count != 1 {
		t.Fatalf("expected lp to be called exactly 1 time, got %d", count)
	}
}

func TestPrintPDFWithCUPS_CancellationCancelsJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	canceledJobID := ""

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is DCPT310-99 (1 file(s))"), nil
		case "lpstat":
			// Trigger context cancel on first poll
			cancel()
			return []byte("DCPT310-99 still printing"), nil
		case "cancel":
			if len(args) > 0 {
				canceledJobID = args[0]
			}
			return []byte(""), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-cancel",
		Copies:      1,
		TotalSheets: 5,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/test.pdf", nil, 5*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if canceledJobID != "DCPT310-99" {
		t.Fatalf("expected cancel DCPT310-99, got %q", canceledJobID)
	}
}

func TestPollCUPSHealth(t *testing.T) {
	tests := []struct {
		name          string
		queue         string
		output        string
		cmdErr        error
		wantOnline    bool
		wantState     string
		wantPaperTray string
	}{
		{
			name:          "empty queue",
			queue:         "",
			wantOnline:    false,
			wantState:     "ERROR",
			wantPaperTray: "UNKNOWN",
		},
		{
			name:          "command failure",
			queue:         "DCPT310",
			cmdErr:        errors.New("lpstat: cups service down"),
			wantOnline:    false,
			wantState:     "OFFLINE",
			wantPaperTray: "UNKNOWN",
		},
		{
			name:          "queue idle",
			queue:         "DCPT310",
			output:        "printer DCPT310 is idle. enabled since Tue Nov  3 10:00:00 2026",
			wantOnline:    true,
			wantState:     "IDLE",
			wantPaperTray: "OK",
		},
		{
			name:          "queue idle out of paper",
			queue:         "DCPT310",
			output:        "printer DCPT310 is idle. out of paper - please refill",
			wantOnline:    true,
			wantState:     "IDLE",
			wantPaperTray: "EMPTY",
		},
		{
			name:          "queue printing",
			queue:         "DCPT310",
			output:        "printer DCPT310 now printing DCPT310-12. enabled since Tue Nov 3",
			wantOnline:    true,
			wantState:     "PRINTING",
			wantPaperTray: "OK",
		},
		{
			name:          "queue disabled",
			queue:         "DCPT310",
			output:        "printer DCPT310 disabled since Tue Nov 3 - filter failed",
			wantOnline:    false,
			wantState:     "ERROR",
			wantPaperTray: "CHECK_PRINTER",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(tc.output), tc.cmdErr
			}

			h := PollCUPSHealthCustom(context.Background(), runner, tc.queue, 150)
			if h.Online != tc.wantOnline {
				t.Errorf("Online: got %v, want %v", h.Online, tc.wantOnline)
			}
			if h.State != tc.wantState {
				t.Errorf("State: got %q, want %q", h.State, tc.wantState)
			}
			if h.PaperTrayStatus != tc.wantPaperTray {
				t.Errorf("PaperTrayStatus: got %q, want %q", h.PaperTrayStatus, tc.wantPaperTray)
			}
			if h.EstimatedSheets != 150 {
				t.Errorf("EstimatedSheets: got %d, want 150", h.EstimatedSheets)
			}
		})
	}
}

func TestPollCUPSHealth_ExtendedPrinterStateReasons(t *testing.T) {
	t.Run("media_jam_error_reason_reports_error_and_paper_jam", func(t *testing.T) {
		output := "printer HPTank is idle.  enabled since Thu 13 Nov 2026 10:00:00 AM\n\tReady to print.\n\tPrinter state reasons: media-jam-error"
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(output), nil
		}

		h := PollCUPSHealthCustom(context.Background(), runner, "HPTank", 100)
		if h.State != "ERROR" {
			t.Errorf("State: got %q, want ERROR", h.State)
		}
		if !h.PaperJam {
			t.Error("expected PaperJam = true")
		}
		if !strings.Contains(strings.ToLower(h.ErrorMessage), "jam") {
			t.Errorf("expected ErrorMessage containing 'jam', got %q", h.ErrorMessage)
		}
	})

	t.Run("cover_open_error_reason_reports_error_and_cover_open", func(t *testing.T) {
		output := "printer HPTank is idle.  enabled since Thu 13 Nov 2026 10:00:00 AM\n\tPrinter state reasons: cover-open-error"
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(output), nil
		}

		h := PollCUPSHealthCustom(context.Background(), runner, "HPTank", 100)
		if h.State != "ERROR" {
			t.Errorf("State: got %q, want ERROR", h.State)
		}
		if !h.CoverOpen {
			t.Error("expected CoverOpen = true")
		}
	})

	t.Run("media_empty_reason_reports_error_and_paper_tray_empty", func(t *testing.T) {
		output := "printer HPTank is stopped.  enabled since Thu 13 Nov 2026 10:00:00 AM\n\tPrinter state reasons: media-empty-error"
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(output), nil
		}

		h := PollCUPSHealthCustom(context.Background(), runner, "HPTank", 100)
		if h.State != "ERROR" {
			t.Errorf("State: got %q, want ERROR", h.State)
		}
		if h.PaperTrayStatus != "EMPTY" {
			t.Errorf("PaperTrayStatus: got %q, want EMPTY", h.PaperTrayStatus)
		}
	})

	t.Run("output_bin_full_reason_reports_error_and_output_bin_full", func(t *testing.T) {
		output := "printer HPTank is idle.  enabled since Thu 13 Nov 2026 10:00:00 AM\n\tPrinter state reasons: output-bin-full"
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(output), nil
		}

		h := PollCUPSHealthCustom(context.Background(), runner, "HPTank", 100)
		if h.State != "ERROR" {
			t.Errorf("State: got %q, want ERROR", h.State)
		}
		if !h.OutputBinFull {
			t.Error("expected OutputBinFull = true")
		}
	})

	t.Run("fallback_without_l_flag_when_l_fails", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			for _, arg := range args {
				if arg == "-l" {
					return nil, errors.New("lpstat: invalid option -l")
				}
			}
			return []byte("printer HPTank is idle. enabled since Thu Nov 13"), nil
		}

		h := PollCUPSHealthCustom(context.Background(), runner, "HPTank", 100)
		if h.State != "IDLE" {
			t.Errorf("State: got %q, want IDLE via fallback", h.State)
		}
		if !h.Online {
			t.Error("expected Online = true")
		}
	})
}

func TestPrintPDFWithCUPSHooksCustom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:       "job-hook-test",
		Copies:      1,
		TotalPages:  1,
		TotalSheets: 1,
		Duplex:      contracts.DuplexSimplex,
	}

	var submittingCalled bool
	var submittedJobID string

	hooks := &CUPSPrintHooks{
		OnSubmitting: func() {
			submittingCalled = true
		},
		OnSubmitted: func(cupsJobID string) {
			submittedJobID = cupsJobID
		},
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			return []byte("request id is DCPT310-777 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("DCPT310-777 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully"), nil
				}
			}
		}
		return []byte(""), nil
	}

	err := PrintPDFWithCUPSHooksCustom(ctx, runner, "DCPT310", cmd, "/tmp/dummy.pdf", nil, 10*time.Millisecond, hooks)
	if err != nil {
		t.Fatalf("PrintPDFWithCUPSHooksCustom failed: %v", err)
	}

	if !submittingCalled {
		t.Error("expected OnSubmitting hook to be called before lp submission")
	}
	if submittedJobID != "DCPT310-777" {
		t.Errorf("expected OnSubmitted hook with DCPT310-777, got %q", submittedJobID)
	}
}

func TestMonitorCUPSJobCustom_ActiveToCompleted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:       "job-mon-test",
		Copies:      1,
		TotalPages:  2,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	pollCount := 0
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			pollCount++
			if pollCount == 1 {
				// First poll: job still in not-completed
				return []byte("DCPT310-888 user 1024 Tue Nov 3 14:00:00 2026"), nil
			}
			// Second poll: job completed
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("DCPT310-888 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully"), nil
				}
			}
		}
		return []byte(""), nil
	}

	err := MonitorCUPSJobCustom(ctx, runner, "DCPT310", cmd, "DCPT310-888", nil, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("MonitorCUPSJobCustom failed: %v", err)
	}
	if pollCount < 2 {
		t.Errorf("expected at least 2 polls, got %d", pollCount)
	}
}

func TestExtractHostFromCUPSDeviceURI(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "standard IPP URI with port and path",
			input:    "ipp://10.42.0.210:631/ipp/print",
			expected: "10.42.0.210",
		},
		{
			name:     "secure IPPS URI with hostname",
			input:    "ipps://print-server.lan:443/printers/LaserJet",
			expected: "print-server.lan",
		},
		{
			name:     "raw socket port 9100",
			input:    "socket://192.168.1.150:9100",
			expected: "192.168.1.150",
		},
		{
			name:     "lpd URI",
			input:    "lpd://10.0.0.5/queue",
			expected: "10.0.0.5",
		},
		{
			name:     "http URI",
			input:    "http://10.42.0.210:631/ipp",
			expected: "10.42.0.210",
		},
		{
			name:     "lpstat output line with prefix",
			input:    "device for HPTank: ipp://10.42.0.210:631/ipp/print",
			expected: "10.42.0.210",
		},
		{
			name:     "local USB URI returns empty",
			input:    "usb://HP/LaserJet%20Tank%201020w?serial=VNC3J00928",
			expected: "",
		},
		{
			name:     "file or pipe URI returns empty",
			input:    "file:///dev/null",
			expected: "",
		},
		{
			name:     "empty string returns empty",
			input:    "",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractHostFromCUPSDeviceURI(tc.input)
			if got != tc.expected {
				t.Errorf("ExtractHostFromCUPSDeviceURI(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestDiscoverQueueHost(t *testing.T) {
	ctx := context.Background()

	t.Run("successful discovery from lpstat -v", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" && len(args) == 2 && args[0] == "-v" && args[1] == "HPTank" {
				return []byte("device for HPTank: ipp://10.42.0.210:631/ipp/print\n"), nil
			}
			return nil, errors.New("unexpected command")
		}

		host := DiscoverQueueHost(ctx, runner, "HPTank")
		if host != "10.42.0.210" {
			t.Errorf("DiscoverQueueHost = %q, want 10.42.0.210", host)
		}
	})

	t.Run("runner failure returns empty", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("lpstat failed")
		}

		host := DiscoverQueueHost(ctx, runner, "HPTank")
		if host != "" {
			t.Errorf("DiscoverQueueHost = %q, want empty", host)
		}
	})

	t.Run("USB printer returns empty", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("device for Deskjet: usb://HP/Deskjet%202700?serial=CN123\n"), nil
		}

		host := DiscoverQueueHost(ctx, runner, "Deskjet")
		if host != "" {
			t.Errorf("DiscoverQueueHost = %q, want empty", host)
		}
	})
}

func TestMonitorCUPSJobWithTelemetryCustom_PaperJam(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:       "job-jam-test",
		Copies:      1,
		TotalPages:  9,
		TotalSheets: 9,
		Duplex:      contracts.DuplexSimplex,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			// CUPS reports processing
			return []byte("HPTank-101 user 1024 Tue Nov 3 14:00:00 2026"), nil
		}
		return []byte(""), nil
	}

	// Telemetry poller reporting paper jam (bit 0x04)
	poller := func(ctx context.Context) HardwareTelemetry {
		return HardwareTelemetry{
			Online:   true,
			HasFlags: true,
			Flags: PrinterErrorFlags{
				Jammed: true,
			},
		}
	}

	err := MonitorCUPSJobWithTelemetryCustom(ctx, runner, "HPTank", cmd, "HPTank-101", nil, 10*time.Millisecond, poller)
	if err == nil {
		t.Fatal("expected error from paper jam, got nil")
	}
	if !strings.Contains(err.Error(), "paper feed jam detected") {
		t.Errorf("expected error containing 'paper feed jam detected', got %v", err)
	}
}

func TestMonitorCUPSJobWithTelemetryCustom_CoverOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:       "job-cover-test",
		Copies:      1,
		TotalPages:  5,
		TotalSheets: 5,
		Duplex:      contracts.DuplexSimplex,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("HPTank-102 user 1024 Tue Nov 3 14:00:00 2026"), nil
	}

	poller := func(ctx context.Context) HardwareTelemetry {
		return HardwareTelemetry{
			Online:   true,
			HasFlags: true,
			Flags: PrinterErrorFlags{
				DoorOpen: true,
			},
		}
	}

	err := MonitorCUPSJobWithTelemetryCustom(ctx, runner, "HPTank", cmd, "HPTank-102", nil, 10*time.Millisecond, poller)
	if err == nil {
		t.Fatal("expected error from cover open, got nil")
	}
	if !strings.Contains(err.Error(), "printer cover is open") {
		t.Errorf("expected error containing 'printer cover is open', got %v", err)
	}
}

func TestMonitorCUPSJobWithTelemetryCustom_OutOfPaper(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:       "job-empty-test",
		Copies:      1,
		TotalPages:  5,
		TotalSheets: 5,
		Duplex:      contracts.DuplexSimplex,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("HPTank-103 user 1024 Tue Nov 3 14:00:00 2026"), nil
	}

	poller := func(ctx context.Context) HardwareTelemetry {
		return HardwareTelemetry{
			Online:   true,
			HasFlags: true,
			Flags: PrinterErrorFlags{
				NoPaper: true,
			},
		}
	}

	err := MonitorCUPSJobWithTelemetryCustom(ctx, runner, "HPTank", cmd, "HPTank-103", nil, 10*time.Millisecond, poller)
	if err == nil {
		t.Fatal("expected error from out of paper, got nil")
	}
	if !strings.Contains(err.Error(), "printer paper tray is empty") {
		t.Errorf("expected error containing 'printer paper tray is empty', got %v", err)
	}
}

func TestMonitorCUPSJobWithTelemetryCustom_OdometerTracking(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:       "job-odometer-test",
		Copies:      1,
		TotalPages:  3,
		TotalSheets: 3,
		Duplex:      contracts.DuplexSimplex,
	}

	var pollCount int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			c := atomic.AddInt32(&pollCount, 1)
			if c <= 3 {
				// While active
				for i, arg := range args {
					if arg == "-W" && i+1 < len(args) && args[i+1] == "not-completed" {
						return []byte("HPTank-104 user 1024 Tue Nov 3 14:00:00 2026"), nil
					}
				}
				return []byte("HPTank-104 user 1024 Tue Nov 3 14:00:00 2026"), nil
			}
			// When completed (not in not-completed anymore)
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-104 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully"), nil
				}
			}
			return []byte(""), nil
		}
		return []byte(""), nil
	}

	var odoCalls int32
	poller := func(ctx context.Context) HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		// Baseline life is sampled on first call (return 1000)
		// Subsequent calls increment: 1001, 1002, 1003
		var life int64 = 1000
		if c > 1 {
			life += int64(c - 1)
		}
		return HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	var reportedSheets []int
	var sheetsMu sync.Mutex
	onPage := func(sheets int) {
		sheetsMu.Lock()
		defer sheetsMu.Unlock()
		reportedSheets = append(reportedSheets, sheets)
	}

	err := MonitorCUPSJobWithTelemetryCustom(ctx, runner, "HPTank", cmd, "HPTank-104", onPage, 10*time.Millisecond, poller)
	if err != nil {
		t.Fatalf("MonitorCUPSJobWithTelemetryCustom failed: %v", err)
	}

	sheetsMu.Lock()
	defer sheetsMu.Unlock()
	if len(reportedSheets) == 0 {
		t.Fatal("expected sheets to be reported via odometer delta, got none")
	}
	// Last reported should reach total sheets
	last := reportedSheets[len(reportedSheets)-1]
	if last != 3 {
		t.Errorf("last reported sheet = %d, want 3", last)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"LocalHost", true},
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"127.1.2.3", true},
		{"127.255.255.255", true},
		{"::1", true},
		{"10.42.0.210", false},
		{"192.168.99.2", false},
		{"192.168.1.50", false},
		{"printer.example.com", false},
		{"", false},
	}
	for _, tc := range tests {
		got := IsLoopbackHost(tc.host)
		if got != tc.want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestResolveSNMPTarget(t *testing.T) {
	ctx := context.Background()

	t.Run("CUPS XeroxGoSim loopback device URI yields no automatic SNMP target", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" && len(args) == 2 && args[0] == "-v" && args[1] == "XeroxGoSim" {
				return []byte("device for XeroxGoSim: ipp://localhost:8631/ipp/print\n"), nil
			}
			return nil, errors.New("unexpected command")
		}

		// Even when printerAddr defaults to 192.168.99.2:9100, do not fall back!
		target := ResolveSNMPTarget(ctx, runner, "", "XeroxGoSim", "192.168.99.2:9100")
		if target != "" {
			t.Errorf("ResolveSNMPTarget for XeroxGoSim loopback = %q, want empty", target)
		}
	})

	t.Run("CUPS HPTank yields discovered host 10.42.0.210", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" && len(args) == 2 && args[0] == "-v" && args[1] == "HPTank" {
				return []byte("device for HPTank: ipp://10.42.0.210:631/ipp/print\n"), nil
			}
			return nil, errors.New("unexpected command")
		}

		target := ResolveSNMPTarget(ctx, runner, "", "HPTank", "192.168.99.2:9100")
		if target != "10.42.0.210" {
			t.Errorf("ResolveSNMPTarget for HPTank = %q, want 10.42.0.210", target)
		}
	})

	t.Run("explicit SNMP target overrides CUPS discovery", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("device for HPTank: ipp://10.42.0.210:631/ipp/print\n"), nil
		}

		// Explicit 127.0.0.1 wins
		target1 := ResolveSNMPTarget(ctx, runner, "127.0.0.1", "HPTank", "192.168.99.2:9100")
		if target1 != "127.0.0.1" {
			t.Errorf("ResolveSNMPTarget with explicit 127.0.0.1 = %q, want 127.0.0.1", target1)
		}

		// Explicit 10.0.0.99 wins
		target2 := ResolveSNMPTarget(ctx, runner, "10.0.0.99", "HPTank", "192.168.99.2:9100")
		if target2 != "10.0.0.99" {
			t.Errorf("ResolveSNMPTarget with explicit 10.0.0.99 = %q, want 10.0.0.99", target2)
		}
	})

	t.Run("failed CUPS discovery does not use 192.168.99.2", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("lpstat error: queue not found")
		}

		target := ResolveSNMPTarget(ctx, runner, "", "BrokenQueue", "192.168.99.2:9100")
		if target != "" {
			t.Errorf("ResolveSNMPTarget on discovery failure = %q, want empty (must not use 192.168.99.2)", target)
		}
	})

	t.Run("raw TCP/PJL mode preserves current target behavior", func(t *testing.T) {
		// Non-loopback printer address returns host
		target1 := ResolveSNMPTarget(ctx, nil, "", "", "192.168.1.50:9100")
		if target1 != "192.168.1.50" {
			t.Errorf("ResolveSNMPTarget raw TCP non-loopback = %q, want 192.168.1.50", target1)
		}

		// Loopback printer address returns empty (ignores loopback candidate)
		target2 := ResolveSNMPTarget(ctx, nil, "", "", "127.0.0.1:9100")
		if target2 != "" {
			t.Errorf("ResolveSNMPTarget raw TCP loopback 127.0.0.1 = %q, want empty", target2)
		}

		target3 := ResolveSNMPTarget(ctx, nil, "", "", "localhost:9100")
		if target3 != "" {
			t.Errorf("ResolveSNMPTarget raw TCP loopback localhost = %q, want empty", target3)
		}

		// Empty printer address returns empty
		target4 := ResolveSNMPTarget(ctx, nil, "", "", "")
		if target4 != "" {
			t.Errorf("ResolveSNMPTarget raw TCP empty = %q, want empty", target4)
		}
	})
}
