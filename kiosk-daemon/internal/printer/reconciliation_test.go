package printer

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestReconcilePrinterHealth_NormalOperation(t *testing.T) {
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-5 * time.Second)

	tests := []struct {
		name          string
		cups          contracts.PrinterHealth
		ipp           IPPPrinterStateResult
		snmp          contracts.PrinterHealth
		wantState     string
		wantOnline    bool
		wantPaperTray string
	}{
		{
			name: "all_subsystems_healthy_and_idle",
			cups: contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 200,
				UpdatedAt:       freshTime,
			},
			ipp: IPPPrinterStateResult{
				Source:            CapabilitySourceCUPSQueue,
				Queue:             "BrotherQueue",
				State:             IPPPrinterStateIdle,
				RawState:          "idle",
				AcceptingJobs:     AcceptingJobsAccepting,
				EndpointReachable: true,
				QueueAccessible:   true,
				StatusCode:        "successful-ok",
				ObservedAt:        freshTime,
			},
			snmp: contracts.PrinterHealth{
				Online:            true,
				State:             "IDLE",
				PaperTrayStatus:   "OK",
				EstimatedSheets:   200,
				TonerLevelPercent: 85,
				DrumLifePercent:   92,
				UpdatedAt:         freshTime,
			},
			wantState:     "IDLE",
			wantOnline:    true,
			wantPaperTray: "OK",
		},
		{
			name: "cups_printing_with_matching_ipp_processing",
			cups: contracts.PrinterHealth{
				Online:          true,
				State:           "PRINTING",
				PaperTrayStatus: "OK",
				EstimatedSheets: 190,
				UpdatedAt:       freshTime,
			},
			ipp: IPPPrinterStateResult{
				Source:            CapabilitySourceCUPSQueue,
				Queue:             "BrotherQueue",
				State:             IPPPrinterStateProcessing,
				RawState:          "processing",
				AcceptingJobs:     AcceptingJobsAccepting,
				EndpointReachable: true,
				QueueAccessible:   true,
				StatusCode:        "successful-ok",
				ObservedAt:        freshTime,
			},
			snmp: contracts.PrinterHealth{
				Online:            true,
				State:             "IDLE",
				PaperTrayStatus:   "OK",
				EstimatedSheets:   190,
				TonerLevelPercent: 85,
				DrumLifePercent:   92,
				UpdatedAt:         freshTime,
			},
			wantState:     "PRINTING",
			wantOnline:    true,
			wantPaperTray: "OK",
		},
		{
			name: "snmp_warming_up_with_cups_idle",
			cups: contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 150,
				UpdatedAt:       freshTime,
			},
			ipp: IPPPrinterStateResult{
				Source:            CapabilitySourceCUPSQueue,
				Queue:             "BrotherQueue",
				State:             IPPPrinterStateIdle,
				RawState:          "idle",
				AcceptingJobs:     AcceptingJobsAccepting,
				EndpointReachable: true,
				QueueAccessible:   true,
				StatusCode:        "successful-ok",
				ObservedAt:        freshTime,
			},
			snmp: contracts.PrinterHealth{
				Online:            true,
				State:             "WARMING_UP",
				PaperTrayStatus:   "OK",
				EstimatedSheets:   150,
				TonerLevelPercent: 85,
				DrumLifePercent:   92,
				UpdatedAt:         freshTime,
			},
			wantState:     "WARMING_UP",
			wantOnline:    true,
			wantPaperTray: "OK",
		},
		{
			name: "cups_offline_marks_overall_offline",
			cups: contracts.PrinterHealth{
				Online:          false,
				State:           "OFFLINE",
				ErrorMessage:    "printer disabled",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       freshTime,
			},
			ipp: IPPPrinterStateResult{
				Source:            CapabilitySourceCUPSQueue,
				Queue:             "BrotherQueue",
				State:             IPPPrinterStateStopped,
				RawState:          "stopped",
				AcceptingJobs:     AcceptingJobsNotAccepting,
				EndpointReachable: true,
				QueueAccessible:   true,
				StatusCode:        "successful-ok",
				ObservedAt:        freshTime,
			},
			snmp: contracts.PrinterHealth{
				Online:            true,
				State:             "IDLE",
				PaperTrayStatus:   "OK",
				EstimatedSheets:   100,
				TonerLevelPercent: 85,
				DrumLifePercent:   92,
				UpdatedAt:         freshTime,
			},
			wantState:     "OFFLINE",
			wantOnline:    false,
			wantPaperTray: "OK",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := ReconcilePrinterHealth(tc.cups, tc.ipp, tc.snmp, evalTime)
			if res.EffectiveState != tc.wantState {
				t.Errorf("EffectiveState = %q, want %q", res.EffectiveState, tc.wantState)
			}
			if res.Online != tc.wantOnline {
				t.Errorf("Online = %v, want %v", res.Online, tc.wantOnline)
			}
			if res.PaperTrayStatus != tc.wantPaperTray {
				t.Errorf("PaperTrayStatus = %q, want %q", res.PaperTrayStatus, tc.wantPaperTray)
			}
			if tc.wantState == "IDLE" && !res.IsReady() {
				t.Errorf("expected IsReady = true for healthy idle printer")
			}
			if len(res.BlockingFaults) > 0 && tc.wantState != "ERROR" && tc.wantState != "OFFLINE" {
				t.Errorf("unexpected blocking faults: %v", res.BlockingFaults)
			}
		})
	}
}

