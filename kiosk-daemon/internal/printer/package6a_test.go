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
				HasFlags:       true,
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
			HasFlags:       true,
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
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 99, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
			},
			wantErr: "decreased/reset",
		},
		{
			name:   "unexpected_counter_increase",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 105, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
			},
			wantErr: "exceeds expected",
		},
		{
			name:   "counter_identity_changed",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.2", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
			},
			wantErr: "identity changed",
		},
		{
			name:   "cups_completed_but_impressions_missing",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 101, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
			},
			wantErr: "physical output incomplete",
		},
		{
			name:   "cups_completed_but_not_quiescent",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "PRINTING", ObservedAt: time.Now().UTC()}
			},
			wantErr: "not quiescent",
		},
		{
			name:   "impressions_reached_with_active_fault",
			runner: completedRunner,
			poller: func(ctx context.Context) HardwareTelemetry {
				return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "ERROR", Flags: PrinterErrorFlags{Jammed: true}, ObservedAt: time.Now().UTC()}
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
			return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
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

func TestPackage6A_OwnershipVerification_DefaultRunnerAndFailClosed(t *testing.T) {
	t.Run("nil_runner_resolves_to_default_and_never_skips", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := VerifyNoUnrelatedActiveCUPSJob(ctx, nil, "NonExistentQueue_Pkg6A_Test", "")
		if err == nil {
			// On a machine with CUPS running and NonExistentQueue_Pkg6A_Test somehow not failing lpstat,
			// lpstat -W not-completed -o <queue> returns non-zero "lpstat: Unknown destination".
			t.Fatalf("expected nil runner to execute real defaultCommandRunner and fail on unknown queue, got nil")
		}
	})

	cmd := contracts.PrintJobCommand{
		JobID: "job-6a-own", OrderID: "ord-6a-own", KioskID: "kiosk-6a",
		TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex,
	}
	validPoller := func(ctx context.Context) HardwareTelemetry {
		return HardwareTelemetry{
			Online: true, HasLife: true, LifeCount: 100, CounterIndex: "1.1",
			CounterUnit: 8, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC(),
		}
	}

	t.Run("ownership_query_error_prevents_lp", func(t *testing.T) {
		var lpCalled atomic.Bool
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpCalled.Store(true)
			}
			if name == "lpstat" {
				return nil, context.DeadlineExceeded
			}
			return []byte(""), nil
		}
		err := PrintPDFWithCUPSHooksCustom(context.Background(), runner, "HPTank", cmd, "/tmp/a.pdf", nil, 10*time.Millisecond, &CUPSPrintHooks{TelemetryPoller: validPoller})
		if err == nil || !strings.Contains(err.Error(), "ownership check failed") {
			t.Fatalf("expected ownership check error, got %v", err)
		}
		if lpCalled.Load() {
			t.Fatal("lp must NOT be called when ownership query fails")
		}
	})

	t.Run("unrelated_active_job_prevents_lp", func(t *testing.T) {
		var lpCalled atomic.Bool
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpCalled.Store(true)
			}
			if name == "lpstat" {
				return []byte("HPTank-999 otheruser 1024 Tue Nov 3 14:00:00 2026\n"), nil
			}
			return []byte(""), nil
		}
		err := PrintPDFWithCUPSHooksCustom(context.Background(), runner, "HPTank", cmd, "/tmp/a.pdf", nil, 10*time.Millisecond, &CUPSPrintHooks{TelemetryPoller: validPoller})
		if err == nil || !strings.Contains(err.Error(), "unrelated active CUPS job") {
			t.Fatalf("expected unrelated active CUPS job error, got %v", err)
		}
		if lpCalled.Load() {
			t.Fatal("lp must NOT be called when unrelated active job is in queue")
		}
	})
}

