package printer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseIPPPrinterState_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		queue     string
		output    string
		checkRes  func(t *testing.T, res IPPPrinterStateResult)
	}{
		{
			name:  "idle printer accepting jobs with none reason",
			queue: "DCPT310",
			output: `
status-code = successful-ok
printer-state (enum) = idle
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = none
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.Source != CapabilitySourceCUPSQueue {
					t.Errorf("expected Source %q, got %q", CapabilitySourceCUPSQueue, res.Source)
				}
				if res.Queue != "DCPT310" {
					t.Errorf("expected Queue DCPT310, got %q", res.Queue)
				}
				if res.State != IPPPrinterStateIdle {
					t.Errorf("expected State idle, got %v", res.State)
				}
				if res.RawState != "idle" {
					t.Errorf("expected RawState idle, got %q", res.RawState)
				}
				if res.AcceptingJobs != AcceptingJobsAccepting {
					t.Errorf("expected AcceptingJobs ACCEPTING, got %v", res.AcceptingJobs)
				}
				if len(res.ParsedReasons) != 0 {
					t.Errorf("expected 0 parsed reasons for 'none', got %d", len(res.ParsedReasons))
				}
				if res.HasHardwareFault {
					t.Errorf("expected HasHardwareFault false, got true")
				}
				if res.PaperJam || res.MediaEmpty || res.CoverOpen || res.OutputBinFull {
					t.Errorf("expected all fault flags false")
				}
			},
		},
		{
			name:  "numeric enum 3 maps to idle",
			queue: "Enum3Queue",
			output: `
status-code = successful-ok
printer-state (enum) = 3
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = none
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.State != IPPPrinterStateIdle {
					t.Errorf("expected State idle for enum 3, got %v", res.State)
				}
				if res.RawState != "3" {
					t.Errorf("expected RawState 3, got %q", res.RawState)
				}
			},
		},
		{
			name:  "processing printer with numeric enum 4",
			queue: "ActiveQueue",
			output: `
status-code = successful-ok
printer-state (enum) = 4
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = none
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.State != IPPPrinterStateProcessing {
					t.Errorf("expected State processing for enum 4, got %v", res.State)
				}
				if res.RawState != "4" {
					t.Errorf("expected RawState 4, got %q", res.RawState)
				}
				if res.AcceptingJobs != AcceptingJobsAccepting {
					t.Errorf("expected AcceptingJobs ACCEPTING, got %v", res.AcceptingJobs)
				}
			},
		},
		{
			name:  "stopped printer with enum 5 and media jam error",
			queue: "JammedQueue",
			output: `
status-code = successful-ok
printer-state (enum) = 5
printer-is-accepting-jobs (boolean) = false
printer-state-reasons (keyword) = media-jam-error
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.State != IPPPrinterStateStopped {
					t.Errorf("expected State stopped for enum 5, got %v", res.State)
				}
				if res.AcceptingJobs != AcceptingJobsNotAccepting {
					t.Errorf("expected AcceptingJobs NOT_ACCEPTING, got %v", res.AcceptingJobs)
				}
				if !res.PaperJam {
					t.Errorf("expected PaperJam true")
				}
				if !res.HasHardwareFault {
					t.Errorf("expected HasHardwareFault true")
				}
				if len(res.ParsedReasons) != 1 {
					t.Fatalf("expected 1 parsed reason, got %d", len(res.ParsedReasons))
				}
				if res.ParsedReasons[0].Keyword != "media-jam" || res.ParsedReasons[0].Severity != SeverityError {
					t.Errorf("unexpected parsed reason: %+v", res.ParsedReasons[0])
				}
			},
		},
		{
			name:  "multiple reasons with various severity suffixes",
			queue: "MultiFaultQueue",
			output: `
status-code = successful-ok
printer-state (enum) = stopped
printer-is-accepting-jobs (boolean) = false
printer-state-reasons (keyword) = media-jam-error,door-open-warning,marker-supply-low-warning,output-area-full-error
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if !res.PaperJam {
					t.Errorf("expected PaperJam true")
				}
				if !res.CoverOpen {
					t.Errorf("expected CoverOpen true")
				}
				if !res.MarkerLow {
					t.Errorf("expected MarkerLow true")
				}
				if !res.OutputBinFull {
					t.Errorf("expected OutputBinFull true")
				}
				if !res.HasHardwareFault {
					t.Errorf("expected HasHardwareFault true")
				}
				if len(res.ParsedReasons) != 4 {
					t.Fatalf("expected 4 parsed reasons, got %d", len(res.ParsedReasons))
				}
			},
		},
		{
			name:  "media-empty and marker-supply-empty errors",
			queue: "SuppliesQueue",
			output: `
status-code = successful-ok
printer-state (enum) = stopped
printer-is-accepting-jobs (boolean) = false
printer-state-reasons (keyword) = media-empty-error,toner-empty-error
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if !res.MediaEmpty {
					t.Errorf("expected MediaEmpty true")
				}
				if !res.MarkerEmpty {
					t.Errorf("expected MarkerEmpty true")
				}
				if !res.HasHardwareFault {
					t.Errorf("expected HasHardwareFault true")
				}
			},
		},
		{
			name:  "report severity suffix does not flag blocking hardware fault",
			queue: "ReportQueue",
			output: `
status-code = successful-ok
printer-state (enum) = idle
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = media-jam-report,toner-low-warning
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if !res.PaperJam {
					t.Errorf("expected PaperJam true for media-jam-report")
				}
				if !res.MarkerLow {
					t.Errorf("expected MarkerLow true for toner-low-warning")
				}
				if res.HasHardwareFault {
					t.Errorf("expected HasHardwareFault false when reason is only report/warning")
				}
			},
		},
		{
			name:  "exact keyword matching avoids substring collisions",
			queue: "SubstringSafetyQueue",
			output: `
status-code = successful-ok
printer-state (enum) = idle
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = recovery-mode-report,pyjama-party,jam-session,cover-song
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.PaperJam {
					t.Errorf("PaperJam must NOT match pyjama-party or jam-session")
				}
				if res.CoverOpen {
					t.Errorf("CoverOpen must NOT match cover-song or recovery-mode")
				}
				if res.HasHardwareFault {
					t.Errorf("HasHardwareFault must be false")
				}
			},
		},
		{
			name:  "missing attributes returns unknown state and accepting",
			queue: "MissingAttrsQueue",
			output: `
status-code = successful-ok
printer-name (nameWithoutLanguage) = MissingAttrsQueue
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.State != IPPPrinterStateUnknown {
					t.Errorf("expected State unknown, got %v", res.State)
				}
				if res.AcceptingJobs != AcceptingJobsUnknown {
					t.Errorf("expected AcceptingJobs UNKNOWN, got %v", res.AcceptingJobs)
				}
				if res.HasRecognizedStateAttributes() {
					t.Errorf("expected HasRecognizedStateAttributes false")
				}
			},
		},
		{
			name:  "empty output",
			queue: "EmptyQueue",
			output: "",
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.State != IPPPrinterStateUnknown {
					t.Errorf("expected State unknown, got %v", res.State)
				}
				if res.AcceptingJobs != AcceptingJobsUnknown {
					t.Errorf("expected AcceptingJobs UNKNOWN, got %v", res.AcceptingJobs)
				}
				if res.HasRecognizedStateAttributes() {
					t.Errorf("expected HasRecognizedStateAttributes false")
				}
			},
		},
		{
			name:  "malformed output handled without crashing",
			queue: "MalformedQueue",
			output: `
some random header
= missing key
printer-state = 
printer-is-accepting-jobs = unknown-boolean
printer-state-reasons = ,,,
`,
			checkRes: func(t *testing.T, res IPPPrinterStateResult) {
				if res.State != IPPPrinterStateUnknown {
					t.Errorf("expected State unknown, got %v", res.State)
				}
				if res.AcceptingJobs != AcceptingJobsUnknown {
					t.Errorf("expected AcceptingJobs UNKNOWN, got %v", res.AcceptingJobs)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ParseIPPPrinterState([]byte(tt.output), tt.queue)
			tt.checkRes(t, res)
		})
	}
}

func TestQueryIPPPrinterState_RunnerCases(t *testing.T) {
	t.Run("empty queue name returns error", func(t *testing.T) {
		res, err := QueryIPPPrinterState(context.Background(), nil, "   ")
		if err == nil {
			t.Fatal("expected error for empty queue name")
		}
		if res.QueueAccessible {
			t.Errorf("expected QueueAccessible false")
		}
	})

	t.Run("empty output returns error", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("   \n\t  "), nil
		}

		res, err := QueryIPPPrinterState(context.Background(), runner, "EmptyQueue")
		if err == nil {
			t.Fatal("expected error when output lacks recognized state attributes")
		}
		if res.QueueAccessible {
			t.Errorf("expected QueueAccessible false")
		}
	})

	t.Run("command failure propagates error and preserves partial diagnostics", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			output := `
printer-state (enum) = stopped
printer-state-reasons (keyword) = media-jam-error
`
			return []byte(output), errors.New("exit status 1")
		}

		res, err := QueryIPPPrinterState(context.Background(), runner, "FailedCommandQueue")
		if err == nil {
			t.Fatal("expected runner error to be returned")
		}
		if res.QueueAccessible {
			t.Errorf("expected QueueAccessible false on command failure")
		}
		// Partial diagnostics are preserved:
		if res.State != IPPPrinterStateStopped {
			t.Errorf("expected State stopped preserved in partial diagnostics, got %v", res.State)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam true preserved in partial diagnostics")
		}
		if res.Error == "" {
			t.Errorf("expected Error field populated")
		}
	})

	t.Run("explicit IPP client-error status returns error", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			output := `
status-code = client-error-not-found (The printer or class was not found.)
printer-state (enum) = unknown
`
			return []byte(output), nil
		}

		res, err := QueryIPPPrinterState(context.Background(), runner, "GhostQueue")
		if err == nil {
			t.Fatal("expected error for explicit IPP error status")
		}
		if res.QueueAccessible {
			t.Errorf("expected QueueAccessible false")
		}
		if !strings.Contains(err.Error(), "client-error-not-found") {
			t.Errorf("expected error mentioning status code, got: %v", err)
		}
	})

	t.Run("context cancellation and timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
				return []byte("printer-state (enum) = idle"), nil
			}
		}

		res, err := QueryIPPPrinterState(ctx, runner, "HangingQueue")
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if res.QueueAccessible {
			t.Errorf("expected QueueAccessible false on timeout")
		}
	})

	t.Run("successful query constructs correct URI and sets QueueAccessible", func(t *testing.T) {
		var invokedName string
		var invokedArgs []string

		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			invokedName = name
			invokedArgs = args
			return []byte(`
status-code = successful-ok
printer-state (enum) = idle
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = none
`), nil
		}

		res, err := QueryIPPPrinterState(context.Background(), runner, "ValidQueue")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if invokedName != "ipptool" {
			t.Errorf("expected binary ipptool, got %q", invokedName)
		}
		expectedURI := "ipp://localhost:631/printers/ValidQueue"
		if len(invokedArgs) < 3 || invokedArgs[0] != "-tv" || invokedArgs[1] != expectedURI || invokedArgs[2] != "get-printer-attributes.test" {
			t.Errorf("unexpected arguments: %v", invokedArgs)
		}

		if !res.QueueAccessible {
			t.Errorf("expected QueueAccessible true")
		}
		if res.State != IPPPrinterStateIdle {
			t.Errorf("expected State idle, got %v", res.State)
		}
		if res.AcceptingJobs != AcceptingJobsAccepting {
			t.Errorf("expected AcceptingJobs ACCEPTING, got %v", res.AcceptingJobs)
		}
	})
}