func TestReconcilePrinterHealth_MajorHardwareFaults(t *testing.T) {
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	baseCUPS := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 200,
		UpdatedAt:       freshTime,
	}
	baseIPP := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateIdle,
		RawState:          "idle",
		AcceptingJobs:     AcceptingJobsAccepting,
		EndpointReachable: true,
		QueueAccessible:   true,
		StatusCode:        "successful-ok",
		ObservedAt:        freshTime,
	}
	baseSNMP := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   200,
		TonerLevelPercent: 80,
		DrumLifePercent:   90,
		UpdatedAt:         freshTime,
	}

	t.Run("paper_jam_from_snmp", func(t *testing.T) {
		snmp := baseSNMP
		snmp.PaperJam = true
		snmp.State = "ERROR"
		snmp.ErrorMessage = "paper jam"

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "paper jam") {
			t.Errorf("expected paper jam in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("paper_jam_from_ipp_error_reason", func(t *testing.T) {
		ipp := baseIPP
		ipp.RawReasons = []string{"media-jam-error"}
		ipp.ParsedReasons = []IPPPrinterReason{
			{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
		}
		ipp.PaperJam = true
		ipp.HasHardwareFault = true

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "paper jam") {
			t.Errorf("expected paper jam in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("paper_jam_from_cups_error_string", func(t *testing.T) {
		cups := baseCUPS
		cups.State = "ERROR"
		cups.ErrorMessage = "Printer error: paper jam in tray 1"

		res := ReconcilePrinterHealth(cups, baseIPP, baseSNMP, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "paper jam") {
			t.Errorf("expected paper jam in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("cover_open_from_snmp", func(t *testing.T) {
		snmp := baseSNMP
		snmp.CoverOpen = true
		snmp.State = "ERROR"

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.CoverOpen {
			t.Errorf("expected CoverOpen = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "cover or door open") {
			t.Errorf("expected cover open in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("cover_open_from_ipp_door_open_reason", func(t *testing.T) {
		ipp := baseIPP
		ipp.RawReasons = []string{"door-open-error"}
		ipp.ParsedReasons = []IPPPrinterReason{
			{Raw: "door-open-error", Keyword: "door-open", Severity: SeverityError},
		}
		ipp.CoverOpen = true
		ipp.HasHardwareFault = true

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.CoverOpen {
			t.Errorf("expected CoverOpen = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "cover or door open") {
			t.Errorf("expected cover open in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("output_bin_full_from_snmp", func(t *testing.T) {
		snmp := baseSNMP
		snmp.OutputBinFull = true
		snmp.State = "ERROR"

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.OutputBinFull {
			t.Errorf("expected OutputBinFull = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "output bin full") {
			t.Errorf("expected output bin full in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("output_bin_full_from_ipp", func(t *testing.T) {
		ipp := baseIPP
		ipp.RawReasons = []string{"output-bin-full-error"}
		ipp.ParsedReasons = []IPPPrinterReason{
			{Raw: "output-bin-full-error", Keyword: "output-bin-full", Severity: SeverityError},
		}
		ipp.OutputBinFull = true
		ipp.HasHardwareFault = true

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.OutputBinFull {
			t.Errorf("expected OutputBinFull = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "output bin full") {
			t.Errorf("expected output bin full in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("paper_tray_empty_from_snmp", func(t *testing.T) {
		snmp := baseSNMP
		snmp.PaperTrayStatus = "EMPTY"
		snmp.State = "ERROR"

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if res.PaperTrayStatus != "EMPTY" {
			t.Errorf("PaperTrayStatus = %q, want EMPTY", res.PaperTrayStatus)
		}
		if !sliceHasItemContaining(res.BlockingFaults, "media empty") {
			t.Errorf("expected media empty in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("paper_tray_empty_from_ipp_media_empty_reason", func(t *testing.T) {
		ipp := baseIPP
		ipp.RawReasons = []string{"media-empty-error"}
		ipp.ParsedReasons = []IPPPrinterReason{
			{Raw: "media-empty-error", Keyword: "media-empty", Severity: SeverityError},
		}
		ipp.MediaEmpty = true
		ipp.HasHardwareFault = true

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if res.PaperTrayStatus != "EMPTY" {
			t.Errorf("PaperTrayStatus = %q, want EMPTY", res.PaperTrayStatus)
		}
		if !sliceHasItemContaining(res.BlockingFaults, "media empty") {
			t.Errorf("expected media empty in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("critical_hardware_error_from_snmp_service_requested", func(t *testing.T) {
		snmp := baseSNMP
		snmp.State = "ERROR"
		snmp.ErrorMessage = "service requested: fuser unit error"

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !sliceHasItemContaining(res.BlockingFaults, "hardware error") {
			t.Errorf("expected hardware error in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("critical_hardware_error_from_ipp_marker_waste_full", func(t *testing.T) {
		ipp := baseIPP
		ipp.RawReasons = []string{"marker-waste-full-error"}
		ipp.ParsedReasons = []IPPPrinterReason{
			{Raw: "marker-waste-full-error", Keyword: "marker-waste-full", Severity: SeverityError},
		}
		ipp.HasHardwareFault = true

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !sliceHasItemContaining(res.BlockingFaults, "marker-waste-full") {
			t.Errorf("expected marker-waste-full in BlockingFaults, got %v", res.BlockingFaults)
		}
	})
}

func TestReconcilePrinterHealth_MultipleSimultaneousFaults(t *testing.T) {
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		CoverOpen:       true,
		PaperTrayStatus: "EMPTY",
		EstimatedSheets: 0,
		UpdatedAt:       freshTime,
	}

	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateStopped,
		RawReasons:        []string{"media-jam-error", "media-empty-error"},
		ParsedReasons: []IPPPrinterReason{
			{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			{Raw: "media-empty-error", Keyword: "media-empty", Severity: SeverityError},
		},
		PaperJam:          true,
		MediaEmpty:        true,
		HasHardwareFault:  true,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}

	snmp := contracts.PrinterHealth{
		Online:            true,
		State:             "ERROR",
		PaperJam:          true,
		CoverOpen:         true,
		PaperTrayStatus:   "EMPTY",
		EstimatedSheets:   0,
		TonerLevelPercent: 50,
		DrumLifePercent:   60,
		UpdatedAt:         freshTime,
	}

	res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)
	if res.EffectiveState != "ERROR" {
		t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
	}
	if !res.PaperJam {
		t.Errorf("expected PaperJam = true")
	}
	if !res.CoverOpen {
		t.Errorf("expected CoverOpen = true")
	}
	if res.PaperTrayStatus != "EMPTY" {
		t.Errorf("expected PaperTrayStatus = EMPTY, got %q", res.PaperTrayStatus)
	}

	// Verify all faults are present in BlockingFaults
	if !sliceHasItemContaining(res.BlockingFaults, "paper jam") {
		t.Errorf("missing paper jam in BlockingFaults: %v", res.BlockingFaults)
	}
	if !sliceHasItemContaining(res.BlockingFaults, "cover or door open") {
		t.Errorf("missing cover open in BlockingFaults: %v", res.BlockingFaults)
	}
	if !sliceHasItemContaining(res.BlockingFaults, "media empty") {
		t.Errorf("missing media empty in BlockingFaults: %v", res.BlockingFaults)
	}
}

func TestReconcilePrinterHealth_ConfirmedFaultOverridesPrintingState(t *testing.T) {
	// Rule 7: A confirmed critical hardware fault overrides a reported printing state
	// in the diagnostic result, but must not interrupt the actual print job.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "PRINTING",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		UpdatedAt:       freshTime,
	}

	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateProcessing,
		RawState:          "processing",
		AcceptingJobs:     AcceptingJobsAccepting,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}

	snmp := contracts.PrinterHealth{
		Online:            true,
		State:             "ERROR",
		PaperJam:          true,
		ErrorMessage:      "paper jam in engine",
		EstimatedSheets:   100,
		TonerLevelPercent: 75,
		DrumLifePercent:   80,
		UpdatedAt:         freshTime,
	}

	res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

	// State must be overridden to ERROR
	if res.EffectiveState != "ERROR" {
		t.Errorf("EffectiveState = %q, want ERROR (confirmed physical jam overrides PRINTING)", res.EffectiveState)
	}
	if !res.PaperJam {
		t.Errorf("expected PaperJam = true")
	}
	if !res.HasBlockingFaults() {
		t.Errorf("expected HasBlockingFaults = true")
	}

	// Contradiction must be documented: CUPS reports PRINTING while physical fault confirmed
	if !sliceHasItemContaining(res.Contradictions, "PRINTING while physical hardware fault is confirmed") {
		t.Errorf("expected contradiction regarding PRINTING with hardware fault, got %v", res.Contradictions)
	}
}

func TestReconcilePrinterHealth_IPPReasonSeverityAndIndependentConfirmation(t *testing.T) {
	// Rule 8: Use IPP ParsedReasons severity: -error is critical,
	// -warning and -report are nonblocking unless independently confirmed as critical.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	baseCUPS := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 150,
		UpdatedAt:       freshTime,
	}
	baseSNMP := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   150,
		TonerLevelPercent: 75,
		DrumLifePercent:   80,
		UpdatedAt:         freshTime,
	}

	t.Run("media_jam_warning_is_blocking_by_keyword_semantics", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:        CapabilitySourceCUPSQueue,
			Queue:         "BrotherQueue",
			State:         IPPPrinterStateIdle,
			RawReasons:    []string{"media-jam-warning"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-warning", Keyword: "media-jam", Severity: SeverityWarning},
			},
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)

		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR (media-jam is blocking regardless of -warning suffix)", res.EffectiveState)
		}
		if !res.HasBlockingFaults() {
			t.Errorf("expected blocking faults, got none")
		}
	})

	t.Run("media_empty_report_is_blocking_by_keyword_semantics", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:        CapabilitySourceCUPSQueue,
			Queue:         "BrotherQueue",
			State:         IPPPrinterStateIdle,
			RawReasons:    []string{"media-empty-report"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-empty-report", Keyword: "media-empty", Severity: SeverityReport},
			},
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)

		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if res.EvaluatedHealth != contracts.EvaluatedHealthFault {
			t.Errorf("EvaluatedHealth = %q, want FAULT", res.EvaluatedHealth)
		}
		if !res.HasBlockingFaults() || res.PaperTrayStatus != "EMPTY" {
			t.Errorf("expected paper empty blocking fault, got tray=%s faults=%v", res.PaperTrayStatus, res.BlockingFaults)
		}
	})

	t.Run("media_jam_warning_confirmed_by_snmp_is_critical", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateIdle,
			RawReasons:        []string{"media-jam-warning"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-warning", Keyword: "media-jam", Severity: SeverityWarning},
			},
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}

		snmp := baseSNMP
		snmp.PaperJam = true
		snmp.State = "ERROR"

		res := ReconcilePrinterHealth(baseCUPS, ipp, snmp, evalTime)

		// Independently confirmed by SNMP -> critical blocking fault
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR (confirmed by SNMP)", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "paper jam") {
			t.Errorf("expected paper jam in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	t.Run("marker_waste_warning_is_nonblocking", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateIdle,
			RawReasons:        []string{"marker-waste-almost-full-warning"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "marker-waste-almost-full-warning", Keyword: "marker-waste-almost-full", Severity: SeverityWarning},
			},
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)

		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.HasBlockingFaults() {
			t.Errorf("expected no blocking faults, got %v", res.BlockingFaults)
		}
		if !sliceHasItemContaining(res.NonblockingWarnings, "marker-waste-almost-full") {
			t.Errorf("expected warning in NonblockingWarnings, got %v", res.NonblockingWarnings)
		}
	})
}

func TestReconcilePrinterHealth_NonblockingWarnings(t *testing.T) {
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	baseCUPS := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		UpdatedAt:       freshTime,
	}
	baseIPP := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateIdle,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}

	t.Run("low_toner_warning", func(t *testing.T) {
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 8,
			DrumLifePercent:   80,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)

		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE (low toner is nonblocking)", res.EffectiveState)
		}
		if res.HasBlockingFaults() {
			t.Errorf("expected no blocking faults, got %v", res.BlockingFaults)
		}
		if !sliceHasItemContaining(res.NonblockingWarnings, "low toner level (8%)") {
			t.Errorf("expected low toner in NonblockingWarnings, got %v", res.NonblockingWarnings)
		}
	})

	t.Run("low_paper_tray_warning", func(t *testing.T) {
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "LOW",
			EstimatedSheets:   15,
			TonerLevelPercent: 80,
			DrumLifePercent:   80,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)

		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.PaperTrayStatus != "LOW" {
			t.Errorf("PaperTrayStatus = %q, want LOW", res.PaperTrayStatus)
		}
		if !sliceHasItemContaining(res.NonblockingWarnings, "paper tray low") {
			t.Errorf("expected low paper tray in NonblockingWarnings, got %v", res.NonblockingWarnings)
		}
	})

	t.Run("low_drum_life_warning", func(t *testing.T) {
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   5,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmp, evalTime)

		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if !sliceHasItemContaining(res.NonblockingWarnings, "low drum life (5%)") {
			t.Errorf("expected low drum in NonblockingWarnings, got %v", res.NonblockingWarnings)
		}
	})
}

func TestReconcilePrinterHealth_StaleAndInvalidIPP(t *testing.T) {
	// Rule 12: Reject stale IPP observations using an explicit documented maximum age;
	// treat zero timestamps and future timestamps conservatively.
	// Rule 13: Do not interpret EndpointReachable alone as proof of printer readiness.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

	baseCUPS := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		UpdatedAt:       evalTime.Add(-5 * time.Second),
	}
	baseSNMP := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   100,
		TonerLevelPercent: 80,
		DrumLifePercent:   80,
		UpdatedAt:         evalTime.Add(-5 * time.Second),
	}

	t.Run("zero_timestamp_rejected_conservatively", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			PaperJam:          true,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        time.Time{}, // zero timestamp
		}

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)

		// Zero timestamp IPP must NOT inject active blocking faults
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE (zero timestamp IPP must be rejected)", res.EffectiveState)
		}
		if res.PaperJam {
			t.Errorf("expected PaperJam = false when IPP is rejected")
		}
		if !sliceHasItemContaining(res.UnknownConditions, "IPP observation timestamp is zero") {
			t.Errorf("expected zero timestamp in UnknownConditions, got %v", res.UnknownConditions)
		}
	})

	t.Run("future_timestamp_rejected_conservatively", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			PaperJam:          true,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        evalTime.Add(15 * time.Second), // 15s in the future (> 5s tolerance)
		}

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)

		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE (future IPP must be rejected)", res.EffectiveState)
		}
		if !sliceHasItemContaining(res.UnknownConditions, "future") {
			t.Errorf("expected future timestamp note in UnknownConditions, got %v", res.UnknownConditions)
		}
	})

	t.Run("stale_observation_older_than_max_age_rejected", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			PaperJam:          true,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        evalTime.Add(-45 * time.Second), // 45s old (> 30s max age)
		}

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)

		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE (stale IPP must be rejected)", res.EffectiveState)
		}
		if !sliceHasItemContaining(res.NonblockingWarnings, "stale") {
			t.Errorf("expected stale warning in NonblockingWarnings, got %v", res.NonblockingWarnings)
		}
		if !sliceHasItemContaining(res.UnknownConditions, "stale") {
			t.Errorf("expected stale note in UnknownConditions, got %v", res.UnknownConditions)
		}
	})

	t.Run("endpoint_reachable_alone_does_not_indicate_readiness", func(t *testing.T) {
		// Endpoint responded, but returned an IPP error and queue is not accessible
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "MissingQueue",
			State:             IPPPrinterStateUnknown,
			EndpointReachable: true,
			QueueAccessible:   false,
			StatusCode:        "client-error-not-found",
			Error:             "IPP state query returned failure status client-error-not-found",
			ObservedAt:        evalTime.Add(-5 * time.Second),
		}

		// When CUPS queue is offline, EndpointReachable must NOT grant readiness
		cupsOffline := baseCUPS
		cupsOffline.Online = false
		cupsOffline.State = "OFFLINE"

		res := ReconcilePrinterHealth(cupsOffline, ipp, baseSNMP, evalTime)

		if res.EffectiveState != "OFFLINE" {
			t.Errorf("EffectiveState = %q, want OFFLINE", res.EffectiveState)
		}
		if res.Online {
			t.Errorf("expected Online = false")
		}
		if res.IsReady() {
			t.Errorf("expected IsReady = false")
		}
	})
}

