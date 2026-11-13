package printer

import (
	"context"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// CombineHealth merges CUPS queue availability with SNMP device health telemetry.
//
// Authoritative separation of responsibilities & fault hierarchy:
// 1. Physical hardware faults (paper jam, paper empty, cover/door open, output bin full,
//    or confirmed critical hardware error) take HIGHEST priority and immediately override
//    active queue execution (including CUPS PRINTING) to ERROR. A blocked printer cannot print.
// 2. When no blocking physical fault exists:
//    - CUPS is authoritative for queue status and active print job execution.
//      If CUPS is actively PRINTING, the combined state remains PRINTING.
//    - If CUPS is IDLE or READY, SNMP WARMING_UP transitions to WARMING_UP, otherwise IDLE.
// 3. SNMP is authoritative for physical hardware sensors:
//    - TonerLevelPercent
//    - DrumLifePercent
//    - CoverOpen (door open)
//    - PaperJam (jammed)
//    - OutputBinFull (output tray full)
//    - PaperTrayStatus (empty/low paper)
//    Hardware telemetry, error details, and sensor flags are preserved across all states.
// 4. When SNMP is unavailable (network timeout / SNMP disabled):
//    - The printer remains online based on CUPS availability without disrupting the kiosk.
//    - Available CUPS status is preserved without claiming verified hardware health
//      (sensor levels remain 0%, hardware health is unverified).
// 5. SNMP hrPrinterStatus=1 ("other") is NOT interpreted as healthy or actively printing.
//    If SNMP returns status=1, it is treated as UNKNOWN and must never override CUPS queue status
//    or falsely claim verified hardware health.
func CombineHealth(cupsHealth contracts.PrinterHealth, snmpHealth contracts.PrinterHealth) contracts.PrinterHealth {
	now := time.Now().UTC()

	// Case 1: Both subsystems report offline
	if !cupsHealth.Online && !snmpHealth.Online {
		combinedErr := cupsHealth.ErrorMessage
		if snmpHealth.ErrorMessage != "" && !strings.Contains(combinedErr, snmpHealth.ErrorMessage) {
			if combinedErr != "" {
				combinedErr += "; " + snmpHealth.ErrorMessage
			} else {
				combinedErr = snmpHealth.ErrorMessage
			}
		}
		finalState := "OFFLINE"
		if cupsHealth.PaperJam || snmpHealth.PaperJam || cupsHealth.CoverOpen || snmpHealth.CoverOpen || cupsHealth.OutputBinFull || snmpHealth.OutputBinFull {
			finalState = "ERROR"
		}
		return contracts.PrinterHealth{
			Online:            false,
			State:             finalState,
			PaperTrayStatus:   "UNKNOWN",
			EstimatedSheets:   cupsHealth.EstimatedSheets,
			TonerLevelPercent: snmpHealth.TonerLevelPercent,
			DrumLifePercent:   snmpHealth.DrumLifePercent,
			CoverOpen:         snmpHealth.CoverOpen || cupsHealth.CoverOpen,
			PaperJam:          snmpHealth.PaperJam || cupsHealth.PaperJam,
			OutputBinFull:     snmpHealth.OutputBinFull || cupsHealth.OutputBinFull,
			ErrorMessage:      combinedErr,
			UpdatedAt:         now,
		}
	}

	// Case 2: CUPS queue is offline / disabled. Spooler cannot send jobs.
	if !cupsHealth.Online {
		combinedErr := cupsHealth.ErrorMessage
		if snmpHealth.ErrorMessage != "" && !strings.Contains(combinedErr, snmpHealth.ErrorMessage) {
			if combinedErr != "" {
				combinedErr += "; " + snmpHealth.ErrorMessage
			} else {
				combinedErr = snmpHealth.ErrorMessage
			}
		}
		return contracts.PrinterHealth{
			Online:            false,
			State:             cupsHealth.State,
			PaperTrayStatus:   snmpHealth.PaperTrayStatus,
			EstimatedSheets:   cupsHealth.EstimatedSheets,
			TonerLevelPercent: snmpHealth.TonerLevelPercent,
			DrumLifePercent:   snmpHealth.DrumLifePercent,
			CoverOpen:         snmpHealth.CoverOpen || cupsHealth.CoverOpen,
			PaperJam:          snmpHealth.PaperJam || cupsHealth.PaperJam,
			OutputBinFull:     snmpHealth.OutputBinFull || cupsHealth.OutputBinFull,
			ErrorMessage:      combinedErr,
			UpdatedAt:         now,
		}
	}

	// Case 3: CUPS is online, but SNMP is unreachable. Maintain printer readiness from CUPS without claiming verified hardware health.
	if !snmpHealth.Online {
		paperJam := cupsHealth.PaperJam
		coverOpen := cupsHealth.CoverOpen
		outputBinFull := cupsHealth.OutputBinFull
		paperTray := cupsHealth.PaperTrayStatus

		lowerCUPS := strings.ToLower(cupsHealth.ErrorMessage)
		if strings.Contains(lowerCUPS, "jam") {
			paperJam = true
		}
		if strings.Contains(lowerCUPS, "door open") || strings.Contains(lowerCUPS, "cover open") {
			coverOpen = true
		}
		if strings.Contains(lowerCUPS, "out of paper") || strings.Contains(lowerCUPS, "media-empty") || strings.Contains(lowerCUPS, "no paper") {
			paperTray = "EMPTY"
		}
		if strings.Contains(lowerCUPS, "output bin full") {
			outputBinFull = true
		}

		state := cupsHealth.State
		if paperJam || coverOpen || outputBinFull || paperTray == "EMPTY" || cupsHealth.State == "ERROR" {
			state = "ERROR"
		} else if state == "READY" {
			state = "IDLE"
		}

		combinedErr := cupsHealth.ErrorMessage
		if snmpHealth.ErrorMessage != "" && !strings.Contains(combinedErr, snmpHealth.ErrorMessage) {
			if combinedErr != "" {
				combinedErr += "; " + snmpHealth.ErrorMessage
			} else {
				combinedErr = snmpHealth.ErrorMessage
			}
		}

		return contracts.PrinterHealth{
			Online:            true,
			State:             state,
			PaperTrayStatus:   paperTray,
			EstimatedSheets:   cupsHealth.EstimatedSheets,
			TonerLevelPercent: 0,
			DrumLifePercent:   0,
			CoverOpen:         coverOpen,
			PaperJam:          paperJam,
			OutputBinFull:     outputBinFull,
			ErrorMessage:      combinedErr,
			UpdatedAt:         now,
		}
	}

	// Case 4: Both CUPS and SNMP are online.
	tonerPct := snmpHealth.TonerLevelPercent
	drumPct := snmpHealth.DrumLifePercent
	coverOpen := snmpHealth.CoverOpen || cupsHealth.CoverOpen
	paperJam := snmpHealth.PaperJam || cupsHealth.PaperJam
	outputBinFull := snmpHealth.OutputBinFull || cupsHealth.OutputBinFull

	// Consolidate non-duplicate error messages
	var combinedIssues []string
	if cupsHealth.ErrorMessage != "" {
		combinedIssues = append(combinedIssues, cupsHealth.ErrorMessage)
	}
	if snmpHealth.ErrorMessage != "" {
		for _, issue := range strings.Split(snmpHealth.ErrorMessage, ", ") {
			issue = strings.TrimSpace(issue)
			if issue != "" && !strings.Contains(strings.ToLower(cupsHealth.ErrorMessage), strings.ToLower(issue)) {
				combinedIssues = append(combinedIssues, issue)
			}
		}
	}
	combinedErrStr := strings.ToLower(strings.Join(combinedIssues, "; "))

	// Elevate faults mentioned in combined error strings
	if strings.Contains(combinedErrStr, "jam") {
		paperJam = true
	}
	if strings.Contains(combinedErrStr, "door open") || strings.Contains(combinedErrStr, "cover open") {
		coverOpen = true
	}
	if strings.Contains(combinedErrStr, "output bin full") {
		outputBinFull = true
	}

	// Physical paper status resolution:
	// If either reports EMPTY, tray is empty (critical fault).
	paperTray := "OK"
	if snmpHealth.PaperTrayStatus == "EMPTY" || cupsHealth.PaperTrayStatus == "EMPTY" ||
		strings.Contains(combinedErrStr, "out of paper") || strings.Contains(combinedErrStr, "no paper") {
		paperTray = "EMPTY"
	} else if snmpHealth.PaperTrayStatus == "LOW" || cupsHealth.PaperTrayStatus == "LOW" {
		paperTray = "LOW"
	} else if snmpHealth.PaperTrayStatus != "" && snmpHealth.PaperTrayStatus != "OK" && snmpHealth.PaperTrayStatus != "UNKNOWN" {
		paperTray = snmpHealth.PaperTrayStatus
	} else if cupsHealth.PaperTrayStatus != "" && cupsHealth.PaperTrayStatus != "OK" && cupsHealth.PaperTrayStatus != "UNKNOWN" {
		paperTray = cupsHealth.PaperTrayStatus
	} else if snmpHealth.PaperTrayStatus == "OK" || cupsHealth.PaperTrayStatus == "OK" {
		paperTray = "OK"
	} else {
		paperTray = "UNKNOWN"
	}

	// Determine if a confirmed physical fault or critical error exists
	hasCriticalError := snmpHealth.State == "ERROR" || cupsHealth.State == "ERROR"
	hasPhysicalFault := paperJam || coverOpen || outputBinFull || paperTray == "EMPTY" || hasCriticalError

	// State determination:
	// Priority 1: Confirmed physical faults override CUPS PRINTING and all other states.
	// Priority 2: When no blocking fault exists, preserve CUPS authority for queue/job execution.
	var finalState string
	if hasPhysicalFault {
		finalState = "ERROR"
	} else {
		if cupsHealth.State == "PRINTING" {
			finalState = "PRINTING"
		} else if snmpHealth.State == "WARMING_UP" {
			finalState = "WARMING_UP"
		} else if cupsHealth.State == "READY" || cupsHealth.State == "IDLE" {
			finalState = "IDLE"
		} else if cupsHealth.State != "" && cupsHealth.State != "UNKNOWN" {
			finalState = cupsHealth.State
		} else if snmpHealth.State == "IDLE" {
			finalState = "IDLE"
		} else {
			finalState = "UNKNOWN"
		}
	}

	// Ensure error messages reflect confirmed physical faults if not already described
	if paperJam && !strings.Contains(combinedErrStr, "jam") {
		combinedIssues = append(combinedIssues, "paper jam")
	}
	if coverOpen && !strings.Contains(combinedErrStr, "door open") && !strings.Contains(combinedErrStr, "cover open") {
		combinedIssues = append(combinedIssues, "door open")
	}
	if outputBinFull && !strings.Contains(combinedErrStr, "output bin full") {
		combinedIssues = append(combinedIssues, "output bin full")
	}
	if paperTray == "EMPTY" && !strings.Contains(combinedErrStr, "paper") && !strings.Contains(combinedErrStr, "empty") {
		combinedIssues = append(combinedIssues, "out of paper")
	}

	return contracts.PrinterHealth{
		Online:            true,
		State:             finalState,
		PaperTrayStatus:   paperTray,
		EstimatedSheets:   cupsHealth.EstimatedSheets,
		TonerLevelPercent: tonerPct,
		DrumLifePercent:   drumPct,
		CoverOpen:         coverOpen,
		PaperJam:          paperJam,
		OutputBinFull:     outputBinFull,
		ErrorMessage:      strings.Join(combinedIssues, ", "),
		UpdatedAt:         now,
	}
}

// PollCombinedHealth polls both CUPS queue status and SNMP hardware telemetry and combines them.
func PollCombinedHealth(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	snmpCfg SNMPConfig,
	estimatedSheets int,
) contracts.PrinterHealth {
	hasCUPS := strings.TrimSpace(queue) != ""
	hasSNMP := strings.TrimSpace(snmpCfg.Target) != ""

	if hasCUPS && !hasSNMP {
		if runner != nil {
			return PollCUPSHealthCustom(ctx, runner, queue, estimatedSheets)
		}
		return PollCUPSHealth(ctx, queue, estimatedSheets)
	}
	if !hasCUPS && hasSNMP {
		return PollHealthConfig(ctx, snmpCfg, estimatedSheets)
	}
	if !hasCUPS && !hasSNMP {
		return offlineHealth(estimatedSheets, "neither CUPS queue nor printer address configured")
	}

	// Both CUPS and SNMP configured
	var cupsHealth contracts.PrinterHealth
	if runner != nil {
		cupsHealth = PollCUPSHealthCustom(ctx, runner, queue, estimatedSheets)
	} else {
		cupsHealth = PollCUPSHealth(ctx, queue, estimatedSheets)
	}

	snmpHealth := PollHealthConfig(ctx, snmpCfg, estimatedSheets)

	return CombineHealth(cupsHealth, snmpHealth)
}
