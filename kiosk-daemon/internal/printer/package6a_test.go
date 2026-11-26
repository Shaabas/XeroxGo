package printer

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestPackage6A_PreSubmissionOrderingAndExplicitCUPSOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:       "job-6a-pre",
		KioskID:     "kiosk-6a",
		TotalPages:  2,
		TotalSheets: 6,
		Copies:      3,
		Duplex:      contracts.DuplexSimplex,
		ColorMode:   "BW",
	}

	var events []string
	var recordedLP string
	var pollerCalls atomic.Int32

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			events = append(events, "lp")
			recordedLP = name + " " + strings.Join(args, " ")
			return []byte("request id is HPTank-601 (1 file(s))\n"), nil
		}
		if name == "lpstat" {
			for i, a := range args {
				if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-601 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
				}
			}
		}
		return []byte(""), nil
	}

	poller := func(ctx context.Context) HardwareTelemetry {
		c := pollerCalls.Add(1)
		if c == 1 {
			events = append(events, "baseline_poll")
			return HardwareTelemetry{
				Online:         true,
				HasLife:        true,
				LifeCount:      2000,
				CounterIndex:   "1.1",
				CounterUnit:    7,
				HasCounterMeta: true,
				IsPageCounter:  true,
				StatusState:    "IDLE",
				ObservedAt:     time.Now().UTC(),
			}
		}
		// Fast job: already produced all 6 impressions immediately after lp
		return HardwareTelemetry{
			Online:         true,
			HasLife:        true,
			LifeCount:      2006,
			CounterIndex:   "1.1",
			CounterUnit:    7,
			HasCounterMeta: true,
			IsPageCounter:  true,
			StatusState:    "IDLE",
			ObservedAt:     time.Now().UTC(),
		}
	}

	var persistedBaseline CounterBaseline
	var reportedSheets []int
	hooks := &CUPSPrintHooks{
		OnSubmittingWithBaseline: func(b CounterBaseline) error {
			events = append(events, "persist_baseline")
			persistedBaseline = b
			return nil
		},
		OnSubmitted: func(cupsJobID string) error {
			events = append(events, "persist_cups_id:"+cupsJobID)
			return nil
		},
		TelemetryPoller: poller,
	}

	err := PrintPDFWithCUPSHooksCustom(ctx, runner, "HPTank", cmd, "/tmp/doc.pdf", func(s int) {
		reportedSheets = append(reportedSheets, s)
	}, 10*time.Millisecond, hooks)
	if err != nil {
		t.Fatalf("PrintPDFWithCUPSHooksCustom failed: %v", err)
	}

	// Verify exact ordering: baseline_poll -> persist_baseline -> lp -> persist_cups_id
	wantPrefix := []string{"baseline_poll", "persist_baseline", "lp", "persist_cups_id:HPTank-601"}
	if len(events) < len(wantPrefix) {
		t.Fatalf("expected events %v, got %v", wantPrefix, events)
	}
	for i, w := range wantPrefix {
		if events[i] != w {
			t.Fatalf("event[%d] = %q, want %q (all=%v)", i, events[i], w, events)
		}
	}
	if !persistedBaseline.HasBaseline || persistedBaseline.Value != 2000 || persistedBaseline.Unit != 7 || persistedBaseline.Index != "1.1" {
		t.Fatalf("unexpected persisted baseline: %+v", persistedBaseline)
	}
	for _, opt := range []string{"-n 3", "-o sides=one-sided", "-o print-color-mode=monochrome", "-o media=A4"} {
		if !strings.Contains(recordedLP, opt) {
			t.Fatalf("expected lp args to contain %q, got %q", opt, recordedLP)
		}
	}
	if len(reportedSheets) == 0 || reportedSheets[len(reportedSheets)-1] != 6 {
		t.Fatalf("expected multi-copy fast job to attribute 6 sheets, got %v", reportedSheets)
	}
}

