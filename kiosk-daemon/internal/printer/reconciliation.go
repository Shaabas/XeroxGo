package printer

import (
	"fmt"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// MaxIPPObservationAge defines the maximum allowable age for an IPP observation.
// An observation older than this duration relative to the evaluation time
// is rejected as stale and cannot provide authoritative state or active faults.
const MaxIPPObservationAge = 30 * time.Second

// FutureTimestampTolerance defines the maximum allowable clock skew for an observation
// timestamp in the future. Observations further in the future than this tolerance
// are rejected as invalid.
const FutureTimestampTolerance = 5 * time.Second

// HealthSource identifies the reporting subsystem for a health observation.
type HealthSource string

const (
	SourceCUPS       HealthSource = "CUPS"
	SourceIPP        HealthSource = "IPP"
	SourceSNMP       HealthSource = "SNMP"
	SourceReconciler HealthSource = "RECONCILER"
)

// Standard diagnostic fault codes
const (
	FaultPaperJam      = "PAPER_JAM"
	FaultCoverOpen     = "COVER_OPEN"
	FaultMediaEmpty    = "MEDIA_EMPTY"
	FaultOutputBinFull = "OUTPUT_BIN_FULL"
	FaultMarkerEmpty   = "MARKER_EMPTY"
	FaultHardwareError = "HARDWARE_ERROR"
	FaultQueueOffline  = "QUEUE_OFFLINE"
	FaultQueueStopped  = "QUEUE_STOPPED"

	WarningLowToner    = "LOW_TONER"
	WarningLowPaper    = "LOW_PAPER"
	WarningLowDrum     = "LOW_DRUM"
	WarningStaleIPP    = "STALE_IPP_OBSERVATION"
	WarningIPPReason   = "IPP_WARNING"

	UnknownSNMP        = "SNMP_TELEMETRY_UNAVAILABLE"
	UnknownIPP         = "IPP_TELEMETRY_UNAVAILABLE"
	UnknownPaperTray   = "PAPER_TRAY_UNKNOWN"
	UnknownQueueState  = "QUEUE_STATE_UNKNOWN"

	ContradictionJobState     = "CONTRADICTION_JOB_STATE"
	ContradictionHardwareFault = "CONTRADICTION_HARDWARE_FAULT"
	ContradictionOnlineStatus = "CONTRADICTION_ONLINE_STATUS"
)

// SubsystemAttribution records the diagnostic findings and operational state
// reported by an individual subsystem.
type SubsystemAttribution struct {
	Queried    bool     `json:"queried"`
	Available  bool     `json:"available"`
	State      string   `json:"state"`
	Faults     []string `json:"faults"`
	Warnings   []string `json:"warnings"`
	RawDetails string   `json:"raw_details,omitempty"`
}

// SourceAttribution records the observations attributed to each reporting subsystem.
type SourceAttribution struct {
	CUPS SubsystemAttribution `json:"cups"`
	IPP  SubsystemAttribution `json:"ipp"`
	SNMP SubsystemAttribution `json:"snmp"`
}

// HealthCategory categorizes diagnostic observations into faults, warnings,
// unknown conditions, or contradictions.
type HealthCategory string

const (
	CategoryFault         HealthCategory = "FAULT"
	CategoryWarning       HealthCategory = "WARNING"
	CategoryUnknown       HealthCategory = "UNKNOWN"
	CategoryContradiction HealthCategory = "CONTRADICTION"
)

// DiagnosticItem represents an individual structured diagnostic finding.
type DiagnosticItem struct {
	Category HealthCategory `json:"category"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Source   HealthSource   `json:"source"`
	RawToken string         `json:"raw_token,omitempty"`
	Severity string         `json:"severity"` // "BLOCKING", "NONBLOCKING", "INFO"
}

// ReconciledHealthResult is the comprehensive diagnostic result produced by
// reconciling CUPS, IPP, and SNMP observations.
type ReconciledHealthResult struct {
	// EffectiveState is the resolved operational state:
	// "IDLE", "PRINTING", "WARMING_UP", "ERROR", "OFFLINE", "UNKNOWN".
	EffectiveState string `json:"effective_state"`

	// Online indicates overall queue/daemon operational reachability.
	Online bool `json:"online"`

	// BlockingFaults contains confirmed critical hardware or queue faults that prevent printing.
	BlockingFaults []string `json:"blocking_faults"`

	// NonblockingWarnings contains non-critical warnings that do not stop printing (e.g. low toner).
	NonblockingWarnings []string `json:"nonblocking_warnings"`

	// UnknownConditions contains unverified telemetry or missing subsystem data.
	UnknownConditions []string `json:"unknown_conditions"`

	// Contradictions lists detected discrepancies between CUPS, IPP, and SNMP observations.
	Contradictions []string `json:"contradictions"`

	// SourceAttribution attributes findings and subsystem status to CUPS, IPP, and SNMP.
	SourceAttribution SourceAttribution `json:"source_attribution"`

	// RawIPPReasons preserves the exact raw reason tokens from IPP for diagnostics.
	RawIPPReasons []string `json:"raw_ipp_reasons,omitempty"`

	// Physical telemetry and consumable levels:
	PaperJam          bool   `json:"paper_jam"`
	CoverOpen         bool   `json:"cover_open"`
	OutputBinFull     bool   `json:"output_bin_full"`
	PaperTrayStatus   string `json:"paper_tray_status"` // "OK", "LOW", "EMPTY", "UNKNOWN"
	EstimatedSheets   int    `json:"estimated_sheets"`
	TonerLevelPercent int    `json:"toner_level_percent"`
	DrumLifePercent   int    `json:"drum_life_percent"`

	// EvaluatedAt is the timestamp when reconciliation was evaluated.
	EvaluatedAt time.Time `json:"evaluated_at"`

	// DiagnosticDetails provides structured diagnostic records with code, source, severity, and raw tokens.
	DiagnosticDetails []DiagnosticItem `json:"diagnostic_details,omitempty"`
}

// HasBlockingFaults returns true if there are one or more confirmed blocking faults.
func (r ReconciledHealthResult) HasBlockingFaults() bool {
	return len(r.BlockingFaults) > 0
}

// IsReady returns true only if the printer is online, free of blocking faults, and in an idle/ready state.
func (r ReconciledHealthResult) IsReady() bool {
	return r.Online && len(r.BlockingFaults) == 0 && (r.EffectiveState == "IDLE" || r.EffectiveState == "READY")
}

// ReconcilePrinterHealth performs a pure, multi-source health reconciliation across
// CUPS queue health, IPP printer-state attributes, and SNMP hardware telemetry.
//
// Rules and invariants:
//  1. Pure function: does not perform network/subprocess I/O, does not query hardware,
//     and does not mutate any inputs.
//  2. CUPS is authoritative for queue/job execution.
//  3. Confirmed critical hardware faults override active printing in the diagnostic result,
//     reporting EffectiveState="ERROR" without interrupting actual print jobs.
//  4. IPP reasons are evaluated by severity:
//     - SeverityError is critical/blocking.
//     - SeverityWarning and SeverityReport are nonblocking warnings unless independently
//       confirmed as critical by SNMP or CUPS.
//  5. SNMP paper jam, open cover, full output bin, empty paper tray, and hardware errors are preserved.
//  6. Consumable percentages (toner/drum) are preserved as observations. Missing/zero telemetry
//     is never interpreted as proof of consumable faults.
//  7. Unknown/missing data is kept strictly distinct from confirmed healthy.
//  8. Stale IPP observations (age > MaxIPPObservationAge), zero timestamps, and future timestamps
//     are rejected from authoritative fault evaluation.
//  9. IPP EndpointReachable alone is never interpreted as printer readiness.
// 10. Contradictions between subsystems are explicitly detected and reported, and never resolved
//     by suppressing confirmed critical fault evidence.
// 11. Deterministic output: evaluation of identical inputs always produces identical results.
func ReconcilePrinterHealth(
	cups contracts.PrinterHealth,
	ipp IPPPrinterStateResult,
	snmp contracts.PrinterHealth,
	evalTime time.Time,
) ReconciledHealthResult {
	if evalTime.IsZero() {
		evalTime = time.Now().UTC()
	}

	blockingFaults := make([]string, 0)
	nonblockingWarnings := make([]string, 0)
	unknownConditions := make([]string, 0)
	contradictions := make([]string, 0)
	diagnosticDetails := make([]DiagnosticItem, 0)

	// Preserve raw IPP reasons for auditing
	rawIPPReasons := make([]string, 0)
	if len(ipp.RawReasons) > 0 {
		rawIPPReasons = make([]string, len(ipp.RawReasons))
		copy(rawIPPReasons, ipp.RawReasons)
	} else if len(ipp.ParsedReasons) > 0 {
		for _, pr := range ipp.ParsedReasons {
			if pr.Raw != "" {
				rawIPPReasons = append(rawIPPReasons, pr.Raw)
			}
		}
	}

	// ---------------------------------------------------------
	// 1. IPP Freshness & Transport Validation
	// ---------------------------------------------------------
	var ippValid bool
	var ippFreshnessIssue string

	if ipp.ObservedAt.IsZero() {
		ippFreshnessIssue = "IPP observation timestamp is zero (missing or uninitialized)"
		unknownConditions = append(unknownConditions, ippFreshnessIssue)
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryUnknown,
			Code:     UnknownIPP,
			Message:  ippFreshnessIssue,
			Source:   SourceIPP,
			Severity: "INFO",
		})
	} else if ipp.ObservedAt.After(evalTime.Add(FutureTimestampTolerance)) {
		skew := ipp.ObservedAt.Sub(evalTime).Round(time.Millisecond)
		ippFreshnessIssue = fmt.Sprintf("IPP observation timestamp is in the future (skew: %s)", skew)
		unknownConditions = append(unknownConditions, ippFreshnessIssue)
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryUnknown,
			Code:     UnknownIPP,
			Message:  ippFreshnessIssue,
			Source:   SourceIPP,
			Severity: "INFO",
		})
	} else if evalTime.Sub(ipp.ObservedAt) > MaxIPPObservationAge {
		age := evalTime.Sub(ipp.ObservedAt).Round(time.Millisecond)
		ippFreshnessIssue = fmt.Sprintf("IPP observation is stale (age %s exceeds max %s)", age, MaxIPPObservationAge)
		nonblockingWarnings = append(nonblockingWarnings, ippFreshnessIssue)
		unknownConditions = append(unknownConditions, fmt.Sprintf("stale IPP observation (age %s); fresh state unknown", age))
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryWarning,
			Code:     WarningStaleIPP,
			Message:  ippFreshnessIssue,
			Source:   SourceIPP,
			Severity: "NONBLOCKING",
		})
	}

	if ippFreshnessIssue != "" {
		ippValid = false
	} else if ipp.Error != "" {
		ippValid = false
		msg := fmt.Sprintf("IPP state query failed: %s", ipp.Error)
		unknownConditions = append(unknownConditions, msg)
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryUnknown,
			Code:     UnknownIPP,
			Message:  msg,
			Source:   SourceIPP,
			Severity: "INFO",
		})
	} else if !ipp.EndpointReachable {
		ippValid = false
		unknownConditions = append(unknownConditions, "IPP endpoint unreachable")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryUnknown,
			Code:     UnknownIPP,
			Message:  "IPP endpoint unreachable",
			Source:   SourceIPP,
			Severity: "INFO",
		})
	} else if !ipp.HasRecognizedStateAttributes() {
		ippValid = false
		unknownConditions = append(unknownConditions, "IPP response contains no recognized printer state attributes")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryUnknown,
			Code:     UnknownIPP,
			Message:  "IPP response contains no recognized printer state attributes",
			Source:   SourceIPP,
			Severity: "INFO",
		})
	} else {
		ippValid = true
	}

	// ---------------------------------------------------------
	// 2. CUPS Subsystem Analysis
	// ---------------------------------------------------------
	cupsFaults := make([]string, 0)
	cupsWarnings := make([]string, 0)
	cupsLowerErr := strings.ToLower(cups.ErrorMessage)

	cupsJam := cups.PaperJam || strings.Contains(cupsLowerErr, "jam")
	cupsCover := cups.CoverOpen || strings.Contains(cupsLowerErr, "door open") || strings.Contains(cupsLowerErr, "cover open")
	cupsOutputFull := cups.OutputBinFull || strings.Contains(cupsLowerErr, "output bin full")
	cupsPaperEmpty := cups.PaperTrayStatus == "EMPTY" ||
		strings.Contains(cupsLowerErr, "out of paper") ||
		strings.Contains(cupsLowerErr, "media-empty") ||
		strings.Contains(cupsLowerErr, "no paper")
	cupsPaperLow := cups.PaperTrayStatus == "LOW" || strings.Contains(cupsLowerErr, "paper low")

	if cupsJam {
		cupsFaults = append(cupsFaults, "paper jam")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryFault,
			Code:     FaultPaperJam,
			Message:  "Paper jam detected via CUPS queue",
			Source:   SourceCUPS,
			Severity: "BLOCKING",
		})
	}
	if cupsCover {
		cupsFaults = append(cupsFaults, "cover or door open")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryFault,
			Code:     FaultCoverOpen,
			Message:  "Cover or door open detected via CUPS queue",
			Source:   SourceCUPS,
			Severity: "BLOCKING",
		})
	}
	if cupsOutputFull {
		cupsFaults = append(cupsFaults, "output bin full")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryFault,
			Code:     FaultOutputBinFull,
			Message:  "Output bin full detected via CUPS queue",
			Source:   SourceCUPS,
			Severity: "BLOCKING",
		})
	}
	if cupsPaperEmpty {
		cupsFaults = append(cupsFaults, "out of paper / media empty")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryFault,
			Code:     FaultMediaEmpty,
			Message:  "Out of paper detected via CUPS queue",
			Source:   SourceCUPS,
			Severity: "BLOCKING",
		})
	}
	if cupsPaperLow {
		cupsWarnings = append(cupsWarnings, "paper tray low")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryWarning,
			Code:     WarningLowPaper,
			Message:  "Paper tray low reported via CUPS queue",
			Source:   SourceCUPS,
			Severity: "NONBLOCKING",
		})
	}
	if cups.State == "ERROR" && len(cupsFaults) == 0 {
		errDesc := fmt.Sprintf("CUPS queue error: %s", cups.ErrorMessage)
		cupsFaults = append(cupsFaults, errDesc)
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryFault,
			Code:     FaultHardwareError,
			Message:  errDesc,
			Source:   SourceCUPS,
			Severity: "BLOCKING",
		})
	}
	if !cups.Online {
		cupsFaults = append(cupsFaults, "CUPS queue offline")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryFault,
			Code:     FaultQueueOffline,
			Message:  "CUPS queue is offline",
			Source:   SourceCUPS,
			Severity: "BLOCKING",
		})
	}

	// ---------------------------------------------------------
	// 3. SNMP Subsystem Analysis
	// ---------------------------------------------------------
	snmpFaults := make([]string, 0)
	snmpWarnings := make([]string, 0)
	var snmpJam, snmpCover, snmpOutputFull, snmpPaperEmpty, snmpPaperLow bool

	if snmp.Online {
		snmpLowerErr := strings.ToLower(snmp.ErrorMessage)
		snmpJam = snmp.PaperJam || strings.Contains(snmpLowerErr, "jam")
		snmpCover = snmp.CoverOpen || strings.Contains(snmpLowerErr, "door open") || strings.Contains(snmpLowerErr, "cover open")
		snmpOutputFull = snmp.OutputBinFull || strings.Contains(snmpLowerErr, "output bin full") || strings.Contains(snmpLowerErr, "output full")
		snmpPaperEmpty = snmp.PaperTrayStatus == "EMPTY" ||
			strings.Contains(snmpLowerErr, "out of paper") ||
			strings.Contains(snmpLowerErr, "no paper") ||
			strings.Contains(snmpLowerErr, "input tray empty")
		snmpPaperLow = snmp.PaperTrayStatus == "LOW" || strings.Contains(snmpLowerErr, "low paper")

		if snmpJam {
			snmpFaults = append(snmpFaults, "paper jam")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryFault,
				Code:     FaultPaperJam,
				Message:  "Paper jam reported by SNMP",
				Source:   SourceSNMP,
				Severity: "BLOCKING",
			})
		}
		if snmpCover {
			snmpFaults = append(snmpFaults, "cover or door open")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryFault,
				Code:     FaultCoverOpen,
				Message:  "Cover or door open reported by SNMP",
				Source:   SourceSNMP,
				Severity: "BLOCKING",
			})
		}
		if snmpOutputFull {
			snmpFaults = append(snmpFaults, "output bin full")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryFault,
				Code:     FaultOutputBinFull,
				Message:  "Output bin full reported by SNMP",
				Source:   SourceSNMP,
				Severity: "BLOCKING",
			})
		}
		if snmpPaperEmpty {
			snmpFaults = append(snmpFaults, "out of paper / media empty")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryFault,
				Code:     FaultMediaEmpty,
				Message:  "Paper tray empty reported by SNMP",
				Source:   SourceSNMP,
				Severity: "BLOCKING",
			})
		}
		if snmpPaperLow {
			snmpWarnings = append(snmpWarnings, "paper tray low")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryWarning,
				Code:     WarningLowPaper,
				Message:  "Paper tray low reported by SNMP",
				Source:   SourceSNMP,
				Severity: "NONBLOCKING",
			})
		}

		if snmp.TonerLevelPercent > 0 && snmp.TonerLevelPercent <= 10 {
			warn := fmt.Sprintf("low toner level (%d%%)", snmp.TonerLevelPercent)
			snmpWarnings = append(snmpWarnings, warn)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryWarning,
				Code:     WarningLowToner,
				Message:  warn,
				Source:   SourceSNMP,
				Severity: "NONBLOCKING",
			})
		}
		if strings.Contains(snmpLowerErr, "low toner") && !sliceHasItemContaining(snmpWarnings, "low toner") {
			snmpWarnings = append(snmpWarnings, "low toner reported")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryWarning,
				Code:     WarningLowToner,
				Message:  "Low toner reported by SNMP",
				Source:   SourceSNMP,
				Severity: "NONBLOCKING",
			})
		}
		if strings.Contains(snmpLowerErr, "no toner") {
			snmpFaults = append(snmpFaults, "toner cartridge empty")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryFault,
				Code:     FaultMarkerEmpty,
				Message:  "Toner cartridge empty reported by SNMP",
				Source:   SourceSNMP,
				Severity: "BLOCKING",
			})
		}
		if snmp.DrumLifePercent > 0 && snmp.DrumLifePercent <= 10 {
			warn := fmt.Sprintf("low drum life (%d%%)", snmp.DrumLifePercent)
			snmpWarnings = append(snmpWarnings, warn)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryWarning,
				Code:     WarningLowDrum,
				Message:  warn,
				Source:   SourceSNMP,
				Severity: "NONBLOCKING",
			})
		}

		if snmp.State == "ERROR" {
			if strings.Contains(snmpLowerErr, "service") || strings.Contains(snmpLowerErr, "hardware") || len(snmpFaults) == 0 {
				hwErr := fmt.Sprintf("hardware error (SNMP): %s", snmp.ErrorMessage)
				snmpFaults = append(snmpFaults, hwErr)
				diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
					Category: CategoryFault,
					Code:     FaultHardwareError,
					Message:  hwErr,
					Source:   SourceSNMP,
					Severity: "BLOCKING",
				})
			}
		} else if snmp.State == "UNKNOWN" {
			unknownConditions = append(unknownConditions, "SNMP reports status UNKNOWN (hrPrinterStatus 1 'other')")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryUnknown,
				Code:     UnknownSNMP,
				Message:  "SNMP reports hrPrinterStatus 1 (other); hardware status unverified",
				Source:   SourceSNMP,
				Severity: "INFO",
			})
		}
	} else {
		// SNMP offline / unreachable
		unknownConditions = append(unknownConditions, "SNMP telemetry unavailable; consumable levels and physical sensor status unverified")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryUnknown,
			Code:     UnknownSNMP,
			Message:  "SNMP telemetry unavailable; consumable levels and physical sensor status unverified",
			Source:   SourceSNMP,
			Severity: "INFO",
		})
	}

	// ---------------------------------------------------------
	// 4. IPP Subsystem Analysis (when valid)
	// ---------------------------------------------------------
	ippFaults := make([]string, 0)
	ippWarnings := make([]string, 0)
	var ippJam, ippCover, ippOutputFull, ippPaperEmpty, ippPaperLow bool

	if ippValid {
		for _, r := range ipp.ParsedReasons {
			kw := strings.ToLower(r.Keyword)
			sev := r.Severity

			isErrorSeverity := (sev == SeverityError) ||
				(sev == SeverityNone && isInherentlyCriticalIPPKeyword(kw))

			isWarningOrReport := (sev == SeverityWarning || sev == SeverityReport)

			if isErrorSeverity {
				ippFaults = append(ippFaults, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
				switch kw {
				case "media-jam":
					ippJam = true
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryFault,
						Code:     FaultPaperJam,
						Message:  "Paper jam reported by IPP error reason",
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "BLOCKING",
					})
				case "cover-open", "door-open":
					ippCover = true
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryFault,
						Code:     FaultCoverOpen,
						Message:  "Cover/door open reported by IPP error reason",
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "BLOCKING",
					})
				case "output-bin-full":
					ippOutputFull = true
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryFault,
						Code:     FaultOutputBinFull,
						Message:  "Output bin full reported by IPP error reason",
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "BLOCKING",
					})
				case "media-empty", "media-needed":
					ippPaperEmpty = true
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryFault,
						Code:     FaultMediaEmpty,
						Message:  "Media empty reported by IPP error reason",
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "BLOCKING",
					})
				case "marker-waste-full":
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryFault,
						Code:     FaultHardwareError,
						Message:  "Waste marker full reported by IPP error reason",
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "BLOCKING",
					})
				default:
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryFault,
						Code:     FaultHardwareError,
						Message:  fmt.Sprintf("Critical hardware fault reported by IPP reason: %s", r.Raw),
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "BLOCKING",
					})
				}
			} else if isWarningOrReport {
				// Nonblocking warning unless independently confirmed as critical by SNMP or CUPS
				isIndependentlyConfirmed := false
				switch kw {
				case "media-jam":
					if cupsJam || (snmp.Online && snmpJam) {
						isIndependentlyConfirmed = true
						ippJam = true
					}
				case "media-empty", "media-needed":
					if cupsPaperEmpty || (snmp.Online && snmpPaperEmpty) {
						isIndependentlyConfirmed = true
						ippPaperEmpty = true
					}
				case "cover-open", "door-open":
					if cupsCover || (snmp.Online && snmpCover) {
						isIndependentlyConfirmed = true
						ippCover = true
					}
				case "output-bin-full":
					if cupsOutputFull || (snmp.Online && snmpOutputFull) {
						isIndependentlyConfirmed = true
						ippOutputFull = true
					}
				}

				if isIndependentlyConfirmed {
					ippFaults = append(ippFaults, fmt.Sprintf("%s (reason: %s, independently confirmed)", kw, r.Raw))
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryFault,
						Code:     fmt.Sprintf("IPP_%s_CONFIRMED", strings.ToUpper(strings.ReplaceAll(kw, "-", "_"))),
						Message:  fmt.Sprintf("IPP reason %s independently confirmed as critical", r.Raw),
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "BLOCKING",
					})
				} else {
					ippWarnings = append(ippWarnings, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
					if kw == "media-low" {
						ippPaperLow = true
					}
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryWarning,
						Code:     WarningIPPReason,
						Message:  fmt.Sprintf("Nonblocking IPP warning/report reason: %s", r.Raw),
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "NONBLOCKING",
					})
				}
			} else {
				if kw != "none" && kw != "" {
					ippWarnings = append(ippWarnings, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
					diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
						Category: CategoryWarning,
						Code:     WarningIPPReason,
						Message:  fmt.Sprintf("Informational IPP reason: %s", r.Raw),
						Source:   SourceIPP,
						RawToken: r.Raw,
						Severity: "NONBLOCKING",
					})
				}
			}
		}

		if ipp.AcceptingJobs == AcceptingJobsNotAccepting {
			ippFaults = append(ippFaults, "CUPS queue is not accepting jobs")
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryFault,
				Code:     FaultQueueStopped,
				Message:  "CUPS queue is not accepting jobs according to IPP printer-is-accepting-jobs",
				Source:   SourceIPP,
				Severity: "BLOCKING",
			})
		}
		if ipp.HasHardwareFault && len(ippFaults) == 0 {
			ippFaults = append(ippFaults, "hardware fault reported by IPP")
		}
	}

	// ---------------------------------------------------------
	// 5. Synthesize Canonical Physical Flags & Faults
	// ---------------------------------------------------------
	paperJam := cupsJam || (snmp.Online && snmpJam) || ippJam
	coverOpen := cupsCover || (snmp.Online && snmpCover) || ippCover
	outputBinFull := cupsOutputFull || (snmp.Online && snmpOutputFull) || ippOutputFull
	paperEmpty := cupsPaperEmpty || (snmp.Online && snmpPaperEmpty) || ippPaperEmpty
	paperLow := !paperEmpty && (cupsPaperLow || (snmp.Online && snmpPaperLow) || ippPaperLow)

	// PaperTrayStatus resolution
	var paperTrayStatus string
	if paperEmpty {
		paperTrayStatus = "EMPTY"
	} else if paperLow {
		paperTrayStatus = "LOW"
	} else if snmp.Online && snmp.PaperTrayStatus == "OK" {
		paperTrayStatus = "OK"
	} else if cups.PaperTrayStatus == "OK" {
		paperTrayStatus = "OK"
	} else {
		paperTrayStatus = "UNKNOWN"
		unknownConditions = appendUnique(unknownConditions, "paper tray level unknown")
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryUnknown,
			Code:     UnknownPaperTray,
			Message:  "Paper tray status is unverified / unknown",
			Source:   SourceReconciler,
			Severity: "INFO",
		})
	}

	// Populate canonical blocking faults in fixed deterministic order
	if paperJam {
		blockingFaults = appendUnique(blockingFaults, "paper jam")
	}
	if coverOpen {
		blockingFaults = appendUnique(blockingFaults, "cover or door open")
	}
	if outputBinFull {
		blockingFaults = appendUnique(blockingFaults, "output bin full")
	}
	if paperEmpty {
		blockingFaults = appendUnique(blockingFaults, "out of paper / media empty")
	}

	// Append remaining subsystem-specific blocking faults
	for _, f := range cupsFaults {
		if !strings.Contains(f, "paper jam") &&
			!strings.Contains(f, "cover") &&
			!strings.Contains(f, "output bin full") &&
			!strings.Contains(f, "media empty") {
			blockingFaults = appendUnique(blockingFaults, f)
		}
	}
	for _, f := range snmpFaults {
		if !strings.Contains(f, "paper jam") &&
			!strings.Contains(f, "cover") &&
			!strings.Contains(f, "output bin full") &&
			!strings.Contains(f, "media empty") {
			blockingFaults = appendUnique(blockingFaults, f)
		}
	}
	for _, f := range ippFaults {
		if !strings.Contains(f, "media-jam") &&
			!strings.Contains(f, "cover-open") &&
			!strings.Contains(f, "door-open") &&
			!strings.Contains(f, "output-bin-full") &&
			!strings.Contains(f, "media-empty") &&
			!strings.Contains(f, "media-needed") {
			blockingFaults = appendUnique(blockingFaults, f)
		}
	}

	// Canonical nonblocking warnings
	if paperLow {
		nonblockingWarnings = appendUnique(nonblockingWarnings, "paper tray low")
	}
	for _, w := range cupsWarnings {
		if !strings.Contains(w, "paper tray low") {
			nonblockingWarnings = appendUnique(nonblockingWarnings, w)
		}
	}
	for _, w := range snmpWarnings {
		if !strings.Contains(w, "paper tray low") {
			nonblockingWarnings = appendUnique(nonblockingWarnings, w)
		}
	}
	for _, w := range ippWarnings {
		if !strings.Contains(w, "media-low") {
			nonblockingWarnings = appendUnique(nonblockingWarnings, w)
		}
	}

	// ---------------------------------------------------------
	// 6. Cross-Subsystem Contradiction Detection
	// ---------------------------------------------------------
	if ippValid {
		// CUPS PRINTING vs IPP idle/stopped
		if cups.State == "PRINTING" && (ipp.State == IPPPrinterStateIdle || ipp.State == IPPPrinterStateStopped) {
			cMsg := fmt.Sprintf("CUPS reports queue PRINTING while IPP reports printer-state %q", ipp.State)
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionJobState,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		// CUPS IDLE/READY vs IPP processing
		if (cups.State == "IDLE" || cups.State == "READY") && ipp.State == IPPPrinterStateProcessing {
			cMsg := "CUPS reports queue IDLE/READY while IPP reports printer-state processing"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionJobState,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		// CUPS Online vs IPP stopped
		if cups.Online && ipp.State == IPPPrinterStateStopped {
			cMsg := "CUPS reports queue online while IPP reports printer-state stopped"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionOnlineStatus,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		// CUPS Online vs IPP not accepting
		if cups.Online && ipp.AcceptingJobs == AcceptingJobsNotAccepting {
			cMsg := "CUPS reports queue online while IPP reports printer-is-accepting-jobs false"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionOnlineStatus,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
	}

	// CUPS PRINTING vs confirmed physical hardware faults
	if cups.State == "PRINTING" && (paperJam || coverOpen || outputBinFull || paperEmpty) {
		cMsg := "CUPS reports queue PRINTING while physical hardware fault is present"
		contradictions = appendUnique(contradictions, cMsg)
		diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
			Category: CategoryContradiction,
			Code:     ContradictionHardwareFault,
			Message:  cMsg,
			Source:   SourceReconciler,
			Severity: "NONBLOCKING",
		})
	}

	// IPP Faults vs Normal SNMP Data (and vice-versa)
	if ippValid && snmp.Online {
		if ippJam && !snmpJam {
			cMsg := "IPP reports paper jam while SNMP reports no paper jam"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		if snmpJam && !ippJam {
			cMsg := "SNMP reports paper jam while IPP reports no paper jam"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}

		if ippCover && !snmpCover {
			cMsg := "IPP reports cover open while SNMP reports cover closed"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		if snmpCover && !ippCover {
			cMsg := "SNMP reports cover open while IPP reports cover closed"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}

		if ippPaperEmpty && snmp.PaperTrayStatus == "OK" {
			cMsg := "IPP reports media empty while SNMP reports paper tray OK"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		if snmpPaperEmpty && !ippPaperEmpty && ipp.State == IPPPrinterStateIdle {
			cMsg := "SNMP reports paper tray EMPTY while IPP reports media not empty"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}

		if ippOutputFull && !snmpOutputFull {
			cMsg := "IPP reports output bin full while SNMP reports output bin not full"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		if snmpOutputFull && !ippOutputFull {
			cMsg := "SNMP reports output bin full while IPP reports output bin not full"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
	}

	// CUPS vs SNMP Contradictions
	if snmp.Online {
		if cupsPaperEmpty && snmp.PaperTrayStatus == "OK" {
			cMsg := "CUPS reports paper tray EMPTY while SNMP reports paper tray OK"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		if snmpPaperEmpty && cups.PaperTrayStatus == "OK" {
			cMsg := "SNMP reports paper tray EMPTY while CUPS reports paper tray OK"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		if cupsJam && !snmpJam {
			cMsg := "CUPS reports paper jam while SNMP reports no paper jam"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionHardwareFault,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
		if !cups.Online && snmp.Online {
			cMsg := "CUPS reports queue offline while SNMP reports device online"
			contradictions = appendUnique(contradictions, cMsg)
			diagnosticDetails = append(diagnosticDetails, DiagnosticItem{
				Category: CategoryContradiction,
				Code:     ContradictionOnlineStatus,
				Message:  cMsg,
				Source:   SourceReconciler,
				Severity: "NONBLOCKING",
			})
		}
	}

	// ---------------------------------------------------------
	// 7. Effective Operational State Determination
	// ---------------------------------------------------------
	var effectiveState string

	hasHardwareFault := paperJam || coverOpen || outputBinFull || paperEmpty ||
		(snmp.Online && (snmp.State == "ERROR" || snmpJam || snmpCover || snmpOutputFull || snmpPaperEmpty)) ||
		(ippValid && (ipp.HasHardwareFault || ippJam || ippCover || ippOutputFull || ippPaperEmpty)) ||
		(cups.State == "ERROR")

	if hasHardwareFault {
		// Confirmed critical hardware fault takes precedence over all execution states
		effectiveState = "ERROR"
	} else if !cups.Online || cups.State == "OFFLINE" {
		effectiveState = "OFFLINE"
	} else if len(blockingFaults) > 0 {
		effectiveState = "ERROR"
	} else if cups.State == "PRINTING" {
		effectiveState = "PRINTING"
	} else if snmp.Online && snmp.State == "WARMING_UP" {
		effectiveState = "WARMING_UP"
	} else if cups.State == "READY" || cups.State == "IDLE" {
		effectiveState = "IDLE"
	} else if cups.State != "" && cups.State != "UNKNOWN" {
		effectiveState = cups.State
	} else if snmp.Online && snmp.State == "IDLE" {
		effectiveState = "IDLE"
	} else if ippValid && ipp.State == IPPPrinterStateIdle {
		effectiveState = "IDLE"
	} else {
		effectiveState = "UNKNOWN"
	}

	online := cups.Online && len(blockingFaults) == 0 && cups.State != "OFFLINE"

	// ---------------------------------------------------------
	// 8. Consumables & Physical Telemetry
	// ---------------------------------------------------------
	estimatedSheets := cups.EstimatedSheets
	if estimatedSheets == 0 && snmp.EstimatedSheets > 0 {
		estimatedSheets = snmp.EstimatedSheets
	}

	var tonerPct, drumPct int
	if snmp.Online {
		tonerPct = snmp.TonerLevelPercent
		drumPct = snmp.DrumLifePercent
	}

	// ---------------------------------------------------------
	// 9. Source Attribution
	// ---------------------------------------------------------
	attribution := SourceAttribution{
		CUPS: SubsystemAttribution{
			Queried:    true,
			Available:  cups.Online,
			State:      cups.State,
			Faults:     cupsFaults,
			Warnings:   cupsWarnings,
			RawDetails: cups.ErrorMessage,
		},
		IPP: SubsystemAttribution{
			Queried:    !ipp.ObservedAt.IsZero() || ipp.EndpointReachable || ipp.Error != "",
			Available:  ippValid,
			State:      string(ipp.State),
			Faults:     ippFaults,
			Warnings:   ippWarnings,
			RawDetails: ipp.Error,
		},
		SNMP: SubsystemAttribution{
			Queried:    snmp.Online || snmp.ErrorMessage != "",
			Available:  snmp.Online,
			State:      snmp.State,
			Faults:     snmpFaults,
			Warnings:   snmpWarnings,
			RawDetails: snmp.ErrorMessage,
		},
	}

	return ReconciledHealthResult{
		EffectiveState:      effectiveState,
		Online:              online,
		BlockingFaults:      blockingFaults,
		NonblockingWarnings: nonblockingWarnings,
		UnknownConditions:   unknownConditions,
		Contradictions:      contradictions,
		SourceAttribution:   attribution,
		RawIPPReasons:       rawIPPReasons,
		PaperJam:            paperJam,
		CoverOpen:           coverOpen,
		OutputBinFull:       outputBinFull,
		PaperTrayStatus:     paperTrayStatus,
		EstimatedSheets:     estimatedSheets,
		TonerLevelPercent:   tonerPct,
		DrumLifePercent:     drumPct,
		EvaluatedAt:         evalTime,
		DiagnosticDetails:   diagnosticDetails,
	}
}

// isInherentlyCriticalIPPKeyword returns true if an IPP reason keyword represents
// a critical blocking physical error when reported without an explicit severity suffix.
func isInherentlyCriticalIPPKeyword(kw string) bool {
	switch kw {
	case "media-jam",
		"media-empty",
		"media-needed",
		"cover-open",
		"door-open",
		"output-bin-full",
		"marker-supply-empty",
		"marker-waste-full",
		"fuser-over-temp",
		"fuser-under-temp":
		return true
	default:
		return false
	}
}

// appendUnique appends items to slice only if they are not already present,
// preserving deterministic insertion order.
func appendUnique(slice []string, items ...string) []string {
	seen := make(map[string]bool, len(slice)+len(items))
	for _, s := range slice {
		seen[s] = true
	}
	res := slice
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			res = append(res, item)
		}
	}
	return res
}

// sliceHasItemContaining returns true if any element in slice contains substr.
func sliceHasItemContaining(slice []string, substr string) bool {
	lowerSub := strings.ToLower(substr)
	for _, item := range slice {
		if strings.Contains(strings.ToLower(item), lowerSub) {
			return true
		}
	}
	return false
}