func TestReconcilePrinterHealth_UnknownTelemetry(t *testing.T) {
	// Rule 10: Preserve toner/drum percentages as observations,
	// not proof of consumable faults when telemetry is unavailable.
	// Rule 11: Keep unknown/missing data distinct from confirmed healthy.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	baseCUPS := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "", // Missing paper tray status
		EstimatedSheets: 100,
		UpdatedAt:       freshTime,
	}
	baseIPP := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateIdle,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}

	t.Run("snmp_offline_does_not_manufacture_consumable_faults", func(t *testing.T) {
		snmpOffline := contracts.PrinterHealth{
			Online:            false,
			State:             "OFFLINE",
			ErrorMessage:      "snmp network timeout",
			TonerLevelPercent: 0,
			DrumLifePercent:   0,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmpOffline, evalTime)

		// Must remain IDLE based on CUPS queue readiness
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.TonerLevelPercent != 0 || res.DrumLifePercent != 0 {
			t.Errorf("expected 0%% levels, got toner=%d drum=%d", res.TonerLevelPercent, res.DrumLifePercent)
		}
		if res.HasBlockingFaults() {
			t.Errorf("unexpected blocking faults: %v", res.BlockingFaults)
		}
		if !sliceHasItemContaining(res.UnknownConditions, "SNMP telemetry unavailable") {
			t.Errorf("expected SNMP unavailable in UnknownConditions, got %v", res.UnknownConditions)
		}
		// Missing paper tray status must be UNKNOWN, not OK
		if res.PaperTrayStatus != "UNKNOWN" {
			t.Errorf("PaperTrayStatus = %q, want UNKNOWN", res.PaperTrayStatus)
		}
	})

	t.Run("snmp_hrprinterstatus_1_other_is_unknown", func(t *testing.T) {
		snmpOther := contracts.PrinterHealth{
			Online:            true,
			State:             "UNKNOWN", // hrPrinterStatus = 1 ("other")
			PaperTrayStatus:   "UNKNOWN",
			EstimatedSheets:   100,
			TonerLevelPercent: 50,
			DrumLifePercent:   70,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(baseCUPS, baseIPP, snmpOther, evalTime)

		if !sliceHasItemContaining(res.UnknownConditions, "hrPrinterStatus 1 'other'") {
			t.Errorf("expected hrPrinterStatus note in UnknownConditions, got %v", res.UnknownConditions)
		}
	})
}