func TestPackage6A_BaselineOrPersistenceFailurePreventsSubmission(t *testing.T) {
	ctx := context.Background()
	cmd := contracts.PrintJobCommand{JobID: "job-6a-nosub", Copies: 1, TotalPages: 1, TotalSheets: 1, Duplex: contracts.DuplexSimplex}

	t.Run("missing_baseline_prevents_lp", func(t *testing.T) {
		lpCalled := false
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpCalled = true
			}
			return nil, nil
		}
		hooks := &CUPSPrintHooks{
			TelemetryPoller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: false}
			},
		}
		err := PrintPDFWithCUPSHooksCustom(ctx, runner, "HPTank", cmd, "/tmp/a.pdf", nil, 10*time.Millisecond, hooks)
		if err == nil || lpCalled {
			t.Fatalf("expected error and lp NOT called when baseline missing, err=%v lpCalled=%v", err, lpCalled)
		}
	})

	t.Run("unsupported_counter_unit_prevents_lp", func(t *testing.T) {
		lpCalled := false
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpCalled = true
			}
			return nil, nil
		}
		hooks := &CUPSPrintHooks{
			TelemetryPoller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterUnit: 11, CounterUnitName: "hours", HasCounterMeta: true}
			},
		}
		err := PrintPDFWithCUPSHooksCustom(ctx, runner, "HPTank", cmd, "/tmp/a.pdf", nil, 10*time.Millisecond, hooks)
		if err == nil || lpCalled {
			t.Fatalf("expected error and lp NOT called on unsupported counter unit, err=%v lpCalled=%v", err, lpCalled)
		}
	})

	t.Run("persistence_failure_prevents_lp", func(t *testing.T) {
		lpCalled := false
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpCalled = true
			}
			return nil, nil
		}
		hooks := &CUPSPrintHooks{
			TelemetryPoller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterUnit: 7, HasCounterMeta: true}
			},
			OnSubmittingWithBaseline: func(b CounterBaseline) error {
				return errors.New("sqlite disk full")
			},
		}
		err := PrintPDFWithCUPSHooksCustom(ctx, runner, "HPTank", cmd, "/tmp/a.pdf", nil, 10*time.Millisecond, hooks)
		if err == nil || lpCalled {
			t.Fatalf("expected error and lp NOT called on persistence failure, err=%v lpCalled=%v", err, lpCalled)
		}
	})
}

func TestPackage6A_CounterAnomaliesAndCombinedCompletionPolicy(t *testing.T) {
	cmd := contracts.PrintJobCommand{JobID: "job-6a-anom", Copies: 1, TotalPages: 2, TotalSheets: 2, Duplex: contracts.DuplexSimplex}
	baseline := CounterBaseline{HasBaseline: true, Value: 100, Index: "1.1", Unit: 7, ObservedAt: time.Now().UTC()}

	completedRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			for i, a := range args {
				if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-701 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
				}
			}
		}
		return []byte(""), nil
	}

	cases := []struct {
		name      string
		runner    CommandRunner
		poller    func(context.Context) HardwareTelemetry
		wantErr   string
	}{
		{
			name:   "counter_decrease_reset",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 99, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, StatusState: "IDLE"}
			},
			wantErr: "decreased/reset",
		},
		{
			name:   "unexpected_counter_increase",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 105, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, StatusState: "IDLE"}
			},
			wantErr: "exceeds expected",
		},
		{
			name:   "counter_identity_changed",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.2", CounterUnit: 7, HasCounterMeta: true, StatusState: "IDLE"}
			},
			wantErr: "identity changed",
		},
		{
			name:   "cups_completed_but_impressions_missing",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 101, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, StatusState: "IDLE"}
			},
			wantErr: "physical output incomplete",
		},
		{
			name:   "cups_completed_but_not_quiescent",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, StatusState: "PRINTING"}
			},
			wantErr: "not quiescent",
		},
		{
			name:   "impressions_reached_with_active_fault",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, StatusState: "ERROR", HasFlags: true, Flags: PrinterErrorFlags{Jammed: true}}
			},
			wantErr: "jam",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := MonitorCUPSJobWithTelemetryBaselineCustom(ctx, tc.runner, "HPTank", cmd, "HPTank-701", nil, 10*time.Millisecond, tc.poller, baseline)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}

	t.Run("impressions_reached_while_cups_still_processing_waits_for_cups", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var polls atomic.Int32
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" {
				c := polls.Add(1)
				if c <= 4 {
					for i, a := range args {
						if a == "-W" && i+1 < len(args) && args[i+1] == "not-completed" {
							return []byte("HPTank-701 user 1024 Tue Nov 3 14:00:00 2026\n"), nil
						}
					}
					return []byte("printer HPTank now printing HPTank-701. enabled\n"), nil
				}
				for i, a := range args {
					if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
						return []byte("HPTank-701 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
					}
				}
			}
			return []byte(""), nil
		}
		poller := func(ctx context.Context) HardwareTelemetry {
			return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, StatusState: "IDLE"}
		}
		err := MonitorCUPSJobWithTelemetryBaselineCustom(ctx, runner, "HPTank", cmd, "HPTank-701", nil, 10*time.Millisecond, poller, baseline)
		if err != nil {
			t.Fatalf("expected clean completion after CUPS transitioned from processing to completed, got %v", err)
		}
		if polls.Load() < 5 {
			t.Fatalf("expected monitor to wait for CUPS completion rather than exiting early, polls=%d", polls.Load())
		}
	})
}
