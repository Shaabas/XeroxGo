package printer

import (
	"context"
	"strings"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestCombineHealth(t *testing.T) {
	t.Run("both_online_idle_and_healthy", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 150,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   150,
			TonerLevelPercent: 82,
			DrumLifePercent:   90,
			CoverOpen:         false,
			PaperJam:          false,
			OutputBinFull:     false,
			UpdatedAt:         time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if !res.Online {
			t.Error("expected Online = true")
		}
		if res.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", res.State)
		}
		if res.TonerLevelPercent != 82 {
			t.Errorf("TonerLevelPercent = %d, want 82", res.TonerLevelPercent)
		}
		if res.DrumLifePercent != 90 {
			t.Errorf("DrumLifePercent = %d, want 90", res.DrumLifePercent)
		}
		if res.PaperTrayStatus != "OK" {
			t.Errorf("PaperTrayStatus = %s, want OK", res.PaperTrayStatus)
		}
		if res.ErrorMessage != "" {
			t.Errorf("expected empty ErrorMessage, got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_printing_state_authoritative_over_snmp_idle", func(t *testing.T) {
		// When no blocking physical fault exists, CUPS is authoritative for queue/job execution.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 145,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   145,
			TonerLevelPercent: 80,
			DrumLifePercent:   90,
			UpdatedAt:         time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "PRINTING" {
			t.Errorf("State = %s, want PRINTING (CUPS authoritative)", res.State)
		}
		if res.TonerLevelPercent != 80 {
			t.Errorf("TonerLevelPercent = %d, want 80", res.TonerLevelPercent)
		}
	})

	t.Run("cups_printing_with_snmp_hardware_jam_overrides_to_error", func(t *testing.T) {
		// Hardware fault priority: confirmed physical paper jam overrides CUPS PRINTING to ERROR.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 140,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 140,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR (confirmed physical jam overrides PRINTING)", res.State)
		}
		if !res.PaperJam {
			t.Error("expected PaperJam = true")
		}
		if !strings.Contains(strings.ToLower(res.ErrorMessage), "paper jam") {
			t.Errorf("expected ErrorMessage to contain 'paper jam', got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_printing_with_snmp_empty_tray_overrides_to_error", func(t *testing.T) {
		// Physical paper empty overrides active CUPS PRINTING to ERROR.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 10,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperTrayStatus: "EMPTY",
			ErrorMessage:    "no paper",
			EstimatedSheets: 0,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR (empty tray overrides PRINTING)", res.State)
		}
		if res.PaperTrayStatus != "EMPTY" {
			t.Errorf("PaperTrayStatus = %s, want EMPTY", res.PaperTrayStatus)
		}
		if !strings.Contains(strings.ToLower(res.ErrorMessage), "no paper") && !strings.Contains(strings.ToLower(res.ErrorMessage), "out of paper") {
			t.Errorf("expected ErrorMessage to describe empty paper, got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_printing_with_snmp_cover_open_overrides_to_error", func(t *testing.T) {
		// Door/cover open overrides active CUPS PRINTING to ERROR.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 120,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			CoverOpen:       true,
			ErrorMessage:    "door open",
			EstimatedSheets: 120,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR (cover open overrides PRINTING)", res.State)
		}
		if !res.CoverOpen {
			t.Error("expected CoverOpen = true")
		}
		if !strings.Contains(strings.ToLower(res.ErrorMessage), "door open") {
			t.Errorf("expected ErrorMessage to contain 'door open', got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_printing_with_snmp_output_bin_full_overrides_to_error", func(t *testing.T) {
		// Output bin full overrides active CUPS PRINTING to ERROR.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			OutputBinFull:   true,
			ErrorMessage:    "output bin full",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR (output bin full overrides PRINTING)", res.State)
		}
		if !res.OutputBinFull {
			t.Error("expected OutputBinFull = true")
		}
	})

	t.Run("cups_idle_snmp_cover_open_elevates_to_error", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			CoverOpen:       true,
			ErrorMessage:    "door open",
			EstimatedSheets: 200,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR", res.State)
		}
		if !res.CoverOpen {
			t.Error("expected CoverOpen = true")
		}
		if !strings.Contains(res.ErrorMessage, "door open") {
			t.Errorf("expected ErrorMessage to contain 'door open', got %q", res.ErrorMessage)
		}
	})

	t.Run("cups_idle_snmp_output_bin_full_elevates_to_error", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 180,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			OutputBinFull:   true,
			ErrorMessage:    "output bin full",
			EstimatedSheets: 180,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR", res.State)
		}
		if !res.OutputBinFull {
			t.Error("expected OutputBinFull = true")
		}
	})

	t.Run("cups_online_snmp_unreachable_maintains_cups_online", func(t *testing.T) {
		// When SNMP is unreachable, preserve available CUPS operational status without claiming verified hardware health.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "SNMP connection timeout",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if !res.Online {
			t.Error("expected Online = true based on CUPS queue operational state")
		}
		if res.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", res.State)
		}
		if res.TonerLevelPercent != 0 {
			t.Errorf("TonerLevelPercent = %d, want 0 (unavailable)", res.TonerLevelPercent)
		}
		if res.CoverOpen || res.PaperJam || res.OutputBinFull {
			t.Error("expected false for unverified hardware sensor flags")
		}
	})

	t.Run("cups_error_snmp_unreachable_elevates_error", func(t *testing.T) {
		// When SNMP is unreachable, CUPS physical error (e.g. jam) is preserved as ERROR.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam detected",
			EstimatedSheets: 50,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "SNMP unreachable",
			EstimatedSheets: 50,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR", res.State)
		}
		if !res.PaperJam {
			t.Error("expected PaperJam = true from CUPS")
		}
	})

	t.Run("cups_disabled_offline_marks_printer_unavailable", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			ErrorMessage:    "cups queue disabled",
			EstimatedSheets: 50,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			TonerLevelPercent: 75,
			EstimatedSheets:   50,
			UpdatedAt:         time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.Online {
			t.Error("expected Online = false when CUPS queue is disabled")
		}
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR", res.State)
		}
		if res.TonerLevelPercent != 75 {
			t.Errorf("TonerLevelPercent = %d, want 75 (telemetry preserved)", res.TonerLevelPercent)
		}
	})

	t.Run("both_offline", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "printer not found in cups",
			EstimatedSheets: 0,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			ErrorMessage:    "snmp connection refused",
			EstimatedSheets: 0,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.Online {
			t.Error("expected Online = false")
		}
		if res.State != "OFFLINE" {
			t.Errorf("State = %s, want OFFLINE", res.State)
		}
		if !strings.Contains(res.ErrorMessage, "cups") || !strings.Contains(res.ErrorMessage, "snmp") {
			t.Errorf("expected combined error message, got %q", res.ErrorMessage)
		}
	})

	t.Run("snmp_hrprinterstatus_1_other_with_jam_reports_error", func(t *testing.T) {
		// Verified Raspberry Pi hardware scenario:
		// HP LaserJet Tank 1020w returns hrPrinterStatus=1 (other) and detected error 0x04 (jam).
		// CUPS simultaneously reports printing HPTank-34.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		// SNMP mapped from hrPrinterStatus=1 (UNKNOWN) with error bit Jammed (ERROR)
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR (hrPrinterStatus=1 + jam must override CUPS PRINTING)", res.State)
		}
		if !res.PaperJam {
			t.Error("expected PaperJam = true")
		}
	})

	t.Run("snmp_hrprinterstatus_1_other_without_fault_does_not_override_cups", func(t *testing.T) {
		// When SNMP returns hrPrinterStatus=1 without any error bits (State="UNKNOWN"),
		// it must NOT be interpreted as healthy or printing, but must not prevent CUPS queue authority.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "UNKNOWN",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "PRINTING" {
			t.Errorf("State = %s, want PRINTING (CUPS authoritative for execution when no fault exists)", res.State)
		}

		// When CUPS is IDLE and SNMP is UNKNOWN:
		cupsIdle := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		resIdle := CombineHealth(cupsIdle, snmp)
		if resIdle.State != "IDLE" {
			t.Errorf("State = %s, want IDLE (CUPS authoritative for queue status)", resIdle.State)
		}
	})

	t.Run("contradictory_cups_jam_and_snmp_idle_reports_error", func(t *testing.T) {
		// When CUPS reports a paper jam but SNMP reports IDLE (e.g. SNMP lag),
		// the physical fault detected by CUPS must NOT be overwritten by SNMP IDLE.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 80,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:            true,
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			EstimatedSheets:   80,
			TonerLevelPercent: 90,
			UpdatedAt:         time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "ERROR" {
			t.Errorf("State = %s, want ERROR (CUPS-detected jam must not be cleared by SNMP IDLE)", res.State)
		}
		if !res.PaperJam {
			t.Error("expected PaperJam = true")
		}
	})

	t.Run("contradictory_cups_idle_and_snmp_printing_preserves_cups_idle", func(t *testing.T) {
		// When CUPS queue is IDLE (no active job) but SNMP reports PRINTING (internal calibration),
		// CUPS authority for queue execution governs.
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 150,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 150,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "IDLE" {
			t.Errorf("State = %s, want IDLE (CUPS authoritative for queue status)", res.State)
		}
	})

	t.Run("cups_idle_snmp_warming_up_transitions_to_warming_up", func(t *testing.T) {
		cups := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       time.Now().UTC(),
		}
		snmp := contracts.PrinterHealth{
			Online:          true,
			State:           "WARMING_UP",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       time.Now().UTC(),
		}

		res := CombineHealth(cups, snmp)
		if res.State != "WARMING_UP" {
			t.Errorf("State = %s, want WARMING_UP", res.State)
		}
	})

	t.Run("recovery_from_jam_restores_idle_or_printing", func(t *testing.T) {
		// 1. Initial jammed state
		cupsJammed := contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		snmpJammed := contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		resJammed := CombineHealth(cupsJammed, snmpJammed)
		if resJammed.State != "ERROR" || !resJammed.PaperJam {
			t.Fatalf("expected jammed state ERROR, got %s (jam=%v)", resJammed.State, resJammed.PaperJam)
		}

		// 2. Recovery: jam cleared, printer returns to IDLE
		cupsRecovered := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		snmpRecovered := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperJam:        false,
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
		resRecovered := CombineHealth(cupsRecovered, snmpRecovered)
		if resRecovered.State != "IDLE" {
			t.Errorf("State = %s, want IDLE after jam cleared", resRecovered.State)
		}
		if resRecovered.PaperJam {
			t.Error("expected PaperJam = false after jam cleared")
		}
		if resRecovered.ErrorMessage != "" {
			t.Errorf("expected empty ErrorMessage after recovery, got %q", resRecovered.ErrorMessage)
		}
	})
}

func TestPollCombinedHealthSubsystems(t *testing.T) {
	t.Run("neither_configured", func(t *testing.T) {
		res := PollCombinedHealth(context.Background(), nil, "", SNMPConfig{}, 100)
		if res.Online {
			t.Error("expected Online = false when neither configured")
		}
		if res.State != "OFFLINE" {
			t.Errorf("State = %s, want OFFLINE", res.State)
		}
	})

	t.Run("cups_only_custom_runner", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("printer DCPT310 is idle. enabled since Thu Jan 01 00:00:00 2026"), nil
		}

		res := PollCombinedHealth(context.Background(), runner, "DCPT310", SNMPConfig{}, 150)
		if !res.Online {
			t.Error("expected Online = true")
		}
		if res.State != "IDLE" {
			t.Errorf("State = %s, want IDLE", res.State)
		}
	})
}