func TestReconcilePrinterHealth_CrossSubsystemContradictions(t *testing.T) {
	// Rule 14: Report contradictions such as CUPS PRINTING versus IPP idle/stopped,
	// or IPP faults versus apparently normal SNMP data.
	// Rule 15: Do not automatically resolve contradictions by suppressing confirmed critical fault evidence.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	t.Run("cups_printing_vs_ipp_idle_contradiction", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       freshTime,
		}
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateIdle,
			RawState:          "idle",
			AcceptingJobs:     AcceptingJobsAccepting,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

		// CUPS remains authoritative for execution state when no hardware fault exists
		if res.EffectiveState != "PRINTING" {
			t.Errorf("EffectiveState = %q, want PRINTING", res.EffectiveState)
		}
		if !sliceHasItemContaining(res.Contradictions, "CUPS reports queue PRINTING while IPP explicitly reports printer-state \"idle\"") {
			t.Errorf("expected job state contradiction, got %v", res.Contradictions)
		}
	})

	t.Run("ipp_paper_jam_vs_normal_snmp_contradiction_does_not_suppress_fault", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       freshTime,
		}
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			PaperJam:          true,
			HasHardwareFault:  true,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperJam:          false,
			CoverOpen:         false,
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

		// Fault must NOT be suppressed
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR (fault must not be suppressed)", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "paper jam") {
			t.Errorf("expected paper jam in BlockingFaults, got %v", res.BlockingFaults)
		}
		if !sliceHasItemContaining(res.Contradictions, "observation mismatch: IPP reports paper jam fault, but SNMP telemetry does not observe a jam condition") {
			t.Errorf("expected observation mismatch regarding IPP jam vs SNMP normal, got %v", res.Contradictions)
		}
	})

	t.Run("snmp_paper_jam_vs_ipp_idle_contradiction_does_not_suppress_fault", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       freshTime,
		}
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateIdle,
			RawReasons:        []string{"none"},
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "ERROR",
			PaperJam:          true,
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

		// Fault must NOT be suppressed
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if !sliceHasItemContaining(res.Contradictions, "observation mismatch: SNMP reports paper jam fault, but IPP state reasons do not list a jam condition") {
			t.Errorf("expected observation mismatch regarding SNMP jam vs IPP normal, got %v", res.Contradictions)
		}
	})

	t.Run("cups_paper_empty_vs_snmp_paper_ok_contradiction", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperTrayStatus: "EMPTY",
			EstimatedSheets: 0,
			UpdatedAt:       freshTime,
		}
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateIdle,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if res.PaperTrayStatus != "EMPTY" {
			t.Errorf("PaperTrayStatus = %q, want EMPTY", res.PaperTrayStatus)
		}
		if !sliceHasItemContaining(res.Contradictions, "contradiction: CUPS reports paper tray EMPTY while SNMP explicitly reports paper tray OK") {
			t.Errorf("expected paper tray contradiction, got %v", res.Contradictions)
		}
	})
}