func TestPackage6A_MissingPollerOrInvalidCounterMetadataPreventsSubmission(t *testing.T) {
	cmd := contracts.PrintJobCommand{
		JobID: "job-6a-meta", OrderID: "ord-6a-meta", KioskID: "kiosk-6a",
		TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex,
	}

	t.Run("missing_telemetry_poller_prevents_lp", func(t *testing.T) {
		var lpCalled atomic.Bool
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpCalled.Store(true)
			}
			return []byte(""), nil
		}
		err := PrintPDFWithCUPSHooksCustom(context.Background(), runner, "HPTank", cmd, "/tmp/a.pdf", nil, 10*time.Millisecond, &CUPSPrintHooks{})
		if err == nil || !strings.Contains(err.Error(), "telemetry poller is required") {
			t.Fatalf("expected missing poller error, got %v", err)
		}
		if lpCalled.Load() {
			t.Fatal("lp must NOT be called without TelemetryPoller")
		}
	})

	now := time.Now().UTC()
	badReadings := []struct {
		name  string
		telem HardwareTelemetry
	}{
		{
			name: "missing_counter_unit",
			telem: HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterIndex: "1.1", CounterUnit: 0, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: now},
		},
		{
			name: "missing_counter_index",
			telem: HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterIndex: "", CounterUnit: 8, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: now},
		},
		{
			name: "missing_flags_bitmask",
			telem: HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterIndex: "1.1", CounterUnit: 8, HasCounterMeta: true, HasFlags: false, StatusState: "IDLE", ObservedAt: now},
		},
		{
			name: "zero_observed_at",
			telem: HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterIndex: "1.1", CounterUnit: 8, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE"},
		},
		{
			name: "stale_observed_at",
			telem: HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterIndex: "1.1", CounterUnit: 8, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: now.Add(-15 * time.Second)},
		},
		{
			name: "future_observed_at",
			telem: HardwareTelemetry{Online: true, HasLife: true, LifeCount: 100, CounterIndex: "1.1", CounterUnit: 8, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: now.Add(20 * time.Second)},
		},
	}

	for _, tc := range badReadings {
		t.Run(tc.name, func(t *testing.T) {
			var lpCalled atomic.Bool
			runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "lp" {
					lpCalled.Store(true)
				}
				return []byte(""), nil
			}
			hooks := &CUPSPrintHooks{
				TelemetryPoller: func(ctx context.Context) HardwareTelemetry { return tc.telem },
			}
			err := PrintPDFWithCUPSHooksCustom(context.Background(), runner, "HPTank", cmd, "/tmp/a.pdf", nil, 10*time.Millisecond, hooks)
			if err == nil || !strings.Contains(err.Error(), "baseline acquisition failed") {
				t.Fatalf("expected baseline acquisition error, got %v", err)
			}
			if lpCalled.Load() {
				t.Fatalf("lp must NOT be called for %s", tc.name)
			}
		})
	}
}

func TestPackage6A_DynamicMarkerSelection_NonDefaultIndex(t *testing.T) {
	markers := []SNMPMarker{
		{Index: "2.3", CounterUnit: 8, LifeCount: 4200, HasLifeCount: true, IsPageCounter: true},
	}
	sel := SelectPrimaryMarker(markers)
	if sel == nil || sel.Index != "2.3" || sel.LifeCount != 4200 || sel.CounterUnit != 8 {
		t.Fatalf("expected dynamic selection of marker 2.3 (life=4200, unit=8), got %+v", sel)
	}

	// Prefer page counter over non-page counter regardless of index order
	multi := []SNMPMarker{
		{Index: "1.1", CounterUnit: 3, LifeCount: 9000, HasLifeCount: true, IsPageCounter: false},
		{Index: "2.3", CounterUnit: 8, LifeCount: 4500, HasLifeCount: true, IsPageCounter: true},
	}
	selMulti := SelectPrimaryMarker(multi)
	if selMulti == nil || selMulti.Index != "2.3" {
		t.Fatalf("expected page-counter marker 2.3 to be preferred over non-page marker 1.1, got %+v", selMulti)
	}
}

