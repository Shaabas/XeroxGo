package printer

import (
	"reflect"
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

	// Contradiction must be documented: CUPS reports PRINTING while physical fault present
	if !sliceHasItemContaining(res.Contradictions, "PRINTING while physical hardware fault is present") {
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

	t.Run("unconfirmed_media_jam_warning_is_nonblocking", func(t *testing.T) {
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

		res := ReconcilePrinterHealth(baseCUPS, ipp, baseSNMP, evalTime)

		// Not blocking, so state remains IDLE
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE (unconfirmed warning is nonblocking)", res.EffectiveState)
		}
		if res.HasBlockingFaults() {
			t.Errorf("expected no blocking faults, got %v", res.BlockingFaults)
		}
		if !sliceHasItemContaining(res.NonblockingWarnings, "media-jam") {
			t.Errorf("expected media-jam in NonblockingWarnings, got %v", res.NonblockingWarnings)
		}
	})

	t.Run("unconfirmed_media_jam_report_is_nonblocking", func(t *testing.T) {
		ipp := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "BrotherQueue",
			State:             IPPPrinterStateIdle,
			RawReasons:        []string{"media-jam-report"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-report", Keyword: "media-jam", Severity: SeverityReport},
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
		if !sliceHasItemContaining(res.Contradictions, "CUPS reports queue PRINTING while IPP reports printer-state \"idle\"") {
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
		if !sliceHasItemContaining(res.Contradictions, "IPP reports paper jam while SNMP reports no paper jam") {
			t.Errorf("expected contradiction regarding IPP jam vs SNMP normal, got %v", res.Contradictions)
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
		if !sliceHasItemContaining(res.Contradictions, "SNMP reports paper jam while IPP reports no paper jam") {
			t.Errorf("expected contradiction regarding SNMP jam vs IPP normal, got %v", res.Contradictions)
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
		if !sliceHasItemContaining(res.Contradictions, "CUPS reports paper tray EMPTY while SNMP reports paper tray OK") {
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