func TestReconcilePrinterHealth_DeterministicRepeatedEvaluation(t *testing.T) {
	// Rule 16: Keep deterministic severity and source attribution.
	// Rule 17: Deterministic repeated evaluation.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "PRINTING",
		PaperTrayStatus: "OK",
		EstimatedSheets: 120,
		UpdatedAt:       freshTime,
	}
	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateProcessing,
		RawReasons:        []string{"media-jam-error", "marker-supply-low-warning"},
		ParsedReasons: []IPPPrinterReason{
			{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			{Raw: "marker-supply-low-warning", Keyword: "marker-supply-low", Severity: SeverityWarning},
		},
		PaperJam:          true,
		MarkerLow:         true,
		HasHardwareFault:  true,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}
	snmp := contracts.PrinterHealth{
		Online:            true,
		State:             "ERROR",
		PaperJam:          true,
		PaperTrayStatus:   "OK",
		EstimatedSheets:   120,
		TonerLevelPercent: 9,
		DrumLifePercent:   88,
		UpdatedAt:         freshTime,
	}

	first := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

	for i := 0; i < 50; i++ {
		subsequent := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)
		if !reflect.DeepEqual(first, subsequent) {
			t.Fatalf("evaluation %d differed from first:\nfirst: %+v\nsubsequent: %+v", i, first, subsequent)
		}
	}
}