func TestPackage6A_CompletionEvidence_StrictPolicy(t *testing.T) {
	cmd := contracts.PrintJobCommand{
		JobID: "job-6a-strict", OrderID: "ord-6a-strict", KioskID: "kiosk-6a",
		TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex,
	}
	baseline := CounterBaseline{HasBaseline: true, Value: 100, Index: "1.1", Unit: 7, ObservedAt: time.Now().UTC()}

	t.Run("empty_printer_state_cannot_authorize_completion", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" {
				for i, a := range args {
					if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
						return []byte("HPTank-701 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
					}
				}
			}
			return []byte(""), nil
		}
		poller := func(ctx context.Context) HardwareTelemetry {
			return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "", ObservedAt: time.Now().UTC()}
		}
		err := MonitorCUPSJobWithTelemetryBaselineCustom(ctx, runner, "HPTank", cmd, "HPTank-701", nil, 10*time.Millisecond, poller, baseline)
		if err == nil {
			t.Fatal("expected error when printer StatusState is empty (non-quiescent)")
		}
	})

	t.Run("completed_with_errors_ipp_cannot_complete", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "ipptool" {
				return []byte("job-id (integer) = 701\njob-state (enum) = completed\njob-state-reasons (keyword) = completed-with-errors,job-completed-successfully\n"), nil
			}
			return []byte(""), nil
		}
		poller := func(ctx context.Context) HardwareTelemetry {
			return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
		}
		err := MonitorCUPSJobWithTelemetryBaselineCustom(ctx, runner, "HPTank", cmd, "HPTank-701", nil, 10*time.Millisecond, poller, baseline)
		if err == nil || !strings.Contains(err.Error(), "completed-with-errors") {
			t.Fatalf("expected completed-with-errors error, got %v", err)
		}
	})

	t.Run("cups_job_id_prefix_collision_ignored", func(t *testing.T) {
		if lpstatLineMatchesJobID("HPTank-10 user 1024 Tue Nov 3 14:00:00 2026", "HPTank-1") {
			t.Fatal("HPTank-10 must NOT match HPTank-1")
		}
		if !lpstatLineMatchesJobID("HPTank-1 user 1024 Tue Nov 3 14:00:00 2026", "HPTank-1") {
			t.Fatal("HPTank-1 should match HPTank-1")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" {
				for i, a := range args {
					if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
						return []byte("HPTank-10 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
					}
				}
			}
			return []byte(""), nil
		}
		poller := func(ctx context.Context) HardwareTelemetry {
			return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
		}
		err := MonitorCUPSJobWithTelemetryBaselineCustom(ctx, runner, "HPTank", cmd, "HPTank-1", nil, 10*time.Millisecond, poller, baseline)
		if err == nil {
			t.Fatal("HPTank-1 must NOT complete from HPTank-10 output")
		}
	})

	t.Run("post_terminal_fresh_observation_required", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		staleObs := time.Now().UTC().Add(-500 * time.Millisecond)
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" {
				for i, a := range args {
					if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
						return []byte("HPTank-701 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
					}
				}
			}
			return []byte(""), nil
		}
		poller := func(ctx context.Context) HardwareTelemetry {
			return HardwareTelemetry{Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: staleObs}
		}
		err := MonitorCUPSJobWithTelemetryBaselineCustom(ctx, runner, "HPTank", cmd, "HPTank-701", nil, 10*time.Millisecond, poller, baseline)
		if err == nil {
			t.Fatal("expected error when telemetry ObservedAt precedes terminal CUPS observation")
		}
	})

	t.Run("counter_anomaly_sticky_cannot_complete_later", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var pollCalls atomic.Int32
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" {
				if pollCalls.Load() >= 2 {
					for i, a := range args {
						if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
							return []byte("HPTank-701 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
						}
					}
				}
				return []byte("HPTank-701 user 1024 Tue Nov 3 14:00:00 2026\n"), nil
			}
			return []byte(""), nil
		}
		poller := func(ctx context.Context) HardwareTelemetry {
			c := pollCalls.Add(1)
			life := int64(102)
			if c == 1 {
				life = 109 // anomaly: exceeds expected 2
			}
			return HardwareTelemetry{Online: true, HasLife: true, LifeCount: life, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC()}
		}
		err := MonitorCUPSJobWithTelemetryBaselineCustom(ctx, runner, "HPTank", cmd, "HPTank-701", nil, 10*time.Millisecond, poller, baseline)
		if err == nil || !strings.Contains(err.Error(), "counter anomaly") {
			t.Fatalf("expected sticky counter anomaly error, got %v", err)
		}
	})
}