func TestReconcilePrinterHealth_InputImmutability(t *testing.T) {
	// Rule 5: Preserve original inputs; reconciliation must not mutate them or query hardware.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "PRINTING",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		ErrorMessage:    "some message",
		UpdatedAt:       freshTime,
	}
	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateProcessing,
		RawReasons:        []string{"media-jam-error"},
		ParsedReasons: []IPPPrinterReason{
			{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
		},
		PaperJam:          true,
		HasHardwareFault:  true,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}
	snmp := contracts.PrinterHealth{
		Online:            true,
		State:             "ERROR",
		PaperJam:          true,
		PaperTrayStatus:   "OK",
		EstimatedSheets:   100,
		TonerLevelPercent: 50,
		DrumLifePercent:   60,
		ErrorMessage:      "snmp error",
		UpdatedAt:         freshTime,
	}

	cupsBefore := cups
	ippBefore := ipp
	snmpBefore := snmp

	_ = ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

	if !reflect.DeepEqual(cups, cupsBefore) {
		t.Errorf("cups input was mutated by ReconcilePrinterHealth")
	}
	if !reflect.DeepEqual(ipp, ippBefore) {
		t.Errorf("ipp input was mutated by ReconcilePrinterHealth")
	}
	if !reflect.DeepEqual(snmp, snmpBefore) {
		t.Errorf("snmp input was mutated by ReconcilePrinterHealth")
	}
}

func TestReconcilePrinterHealth_ZeroEvaluationTimeRegression(t *testing.T) {
	// Rule 1, 2, 3: Keep pure, remove time.Now fallback, return deterministic UNKNOWN/invalid-time
	// diagnostics for zero evalTime without accepting time-dependent IPP fault evidence.
	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
	}
	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateStopped,
		RawReasons:        []string{"media-jam-error"},
		ParsedReasons: []IPPPrinterReason{
			{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
		},
		PaperJam:          true,
		HasHardwareFault:  true,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
	}
	snmp := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   100,
		TonerLevelPercent: 80,
		DrumLifePercent:   90,
	}

	res := ReconcilePrinterHealth(cups, ipp, snmp, time.Time{})

	if res.EffectiveState != "UNKNOWN" {
		t.Errorf("EffectiveState = %q, want UNKNOWN for zero evalTime", res.EffectiveState)
	}
	if res.Online {
		t.Errorf("Online = %v, want false for zero evalTime", res.Online)
	}
	if !res.EvaluatedAt.IsZero() {
		t.Errorf("EvaluatedAt must be zero time.Time, got %v", res.EvaluatedAt)
	}
	if !sliceHasItemContaining(res.UnknownConditions, "evaluation time is zero") {
		t.Errorf("expected zero evaluation time in UnknownConditions, got %v", res.UnknownConditions)
	}
	// Time-dependent IPP fault evidence must NOT be accepted into BlockingFaults
	if sliceHasItemContaining(res.BlockingFaults, "paper jam") {
		t.Errorf("IPP paper jam should not be accepted when evalTime is zero: %v", res.BlockingFaults)
	}
	if res.SourceAttribution.IPP.Available {
		t.Errorf("IPP Available must be false when evalTime is zero")
	}

	// Assert repeated evaluation on zero evalTime is 100% deterministic
	for i := 0; i < 20; i++ {
		repeat := ReconcilePrinterHealth(cups, ipp, snmp, time.Time{})
		if !reflect.DeepEqual(res, repeat) {
			t.Fatalf("repeated zero evalTime evaluation differed at iteration %d", i)
		}
	}
}

func TestReconcilePrinterHealth_InaccessibleIPPQueueRegression(t *testing.T) {
	// Rule 4, 5, 6: Require QueueAccessible=true; reject observations with QueueAccessible=false
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		UpdatedAt:       freshTime,
	}
	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "NonExistentQueue",
		State:             IPPPrinterStateStopped,
		RawReasons:        []string{"media-jam-error"},
		ParsedReasons: []IPPPrinterReason{
			{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
		},
		PaperJam:          true,
		HasHardwareFault:  true,
		EndpointReachable: true,
		QueueAccessible:   false, // queue does not exist / inaccessible on server
		StatusCode:        "client-error-not-found",
		ObservedAt:        freshTime,
	}
	snmp := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   100,
		TonerLevelPercent: 80,
		DrumLifePercent:   90,
		UpdatedAt:         freshTime,
	}

	res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

	// Since IPP QueueAccessible is false, IPP data is rejected from establishing active blocking faults
	if res.EffectiveState != "IDLE" {
		t.Errorf("EffectiveState = %q, want IDLE (inaccessible IPP queue must be rejected)", res.EffectiveState)
	}
	if res.PaperJam {
		t.Errorf("PaperJam must be false when IPP queue is inaccessible")
	}
	if sliceHasItemContaining(res.BlockingFaults, "paper jam") {
		t.Errorf("BlockingFaults must not contain paper jam from inaccessible IPP: %v", res.BlockingFaults)
	}
	if res.SourceAttribution.IPP.Available {
		t.Errorf("IPP attribution Available must be false for inaccessible queue")
	}
	if !sliceHasItemContaining(res.UnknownConditions, "inaccessible on CUPS server") {
		t.Errorf("expected inaccessible queue note in UnknownConditions, got %v", res.UnknownConditions)
	}
	// Raw diagnostics must still be preserved for auditing (Rule 6)
	if !sliceHasItemContaining(res.RawIPPReasons, "media-jam-error") {
		t.Errorf("RawIPPReasons must preserve raw tokens even when rejected: %v", res.RawIPPReasons)
	}
}

func TestReconcilePrinterHealth_FailedIPPStatusAndQueryErrorRegression(t *testing.T) {
	// Rule 5: Reject IPP observations with explicit IPP failure status or nonempty query error
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		UpdatedAt:       freshTime,
	}
	snmp := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   100,
		TonerLevelPercent: 80,
		DrumLifePercent:   90,
		UpdatedAt:         freshTime,
	}

	t.Run("server_error_status_rejected", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "FaultyQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "server-error-internal-error",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.SourceAttribution.IPP.Available {
			t.Errorf("IPP Available must be false on IPP failure status")
		}
		if !sliceHasItemContaining(res.UnknownConditions, "failure status \"server-error-internal-error\"") {
			t.Errorf("expected failure status in UnknownConditions, got %v", res.UnknownConditions)
		}
	})

	t.Run("query_error_rejected", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "CrashingQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			EndpointReachable: true,
			QueueAccessible:   true,
			Error:             "ipptool crashed: signal 11",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.SourceAttribution.IPP.Available {
			t.Errorf("IPP Available must be false on query error")
		}
		if !sliceHasItemContaining(res.UnknownConditions, "IPP state query failed: ipptool crashed: signal 11") {
			t.Errorf("expected query error in UnknownConditions, got %v", res.UnknownConditions)
		}
	})
}

func TestReconcilePrinterHealth_MissingVsExplicitlyHealthyObservations(t *testing.T) {
	// Rule 7, 8, 9: Missing fault flags must not be described as confirmed healthy sensor readings.
	// Only report a definite contradiction when the other source explicitly reports a relevant healthy condition.
	// Otherwise report a weaker observation mismatch.
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		UpdatedAt:       freshTime,
	}

	t.Run("definite_contradiction_for_explicit_paper_tray_status_ok", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-empty-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-empty-error", Keyword: "media-empty", Severity: SeverityError},
			},
			MediaEmpty:        true,
			HasHardwareFault:  true,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK", // Explicitly reported OK
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

		// Must report definite contradiction because SNMP explicitly reported "OK"
		if !sliceHasItemContaining(res.Contradictions, "contradiction: IPP reports media empty while SNMP explicitly reports paper tray OK") {
			t.Errorf("expected definite contradiction for explicit OK, got %v", res.Contradictions)
		}
		// Fault must not be suppressed
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if res.PaperTrayStatus != "EMPTY" {
			t.Errorf("PaperTrayStatus = %q, want EMPTY", res.PaperTrayStatus)
		}
	})

	t.Run("observation_mismatch_for_missing_jam_flag", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			PaperJam:          true,
			HasHardwareFault:  true,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperJam:          false, // Absent flag, not explicit "no jam" sensor
			CoverOpen:         false,
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

		// Must report observation mismatch, NOT describe SNMP as "no paper jam" or "healthy sensor reading"
		if !sliceHasItemContaining(res.Contradictions, "observation mismatch: IPP reports paper jam fault, but SNMP telemetry does not observe a jam condition") {
			t.Errorf("expected weaker observation mismatch, got %v", res.Contradictions)
		}
		for _, c := range res.Contradictions {
			if strings.Contains(c, "reports no paper jam") {
				t.Errorf("contradiction must not describe missing flag as confirmed healthy: %q", c)
			}
			if strings.Contains(c, "healthy sensor reading") {
				t.Errorf("contradiction must not describe missing flag as confirmed healthy reading: %q", c)
			}
		}
		// Confirmed fault must NEVER be suppressed
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("PaperJam must remain true")
		}
	})

	t.Run("observation_mismatch_for_missing_cover_flag_in_reverse", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateIdle,
			RawReasons:        []string{"none"},
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTime,
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "ERROR",
			CoverOpen:         true,
			PaperTrayStatus:   "OK",
			EstimatedSheets:   100,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime)

		// Must report observation mismatch, NOT describe IPP as "cover closed"
		if !sliceHasItemContaining(res.Contradictions, "observation mismatch: SNMP reports cover open fault, but IPP state reasons do not list cover open") {
			t.Errorf("expected weaker observation mismatch, got %v", res.Contradictions)
		}
		for _, c := range res.Contradictions {
			if strings.Contains(c, "cover closed") {
				t.Errorf("contradiction must not describe absent IPP reason as 'cover closed': %q", c)
			}
		}
		// Confirmed fault must NEVER be suppressed
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.CoverOpen {
			t.Errorf("CoverOpen must remain true")
		}
	})
}

func TestReconcilePrinterHealth_SourceValidityCUPSContrasts(t *testing.T) {
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateIdle,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}
	snmp := contracts.PrinterHealth{
		Online: false,
		State:  "UNKNOWN",
	}

	t.Run("cups_invalid_yields_unknown_without_offline_fault", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "printer disabled",
			PaperTrayStatus: "UNKNOWN",
			UpdatedAt:       freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime, SourceValidity{
			CUPSValid: false,
			SNMPValid: false,
		})

		if res.EffectiveState != "UNKNOWN" {
			t.Errorf("EffectiveState = %q, want UNKNOWN", res.EffectiveState)
		}
		if res.Online {
			t.Error("Online must be false")
		}
		for _, f := range res.BlockingFaults {
			if strings.Contains(strings.ToLower(f), "offline") {
				t.Errorf("invalid CUPS must not produce queue offline blocking fault: %v", res.BlockingFaults)
			}
		}
		for _, d := range res.DiagnosticDetails {
			if d.Code == FaultQueueOffline {
				t.Errorf("invalid CUPS must not emit FaultQueueOffline diagnostic item")
			}
		}
		if res.SourceAttribution.CUPS.Available {
			t.Error("CUPS attribution Available must be false when invalid")
		}
	})

	t.Run("cups_valid_offline_yields_offline_fault", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "printer disabled",
			PaperTrayStatus: "UNKNOWN",
			UpdatedAt:       freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime, SourceValidity{
			CUPSValid: true,
			SNMPValid: false,
		})

		if res.EffectiveState != "OFFLINE" {
			t.Errorf("EffectiveState = %q, want OFFLINE", res.EffectiveState)
		}
		if res.Online {
			t.Error("Online must be false")
		}
		hasOffline := false
		for _, f := range res.BlockingFaults {
			if strings.Contains(strings.ToLower(f), "offline") {
				hasOffline = true
				break
			}
		}
		if !hasOffline {
			t.Errorf("valid offline CUPS must produce queue offline blocking fault: %v", res.BlockingFaults)
		}
		if !res.SourceAttribution.CUPS.Available {
			t.Error("CUPS attribution Available must be true for valid CUPS observation")
		}
	})
}

func TestReconcilePrinterHealth_SourceValiditySNMPContrasts(t *testing.T) {
	evalTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	freshTime := evalTime.Add(-2 * time.Second)

	cups := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		UpdatedAt:       freshTime,
	}
	ipp := IPPPrinterStateResult{
		Source:            CapabilitySourceCUPSQueue,
		Queue:             "BrotherQueue",
		State:             IPPPrinterStateIdle,
		EndpointReachable: true,
		QueueAccessible:   true,
		ObservedAt:        freshTime,
	}

	t.Run("snmp_invalid_suppresses_hardware_faults", func(t *testing.T) {
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "ERROR",
			PaperJam:          true,
			CoverOpen:         true,
			OutputBinFull:     true,
			PaperTrayStatus:   "EMPTY",
			TonerLevelPercent: 5,
			DrumLifePercent:   10,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime, SourceValidity{
			CUPSValid: true,
			SNMPValid: false,
		})

		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE (from valid CUPS)", res.EffectiveState)
		}
		if res.PaperJam {
			t.Error("PaperJam must be false when SNMP is invalid")
		}
		if res.CoverOpen {
			t.Error("CoverOpen must be false when SNMP is invalid")
		}
		if res.OutputBinFull {
			t.Error("OutputBinFull must be false when SNMP is invalid")
		}
		if res.TonerLevelPercent != 0 {
			t.Errorf("TonerLevelPercent = %d, want 0 when SNMP is invalid", res.TonerLevelPercent)
		}
		if len(res.BlockingFaults) > 0 {
			t.Errorf("expected no blocking faults, got %v", res.BlockingFaults)
		}
		if res.SourceAttribution.SNMP.Available {
			t.Error("SNMP attribution Available must be false when invalid")
		}
	})

	t.Run("snmp_valid_accepts_hardware_faults", func(t *testing.T) {
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "ERROR",
			PaperJam:          true,
			PaperTrayStatus:   "EMPTY",
			TonerLevelPercent: 50,
			DrumLifePercent:   60,
			UpdatedAt:         freshTime,
		}

		res := ReconcilePrinterHealth(cups, ipp, snmp, evalTime, SourceValidity{
			CUPSValid: true,
			SNMPValid: true,
		})

		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Error("PaperJam must be true when SNMP is valid")
		}
		if res.TonerLevelPercent != 50 {
			t.Errorf("TonerLevelPercent = %d, want 50", res.TonerLevelPercent)
		}
		if !res.SourceAttribution.SNMP.Available {
			t.Error("SNMP attribution Available must be true when valid")
		}
	})
}

