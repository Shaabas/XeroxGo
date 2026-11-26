package printer

import (
	"fmt"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// DefaultMaxSourceAge defines the canonical maximum age (6s) for required printer telemetry sources.
const DefaultMaxSourceAge = contracts.DefaultTelemetryMaxAge

// MaxIPPObservationAge aliases DefaultMaxSourceAge for existing callers and tests.
const MaxIPPObservationAge = DefaultMaxSourceAge

// FutureTimestampTolerance defines maximum allowable future clock skew on observations.
const FutureTimestampTolerance = contracts.FutureHealthTimestampTolerance

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

	WarningLowToner  = "LOW_TONER"
	WarningLowPaper  = "LOW_PAPER"
	WarningLowDrum   = "LOW_DRUM"
	WarningStaleIPP  = "STALE_IPP_OBSERVATION"
	WarningIPPReason = "IPP_WARNING"

	UnknownSNMP       = "SNMP_TELEMETRY_UNAVAILABLE"
	UnknownIPP        = "IPP_TELEMETRY_UNAVAILABLE"
	UnknownPaperTray  = "PAPER_TRAY_UNKNOWN"
	UnknownQueueState = "QUEUE_STATE_UNKNOWN"
	UnknownEvalTime   = "EVAL_TIME_INVALID"

	ContradictionJobState       = "CONTRADICTION_JOB_STATE"
	ContradictionHardwareFault  = "CONTRADICTION_HARDWARE_FAULT"
	ContradictionOnlineStatus   = "CONTRADICTION_ONLINE_STATUS"
	ContradictionTrayCapacity   = "CONTRADICTION_TRAY_CAPACITY"
	ObservationMismatchHardware = "OBSERVATION_MISMATCH_HARDWARE"
)

// SubsystemAttribution records the diagnostic findings and operational state reported by one subsystem.
type SubsystemAttribution struct {
	Queried    bool     `json:"queried"`
	Available  bool     `json:"available"`
	State      string   `json:"state"`
	Faults     []string `json:"faults"`
	Warnings   []string `json:"warnings"`
	RawDetails string   `json:"raw_details,omitempty"`
}

// SourceAttribution records observations attributed to CUPS, IPP, and SNMP.
type SourceAttribution struct {
	CUPS SubsystemAttribution `json:"cups"`
	IPP  SubsystemAttribution `json:"ipp"`
	SNMP SubsystemAttribution `json:"snmp"`
}

// HealthCategory categorizes diagnostic observations.
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

// ReconciledHealthResult is the deterministic output of the canonical health evaluator.
type ReconciledHealthResult struct {
	// EffectiveState is the operational printer state ("IDLE", "PRINTING", "WARMING_UP", "ERROR", "OFFLINE", "UNKNOWN").
	EffectiveState string `json:"effective_state"`

	// EvaluatedHealth is the canonical dispatch readiness state ("HEALTHY", "FAULT", "UNVERIFIED").
	EvaluatedHealth contracts.EvaluatedHealthStatus `json:"evaluated_health"`

	Online bool `json:"online"`

	BlockingFaults      []string `json:"blocking_faults"`
	NonblockingWarnings []string `json:"nonblocking_warnings"`
	UnknownConditions   []string `json:"unknown_conditions"`
	Contradictions      []string `json:"contradictions"`

	SourceAttribution SourceAttribution `json:"source_attribution"`
	RawIPPReasons     []string          `json:"raw_ipp_reasons,omitempty"`

	PaperJam               bool   `json:"paper_jam"`
	CoverOpen              bool   `json:"cover_open"`
	OutputBinFull          bool   `json:"output_bin_full"`
	PaperTrayStatus        string `json:"paper_tray_status"` // "OK", "LOW", "EMPTY", "UNKNOWN"
	EstimatedSheets        int    `json:"estimated_sheets"`
	TonerLevelPercent      int    `json:"toner_level_percent"`
	TonerLevelKnown        bool   `json:"toner_level_known"`
	DrumLifePercent        int    `json:"drum_life_percent"`
	DrumLifeKnown          bool   `json:"drum_life_known"`
	InputTrayLevel         int    `json:"input_tray_level"`
	InputTrayLevelKnown    bool   `json:"input_tray_level_known"`
	InputTrayCapacity      int    `json:"input_tray_capacity"`
	InputTrayCapacityKnown bool   `json:"input_tray_capacity_known"`

	BlockingReasons   []contracts.HealthReason         `json:"blocking_reasons,omitempty"`
	Warnings          []contracts.HealthReason         `json:"warnings,omitempty"`
	EvidenceFreshness  *contracts.HealthEvidenceFreshness `json:"evidence_freshness,omitempty"`

	EvaluatedAt        time.Time        `json:"evaluated_at"`
	EffectiveUpdatedAt time.Time        `json:"effective_updated_at,omitempty"`
	DiagnosticDetails  []DiagnosticItem `json:"diagnostic_details,omitempty"`
}

// HasBlockingFaults returns true if there are one or more confirmed blocking faults.
func (r ReconciledHealthResult) HasBlockingFaults() bool {
	return len(r.BlockingFaults) > 0
}

// IsReady returns true only when canonical evaluation is HEALTHY and operational state accepts new work.
func (r ReconciledHealthResult) IsReady() bool {
	return r.EvaluatedHealth == contracts.EvaluatedHealthHealthy &&
		r.Online &&
		len(r.BlockingFaults) == 0 &&
		(r.EffectiveState == "IDLE" || r.EffectiveState == "READY")
}

// ToContract converts the canonical evaluation result into contracts.PrinterHealth.
func (r ReconciledHealthResult) ToContract(
	estimatedSheets int,
	inventoryCalibrated bool,
	executionSafe bool,
	executionSafetyReason string,
) contracts.PrinterHealth {
	tonerPct := -1
	if r.TonerLevelKnown {
		tonerPct = r.TonerLevelPercent
	}
	drumPct := -1
	if r.DrumLifeKnown {
		drumPct = r.DrumLifePercent
	}
	trayLvl := -1
	if r.InputTrayLevelKnown {
		trayLvl = r.InputTrayLevel
	}
	trayCap := -1
	if r.InputTrayCapacityKnown {
		trayCap = r.InputTrayCapacity
	}

	var errMsgs []string
	for _, b := range r.BlockingReasons {
		if b.Message != "" {
			errMsgs = append(errMsgs, b.Message)
		} else if b.Code != "" {
			errMsgs = append(errMsgs, b.Code)
		}
	}
	if len(errMsgs) == 0 && len(r.BlockingFaults) > 0 {
		errMsgs = append(errMsgs, r.BlockingFaults...)
	} else if len(errMsgs) == 0 && r.EvaluatedHealth == contracts.EvaluatedHealthUnverified && len(r.UnknownConditions) > 0 {
		errMsgs = append(errMsgs, r.UnknownConditions[0])
	}

	state := r.EffectiveState
	if state == "" {
		state = "UNKNOWN"
	}
	trayStatus := r.PaperTrayStatus
	if trayStatus == "" {
		trayStatus = "UNKNOWN"
	}

	updatedAt := r.EffectiveUpdatedAt
	if updatedAt.IsZero() {
		updatedAt = r.EvaluatedAt
	}

	effectiveSheets := estimatedSheets
	// Conservative capacity enforcement: contradictory physical capacity must not permit
	// inventory above verified physical limit.
	if r.InputTrayCapacityKnown && r.InputTrayCapacity > 0 && effectiveSheets > r.InputTrayCapacity {
		effectiveSheets = r.InputTrayCapacity
	}

	return contracts.PrinterHealth{
		Online:                 r.EvaluatedHealth == contracts.EvaluatedHealthHealthy,
		State:                  state,
		EvaluatedHealth:        r.EvaluatedHealth,
		PaperTrayStatus:        trayStatus,
		EstimatedSheets:        effectiveSheets,
		InventoryCalibrated:    inventoryCalibrated,
		ExecutionSafe:          executionSafe,
		ExecutionSafetyReason:  strings.TrimSpace(executionSafetyReason),
		TonerLevelPercent:      tonerPct,
		TonerLevelKnown:        r.TonerLevelKnown,
		DrumLifePercent:        drumPct,
		DrumLifeKnown:          r.DrumLifeKnown,
		InputTrayLevel:         trayLvl,
		InputTrayLevelKnown:    r.InputTrayLevelKnown,
		InputTrayCapacity:      trayCap,
		InputTrayCapacityKnown: r.InputTrayCapacityKnown,
		CoverOpen:              r.CoverOpen,
		PaperJam:               r.PaperJam,
		OutputBinFull:          r.OutputBinFull,
		BlockingReasons:        append([]contracts.HealthReason(nil), r.BlockingReasons...),
		Warnings:               append([]contracts.HealthReason(nil), r.Warnings...),
		EvidenceFreshness:      r.EvidenceFreshness,
		ErrorMessage:           strings.Join(errMsgs, "; "),
		UpdatedAt:              updatedAt,
	}
}

// SourceValidity specifies explicit source validity/requirement overrides for compatibility & tests.
type SourceValidity struct {
	CUPSValid        bool
	SNMPValid        bool
	SNMPQueried      bool
	RequireNativeIPP bool
	MaxAge           time.Duration
}

// CanonicalHealthInput bundles telemetry from CUPS, native printer IPP, and SNMP for pure evaluation.
type CanonicalHealthInput struct {
	CUPS        contracts.PrinterHealth
	CUPSValid   *bool
	CUPSQueried bool
	CUPSError   string

	IPP              IPPPrinterStateResult
	RequireNativeIPP bool

	SNMP             SNMPObservation
	LegacySNMP       *contracts.PrinterHealth
	SNMPValid        *bool
	SNMPQueried      bool
	RequireSNMP      bool
	RequireIPP       bool
	RequireCUPS      bool

	EvalTime               time.Time
	MaxSourceAge           time.Duration
	ConfiguredTrayCapacity int
}

// EvaluateCanonicalPrinterHealth evaluates CUPS, native IPP, and SNMP evidence with strict
// freshness, physical fault precedence, and unknown-vs-fault separation.
func EvaluateCanonicalPrinterHealth(in CanonicalHealthInput) ReconciledHealthResult {
	maxAge := in.MaxSourceAge
	if maxAge <= 0 {
		maxAge = DefaultMaxSourceAge
	}

	snmpHealth := in.SNMP.Health
	if in.LegacySNMP != nil {
		snmpHealth = *in.LegacySNMP
	}

	eval := evaluatorState{
		in:                  in,
		maxAge:              maxAge,
		evalTime:            in.EvalTime,
		snmpHealth:          snmpHealth,
		blockingFaults:      make([]string, 0),
		nonblockingWarnings: make([]string, 0),
		unknownConditions:   make([]string, 0),
		contradictions:      make([]string, 0),
		diagnosticDetails:   make([]DiagnosticItem, 0),
	}
	return eval.run()
}

// ReconcilePrinterHealth provides the compatibility signature for unit and emulator integration tests.
func ReconcilePrinterHealth(
	cups contracts.PrinterHealth,
	ipp IPPPrinterStateResult,
	snmp contracts.PrinterHealth,
	evalTime time.Time,
	validity ...SourceValidity,
) ReconciledHealthResult {
	in := CanonicalHealthInput{
		CUPS:        cups,
		CUPSQueried: true,
		IPP:         ipp,
		LegacySNMP:  &snmp,
		EvalTime:    evalTime,
		RequireCUPS: true,
		RequireIPP:  true,
		RequireSNMP: true,
	}
	if len(validity) > 0 {
		v := validity[0]
		in.CUPSValid = &v.CUPSValid
		in.SNMPValid = &v.SNMPValid
		in.SNMPQueried = v.SNMPQueried
		in.RequireNativeIPP = v.RequireNativeIPP
		in.MaxSourceAge = v.MaxAge
	}
	return EvaluateCanonicalPrinterHealth(in)
}

type evaluatorState struct {
	in         CanonicalHealthInput
	maxAge     time.Duration
	evalTime   time.Time
	snmpHealth contracts.PrinterHealth

	cupsValid bool
	ippValid  bool
	snmpValid bool

	cupsFresh contracts.SourceFreshness
	ippFresh  contracts.SourceFreshness
	snmpFresh contracts.SourceFreshness

	cupsFaults, cupsWarnings []string
	ippFaults, ippWarnings   []string
	snmpFaults, snmpWarnings []string

	cupsJam, cupsCover, cupsOutputFull, cupsPaperEmpty, cupsPaperLow bool
	ippJam, ippCover, ippOutputFull, ippPaperEmpty, ippPaperLow      bool
	snmpJam, snmpCover, snmpOutputFull, snmpPaperEmpty, snmpPaperLow bool
	snmpDeviceDown, snmpCriticalAlert                                bool

	blockingFaults      []string
	nonblockingWarnings []string
	unknownConditions   []string
	contradictions      []string
	diagnosticDetails   []DiagnosticItem
	blockingReasons     []contracts.HealthReason
	warningsReasons     []contracts.HealthReason
}

func (e *evaluatorState) addDiagnostic(cat HealthCategory, code, msg string, src HealthSource, rawToken, sev string) {
	e.diagnosticDetails = append(e.diagnosticDetails, DiagnosticItem{
		Category: cat,
		Code:     code,
		Message:  msg,
		Source:   src,
		RawToken: rawToken,
		Severity: sev,
	})
}

func (e *evaluatorState) addBlocking(code, srcLabel, msg string, src HealthSource, faultSummary string) {
	e.blockingReasons = append(e.blockingReasons, contracts.HealthReason{
		Code:    strings.ToLower(code),
		Source:  srcLabel,
		Message: msg,
	})
	if faultSummary != "" {
		e.blockingFaults = appendUnique(e.blockingFaults, faultSummary)
	}
	e.addDiagnostic(CategoryFault, code, msg, src, "", "BLOCKING")
}

func checkFreshTimestamp(ts, evalTime time.Time, maxAge time.Duration, label string) (ok bool, ageSec float64, reason string) {
	if evalTime.IsZero() {
		return false, 0, "evaluation time is zero"
	}
	if ts.IsZero() {
		return false, 0, fmt.Sprintf("%s observation timestamp is zero", label)
	}
	if ts.After(evalTime.Add(FutureTimestampTolerance)) {
		return false, 0, fmt.Sprintf("%s observation timestamp is in the future (skew: %s)", label, ts.Sub(evalTime).Round(time.Millisecond))
	}
	age := evalTime.Sub(ts)
	if age > maxAge {
		return false, age.Seconds(), fmt.Sprintf("%s observation is stale (age %s exceeds max %s)", label, age.Round(time.Millisecond), maxAge)
	}
	if age < 0 {
		age = 0
	}
	return true, age.Seconds(), ""
}

func (e *evaluatorState) validateSources() {
	// CUPS validity & freshness: validity overrides must NEVER bypass timestamp validation.
	cupsTS := e.in.CUPS.UpdatedAt
	cupsFreshOK, cupsAgeSec, cupsTimeErr := checkFreshTimestamp(cupsTS, e.evalTime, e.maxAge, "CUPS")
	if !cupsTS.IsZero() {
		tsCopy := cupsTS
		e.cupsFresh.ObservedAt = &tsCopy
	}
	e.cupsFresh.AgeSeconds = cupsAgeSec

	if !cupsFreshOK {
		e.cupsValid = false
		e.cupsFresh.Error = cupsTimeErr
		e.unknownConditions = appendUnique(e.unknownConditions, cupsTimeErr)
		e.addDiagnostic(CategoryUnknown, UnknownQueueState, cupsTimeErr, SourceCUPS, "", "INFO")
	} else if e.in.CUPSValid != nil {
		e.cupsValid = *e.in.CUPSValid
		if !e.cupsValid {
			e.cupsFresh.Error = firstNonEmpty(e.in.CUPSError, "CUPS source invalid or unavailable")
		}
	} else {
		cupsStateUpper := strings.ToUpper(strings.TrimSpace(e.in.CUPS.State))
		if cupsStateUpper == "" || cupsStateUpper == "UNKNOWN" {
			e.cupsValid = false
			e.cupsFresh.Error = firstNonEmpty(e.in.CUPSError, "CUPS queue state unknown")
		} else {
			e.cupsValid = true
		}
	}
	e.cupsFresh.Available = e.cupsValid
	e.cupsFresh.Fresh = e.cupsValid && cupsFreshOK

	// IPP validity & freshness
	ippTS := e.in.IPP.ObservedAt
	if !ippTS.IsZero() {
		tsCopy := ippTS
		e.ippFresh.ObservedAt = &tsCopy
	}
	ippFreshOK, ippAgeSec, ippTimeErr := checkFreshTimestamp(ippTS, e.evalTime, e.maxAge, "IPP")
	e.ippFresh.AgeSeconds = ippAgeSec

	var ippReject string
	switch {
	case e.evalTime.IsZero():
		ippReject = "evaluation time is zero (invalid or unspecified); time-dependent evaluation rejected"
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownEvalTime, ippReject, SourceReconciler, "", "INFO")
	case ippTS.IsZero():
		ippReject = "IPP observation timestamp is zero (missing or uninitialized)"
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
	case !ippFreshOK:
		ippReject = ippTimeErr
		if strings.Contains(ippTimeErr, "stale") {
			e.nonblockingWarnings = appendUnique(e.nonblockingWarnings, ippReject)
			e.unknownConditions = appendUnique(e.unknownConditions, fmt.Sprintf("stale IPP observation (age %.1fs); fresh state unknown", ippAgeSec))
			e.addDiagnostic(CategoryWarning, WarningStaleIPP, ippReject, SourceIPP, "", "NONBLOCKING")
		} else {
			e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
			e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
		}
	case e.in.RequireNativeIPP && e.in.IPP.Source != CapabilitySourceNativeIPP:
		ippReject = fmt.Sprintf("IPP observation source is %q, expected native printer IPP", e.in.IPP.Source)
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
	case !e.in.IPP.EndpointReachable:
		ippReject = "IPP endpoint unreachable"
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
	case !e.in.IPP.QueueAccessible:
		ippReject = "IPP queue is inaccessible on CUPS server"
		if e.in.IPP.StatusCode != "" {
			ippReject = fmt.Sprintf("IPP queue is inaccessible on CUPS server (status: %s)", e.in.IPP.StatusCode)
		}
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
	case isIPPFailureStatus(e.in.IPP.StatusCode):
		ippReject = fmt.Sprintf("IPP query returned failure status %q", e.in.IPP.StatusCode)
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
	case e.in.IPP.Error != "":
		ippReject = fmt.Sprintf("IPP state query failed: %s", e.in.IPP.Error)
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
	case !e.in.IPP.HasRecognizedStateAttributes():
		ippReject = "IPP response contains no recognized printer state attributes"
		e.unknownConditions = appendUnique(e.unknownConditions, ippReject)
		e.addDiagnostic(CategoryUnknown, UnknownIPP, ippReject, SourceIPP, "", "INFO")
	default:
		e.ippValid = true
	}
	e.ippFresh.Available = e.ippValid
	e.ippFresh.Fresh = e.ippValid && ippFreshOK
	e.ippFresh.Error = ippReject

	// SNMP validity & freshness: validity overrides must NEVER bypass timestamp validation.
	snmpTS := e.snmpHealth.UpdatedAt
	if !e.in.SNMP.ObservedAt.IsZero() {
		snmpTS = e.in.SNMP.ObservedAt
	}
	if !snmpTS.IsZero() {
		tsCopy := snmpTS
		e.snmpFresh.ObservedAt = &tsCopy
	}
	snmpFreshOK, snmpAgeSec, snmpTimeErr := checkFreshTimestamp(snmpTS, e.evalTime, e.maxAge, "SNMP")
	e.snmpFresh.AgeSeconds = snmpAgeSec

	if !snmpFreshOK {
		e.snmpValid = false
		e.snmpFresh.Error = snmpTimeErr
		e.unknownConditions = appendUnique(e.unknownConditions, snmpTimeErr)
		e.addDiagnostic(CategoryUnknown, UnknownSNMP, snmpTimeErr, SourceSNMP, "", "INFO")
	} else if e.in.SNMPValid != nil {
		e.snmpValid = *e.in.SNMPValid
		if !e.snmpValid {
			e.snmpFresh.Error = firstNonEmpty(e.in.SNMP.Error, e.snmpHealth.ErrorMessage, "SNMP source invalid or unavailable")
		}
	} else {
		snmpAvail := e.snmpHealth.Online
		if e.in.LegacySNMP == nil {
			snmpAvail = e.in.SNMP.Available
		}
		if e.in.LegacySNMP == nil && !snmpAvail {
			e.snmpValid = false
			e.snmpFresh.Error = firstNonEmpty(e.in.SNMP.Error, e.snmpHealth.ErrorMessage, "SNMP telemetry unavailable")
		} else {
			e.snmpValid = snmpAvail
			if !snmpAvail {
				e.snmpFresh.Error = firstNonEmpty(e.snmpHealth.ErrorMessage, "SNMP telemetry unavailable")
			}
		}
	}
	e.snmpFresh.Available = e.snmpValid
	e.snmpFresh.Fresh = e.snmpValid && snmpFreshOK
}

func (e *evaluatorState) analyzeCUPS() {
	if !e.cupsValid {
		e.unknownConditions = appendUnique(e.unknownConditions, "CUPS telemetry unavailable or unverified")
		e.addDiagnostic(CategoryUnknown, UnknownQueueState, "CUPS queue telemetry is unavailable, stale, or unverified", SourceCUPS, "", "INFO")
		return
	}

	c := e.in.CUPS
	lowerErr := strings.ToLower(c.ErrorMessage)
	e.cupsJam = c.PaperJam || strings.Contains(lowerErr, "jam")
	e.cupsCover = c.CoverOpen || strings.Contains(lowerErr, "door open") || strings.Contains(lowerErr, "cover open")
	e.cupsOutputFull = c.OutputBinFull || strings.Contains(lowerErr, "output bin full")
	e.cupsPaperEmpty = c.PaperTrayStatus == "EMPTY" ||
		strings.Contains(lowerErr, "out of paper") ||
		strings.Contains(lowerErr, "media-empty") ||
		strings.Contains(lowerErr, "no paper")
	e.cupsPaperLow = c.PaperTrayStatus == "LOW" || strings.Contains(lowerErr, "paper low")

	if e.cupsJam {
		e.cupsFaults = append(e.cupsFaults, "paper jam")
		e.addDiagnostic(CategoryFault, FaultPaperJam, "Paper jam detected via CUPS queue", SourceCUPS, "", "BLOCKING")
	}
	if e.cupsCover {
		e.cupsFaults = append(e.cupsFaults, "cover or door open")
		e.addDiagnostic(CategoryFault, FaultCoverOpen, "Cover or door open detected via CUPS queue", SourceCUPS, "", "BLOCKING")
	}
	if e.cupsOutputFull {
		e.cupsFaults = append(e.cupsFaults, "output bin full")
		e.addDiagnostic(CategoryFault, FaultOutputBinFull, "Output bin full detected via CUPS queue", SourceCUPS, "", "BLOCKING")
	}
	if e.cupsPaperEmpty {
		e.cupsFaults = append(e.cupsFaults, "out of paper / media empty")
		e.addDiagnostic(CategoryFault, FaultMediaEmpty, "Out of paper detected via CUPS queue", SourceCUPS, "", "BLOCKING")
	}
	if e.cupsPaperLow {
		e.cupsWarnings = append(e.cupsWarnings, "paper tray low")
		e.addDiagnostic(CategoryWarning, WarningLowPaper, "Paper tray low reported by CUPS", SourceCUPS, "", "NONBLOCKING")
	}

	if !c.Online || c.State == "OFFLINE" {
		e.cupsFaults = append(e.cupsFaults, "CUPS queue offline or disabled")
		e.addDiagnostic(CategoryFault, FaultQueueOffline, "CUPS printer queue is offline or disabled", SourceCUPS, "", "BLOCKING")
	} else if c.State == "ERROR" && !e.cupsJam && !e.cupsCover && !e.cupsOutputFull && !e.cupsPaperEmpty {
		msg := firstNonEmpty(c.ErrorMessage, "CUPS queue reported error")
		e.cupsFaults = append(e.cupsFaults, fmt.Sprintf("CUPS queue error: %s", msg))
		e.addDiagnostic(CategoryFault, FaultQueueStopped, msg, SourceCUPS, "", "BLOCKING")
	}
}

func (e *evaluatorState) analyzeSNMP() {
	if !e.snmpValid || (!e.snmpHealth.Online && !e.in.SNMP.HasBlockingFault) {
		e.unknownConditions = appendUnique(e.unknownConditions, "SNMP telemetry unavailable; consumable levels and physical sensor status unverified")
		e.addDiagnostic(CategoryUnknown, UnknownSNMP, "SNMP telemetry unavailable; consumable levels and physical sensor status unverified", SourceSNMP, "", "INFO")
		return
	}

	s := e.snmpHealth
	lowerErr := strings.ToLower(s.ErrorMessage)

	e.snmpJam = s.PaperJam || strings.Contains(lowerErr, "jam")
	e.snmpCover = s.CoverOpen || strings.Contains(lowerErr, "door open") || strings.Contains(lowerErr, "cover open")
	e.snmpOutputFull = s.OutputBinFull || strings.Contains(lowerErr, "output bin full") || strings.Contains(lowerErr, "output full")
	e.snmpPaperEmpty = s.PaperTrayStatus == "EMPTY" ||
		strings.Contains(lowerErr, "out of paper") ||
		strings.Contains(lowerErr, "no paper") ||
		strings.Contains(lowerErr, "input tray empty") ||
		strings.Contains(lowerErr, "tray empty")
	e.snmpPaperLow = s.PaperTrayStatus == "LOW" || strings.Contains(lowerErr, "low paper")
	e.snmpDeviceDown = e.in.SNMP.DeviceStatus == 5 || strings.Contains(lowerErr, "hrdevicestatus") && strings.Contains(lowerErr, "down")

	for _, bf := range e.in.SNMP.BlockingFaults {
		switch bf.Code {
		case "paper_jam":
			e.snmpJam = true
		case "cover_open":
			e.snmpCover = true
		case "output_bin_full":
			e.snmpOutputFull = true
		case "paper_empty":
			e.snmpPaperEmpty = true
		case "snmp_device_down":
			e.snmpDeviceDown = true
		case "snmp_critical_alert":
			e.snmpCriticalAlert = true
		default:
			e.snmpFaults = append(e.snmpFaults, firstNonEmpty(bf.Message, bf.Code))
		}
	}

	if e.snmpJam {
		e.snmpFaults = append(e.snmpFaults, "paper jam")
		e.addDiagnostic(CategoryFault, FaultPaperJam, "Paper jam reported by SNMP", SourceSNMP, "", "BLOCKING")
	}
	if e.snmpCover {
		e.snmpFaults = append(e.snmpFaults, "cover or door open")
		e.addDiagnostic(CategoryFault, FaultCoverOpen, "Cover or door open reported by SNMP", SourceSNMP, "", "BLOCKING")
	}
	if e.snmpOutputFull {
		e.snmpFaults = append(e.snmpFaults, "output bin full")
		e.addDiagnostic(CategoryFault, FaultOutputBinFull, "Output bin full reported by SNMP", SourceSNMP, "", "BLOCKING")
	}
	if e.snmpPaperEmpty {
		e.snmpFaults = append(e.snmpFaults, "out of paper / media empty")
		e.addDiagnostic(CategoryFault, FaultMediaEmpty, "Paper tray empty reported by SNMP", SourceSNMP, "", "BLOCKING")
	}
	if e.snmpDeviceDown {
		e.snmpFaults = append(e.snmpFaults, "SNMP device status down (hrDeviceStatus=5)")
		e.addDiagnostic(CategoryFault, FaultHardwareError, "SNMP hrDeviceStatus reports device down (5)", SourceSNMP, "", "BLOCKING")
	}
	if e.snmpCriticalAlert {
		e.snmpFaults = append(e.snmpFaults, "SNMP critical hardware alert")
		e.addDiagnostic(CategoryFault, FaultHardwareError, "SNMP critical alert reported by device", SourceSNMP, "", "BLOCKING")
	}
	if strings.Contains(lowerErr, "no toner") || strings.Contains(lowerErr, "toner empty") {
		e.snmpFaults = append(e.snmpFaults, "toner cartridge empty")
		e.addDiagnostic(CategoryFault, FaultMarkerEmpty, "Toner cartridge empty reported by SNMP", SourceSNMP, "", "BLOCKING")
	}
	if strings.Contains(lowerErr, "service requested") {
		e.snmpFaults = append(e.snmpFaults, "hardware error: printer service requested")
		e.addDiagnostic(CategoryFault, FaultHardwareError, "Printer service requested reported by SNMP", SourceSNMP, "", "BLOCKING")
	}

	if e.snmpPaperLow {
		e.snmpWarnings = append(e.snmpWarnings, "paper tray low")
		e.addDiagnostic(CategoryWarning, WarningLowPaper, "Paper tray low reported by SNMP", SourceSNMP, "", "NONBLOCKING")
	}
	if (s.TonerLevelKnown || s.TonerLevelPercent > 0) && s.TonerLevelPercent > 0 && s.TonerLevelPercent <= 10 {
		warn := fmt.Sprintf("low toner level (%d%%)", s.TonerLevelPercent)
		e.snmpWarnings = append(e.snmpWarnings, warn)
		e.addDiagnostic(CategoryWarning, WarningLowToner, warn, SourceSNMP, "", "NONBLOCKING")
	} else if strings.Contains(lowerErr, "low toner") {
		e.snmpWarnings = append(e.snmpWarnings, "low toner reported")
		e.addDiagnostic(CategoryWarning, WarningLowToner, "Low toner reported by SNMP", SourceSNMP, "", "NONBLOCKING")
	}

	if (s.DrumLifeKnown || s.DrumLifePercent > 0) && s.DrumLifePercent > 0 && s.DrumLifePercent <= 10 {
		warn := fmt.Sprintf("low drum life (%d%%)", s.DrumLifePercent)
		e.snmpWarnings = append(e.snmpWarnings, warn)
		e.addDiagnostic(CategoryWarning, WarningLowDrum, warn, SourceSNMP, "", "NONBLOCKING")
	}
	// Note: Unknown or 0% drum life alone is NEVER a blocking fault.

	if s.State == "UNKNOWN" {
		msg := "SNMP reports printer status unknown (e.g. hrPrinterStatus 1 'other')"
		e.unknownConditions = appendUnique(e.unknownConditions, msg)
		e.addDiagnostic(CategoryUnknown, UnknownSNMP, msg, SourceSNMP, "", "INFO")
	}
}

func (e *evaluatorState) analyzeIPP() {
	if !e.ippValid {
		return
	}
	for _, r := range e.in.IPP.ParsedReasons {
		kw := strings.ToLower(strings.TrimSpace(r.Keyword))
		switch kw {
		case "", "none":
			continue
		case "media-jam", "paper-jam":
			e.ippJam = true
			e.ippFaults = append(e.ippFaults, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
			e.addDiagnostic(CategoryFault, FaultPaperJam, "Paper jam reported by IPP reason", SourceIPP, r.Raw, "BLOCKING")
		case "cover-open", "door-open", "interlock-open":
			e.ippCover = true
			e.ippFaults = append(e.ippFaults, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
			e.addDiagnostic(CategoryFault, FaultCoverOpen, "Cover/door open reported by IPP reason", SourceIPP, r.Raw, "BLOCKING")
		case "output-bin-full", "output-area-full":
			e.ippOutputFull = true
			e.ippFaults = append(e.ippFaults, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
			e.addDiagnostic(CategoryFault, FaultOutputBinFull, "Output bin full reported by IPP reason", SourceIPP, r.Raw, "BLOCKING")
		case "media-empty", "media-needed", "input-tray-missing":
			e.ippPaperEmpty = true
			e.ippFaults = append(e.ippFaults, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
			e.addDiagnostic(CategoryFault, FaultMediaEmpty, "Media empty reported by IPP reason", SourceIPP, r.Raw, "BLOCKING")
		case "marker-supply-empty", "toner-empty", "ink-empty", "marker-waste-full":
			e.ippFaults = append(e.ippFaults, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
			e.addDiagnostic(CategoryFault, FaultMarkerEmpty, fmt.Sprintf("Critical marker condition reported by IPP: %s", r.Raw), SourceIPP, r.Raw, "BLOCKING")
		case "media-low":
			e.ippPaperLow = true
			e.ippWarnings = append(e.ippWarnings, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
			e.addDiagnostic(CategoryWarning, WarningLowPaper, fmt.Sprintf("Nonblocking IPP warning: %s", r.Raw), SourceIPP, r.Raw, "NONBLOCKING")
		case "toner-low", "ink-low", "marker-supply-low", "marker-waste-almost-full":
			e.ippWarnings = append(e.ippWarnings, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
			e.addDiagnostic(CategoryWarning, WarningLowToner, fmt.Sprintf("Nonblocking IPP warning: %s", r.Raw), SourceIPP, r.Raw, "NONBLOCKING")
		default:
			if r.Severity == SeverityError || isInherentlyCriticalIPPKeyword(kw) {
				e.ippFaults = append(e.ippFaults, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
				e.addDiagnostic(CategoryFault, FaultHardwareError, fmt.Sprintf("Critical fault reported by IPP reason: %s", r.Raw), SourceIPP, r.Raw, "BLOCKING")
			} else {
				e.ippWarnings = append(e.ippWarnings, fmt.Sprintf("%s (reason: %s)", kw, r.Raw))
				e.addDiagnostic(CategoryWarning, WarningIPPReason, fmt.Sprintf("Nonblocking IPP reason: %s", r.Raw), SourceIPP, r.Raw, "NONBLOCKING")
			}
		}
	}

	if e.in.IPP.AcceptingJobs == AcceptingJobsNotAccepting {
		e.ippFaults = append(e.ippFaults, "queue is not accepting jobs")
		e.addDiagnostic(CategoryFault, FaultQueueStopped, "Printer/queue is not accepting jobs", SourceIPP, "", "BLOCKING")
	} else if e.in.IPP.State == IPPPrinterStateStopped && len(e.ippFaults) == 0 {
		e.ippFaults = append(e.ippFaults, "printer reports stopped state via IPP")
		e.addDiagnostic(CategoryFault, FaultQueueStopped, "Printer reports stopped state via IPP", SourceIPP, "", "BLOCKING")
	}
	if e.in.IPP.HasHardwareFault && len(e.ippFaults) == 0 {
		e.ippFaults = append(e.ippFaults, "hardware fault reported by IPP")
		e.addDiagnostic(CategoryFault, FaultHardwareError, "Hardware fault reported by IPP", SourceIPP, "", "BLOCKING")
	}
}

func (e *evaluatorState) detectContradictions(paperJam, coverOpen, outputFull, paperEmpty bool) {
	record := func(code, msg string) {
		e.contradictions = appendUnique(e.contradictions, msg)
		e.addDiagnostic(CategoryContradiction, code, msg, SourceReconciler, "", "NONBLOCKING")
	}

	if e.ippValid && e.cupsValid {
		if e.in.CUPS.State == "PRINTING" && (e.in.IPP.State == IPPPrinterStateIdle || e.in.IPP.State == IPPPrinterStateStopped) {
			record(ContradictionJobState, fmt.Sprintf("contradiction: CUPS reports queue PRINTING while IPP explicitly reports printer-state %q", e.in.IPP.State))
		}
		if (e.in.CUPS.State == "IDLE" || e.in.CUPS.State == "READY") && e.in.IPP.State == IPPPrinterStateProcessing {
			record(ContradictionJobState, "contradiction: CUPS reports queue IDLE/READY while IPP explicitly reports printer-state processing")
		}
		if e.in.CUPS.Online && e.in.IPP.State == IPPPrinterStateStopped {
			record(ContradictionOnlineStatus, "contradiction: CUPS reports queue online while IPP explicitly reports printer-state stopped")
		}
		if e.in.CUPS.Online && e.in.IPP.AcceptingJobs == AcceptingJobsNotAccepting {
			record(ContradictionOnlineStatus, "contradiction: CUPS reports queue online while IPP explicitly reports printer-is-accepting-jobs false")
		}
	}

	if e.cupsValid && e.in.CUPS.State == "PRINTING" && (paperJam || coverOpen || outputFull || paperEmpty) {
		record(ContradictionHardwareFault, "contradiction: CUPS reports queue PRINTING while physical hardware fault is confirmed")
	}

	if e.ippValid && e.snmpValid && e.snmpHealth.Online {
		if e.ippPaperEmpty && e.snmpHealth.PaperTrayStatus == "OK" {
			record(ContradictionHardwareFault, "contradiction: IPP reports media empty while SNMP explicitly reports paper tray OK")
		} else if e.snmpPaperEmpty && !e.ippPaperEmpty && e.in.IPP.State == IPPPrinterStateIdle {
			record(ObservationMismatchHardware, "observation mismatch: SNMP reports paper tray EMPTY, but IPP state reasons do not list media empty")
		}
		if e.ippJam && !e.snmpJam {
			record(ObservationMismatchHardware, "observation mismatch: IPP reports paper jam fault, but SNMP telemetry does not observe a jam condition")
		} else if e.snmpJam && !e.ippJam && e.in.IPP.State == IPPPrinterStateIdle {
			record(ObservationMismatchHardware, "observation mismatch: SNMP reports paper jam fault, but IPP state reasons do not list a jam condition")
		}
		if e.ippCover && !e.snmpCover {
			record(ObservationMismatchHardware, "observation mismatch: IPP reports cover open fault, but SNMP telemetry does not observe an open cover")
		} else if e.snmpCover && !e.ippCover && e.in.IPP.State == IPPPrinterStateIdle {
			record(ObservationMismatchHardware, "observation mismatch: SNMP reports cover open fault, but IPP state reasons do not list cover open")
		}
	}

	if e.cupsValid && e.snmpValid && e.snmpHealth.Online {
		if e.cupsPaperEmpty && e.snmpHealth.PaperTrayStatus == "OK" {
			record(ContradictionHardwareFault, "contradiction: CUPS reports paper tray EMPTY while SNMP explicitly reports paper tray OK")
		} else if e.snmpPaperEmpty && e.in.CUPS.PaperTrayStatus == "OK" {
			record(ContradictionHardwareFault, "contradiction: SNMP reports paper tray EMPTY while CUPS explicitly reports paper tray OK")
		}
		if e.cupsJam && !e.snmpJam {
			record(ObservationMismatchHardware, "observation mismatch: CUPS reports paper jam, but SNMP telemetry does not observe a jam condition")
		}
		if !e.in.CUPS.Online && e.snmpHealth.Online {
			record(ContradictionOnlineStatus, "observation mismatch: CUPS reports queue offline while SNMP device communication is online")
		}
	}

	if e.snmpValid && e.snmpHealth.Online && e.snmpHealth.InputTrayCapacityKnown &&
		e.in.ConfiguredTrayCapacity > 0 && e.snmpHealth.InputTrayCapacity > 0 &&
		e.snmpHealth.InputTrayCapacity != e.in.ConfiguredTrayCapacity {
		msg := fmt.Sprintf("contradiction: SNMP-reported tray capacity (%d) differs from configured tray capacity (%d)", e.snmpHealth.InputTrayCapacity, e.in.ConfiguredTrayCapacity)
		record(ContradictionTrayCapacity, msg)
		e.nonblockingWarnings = appendUnique(e.nonblockingWarnings, msg)
		e.warningsReasons = append(e.warningsReasons, contracts.HealthReason{Code: "tray_capacity_contradiction", Source: "reconciler", Message: msg})
	}
}

func (e *evaluatorState) run() ReconciledHealthResult {
	var rawIPPReasons []string
	if len(e.in.IPP.RawReasons) > 0 {
		rawIPPReasons = append([]string(nil), e.in.IPP.RawReasons...)
	} else {
		for _, pr := range e.in.IPP.ParsedReasons {
			if pr.Raw != "" {
				rawIPPReasons = append(rawIPPReasons, pr.Raw)
			}
		}
	}

	e.validateSources()
	e.analyzeCUPS()
	e.analyzeSNMP()
	e.analyzeIPP()

	paperJam := e.cupsJam || (e.snmpValid && e.snmpJam) || e.ippJam
	coverOpen := e.cupsCover || (e.snmpValid && e.snmpCover) || e.ippCover
	outputFull := e.cupsOutputFull || (e.snmpValid && e.snmpOutputFull) || e.ippOutputFull
	paperEmpty := e.cupsPaperEmpty || (e.snmpValid && e.snmpPaperEmpty) || e.ippPaperEmpty
	paperLow := !paperEmpty && (e.cupsPaperLow || (e.snmpValid && e.snmpPaperLow) || e.ippPaperLow)

	var paperTrayStatus string
	switch {
	case paperEmpty:
		paperTrayStatus = "EMPTY"
	case paperLow:
		paperTrayStatus = "LOW"
	case e.snmpValid && e.snmpHealth.Online && e.snmpHealth.PaperTrayStatus == "OK":
		paperTrayStatus = "OK"
	case e.cupsValid && e.in.CUPS.PaperTrayStatus == "OK":
		paperTrayStatus = "OK"
	default:
		paperTrayStatus = "UNKNOWN"
		e.unknownConditions = appendUnique(e.unknownConditions, "paper tray level unknown")
		e.addDiagnostic(CategoryUnknown, UnknownPaperTray, "Paper tray status is unverified / unknown", SourceReconciler, "", "INFO")
	}

	if paperJam {
		e.addBlocking(FaultPaperJam, "reconciler", "paper jam", SourceReconciler, "paper jam")
	}
	if coverOpen {
		e.addBlocking(FaultCoverOpen, "reconciler", "cover or door open", SourceReconciler, "cover or door open")
	}
	if outputFull {
		e.addBlocking(FaultOutputBinFull, "reconciler", "output bin full", SourceReconciler, "output bin full")
	}
	if paperEmpty {
		e.addBlocking(FaultMediaEmpty, "reconciler", "out of paper / media empty", SourceReconciler, "out of paper / media empty")
	}

	appendExtraFaults := func(faults []string, src HealthSource, label string) {
		for _, f := range faults {
			lower := strings.ToLower(f)
			if strings.Contains(lower, "paper jam") || strings.Contains(lower, "media-jam") ||
				strings.Contains(lower, "cover") || strings.Contains(lower, "door-open") ||
				strings.Contains(lower, "output bin full") || strings.Contains(lower, "output-bin-full") ||
				strings.Contains(lower, "media empty") || strings.Contains(lower, "media-empty") || strings.Contains(lower, "media-needed") {
				continue
			}
			e.addBlocking(FaultHardwareError, label, f, src, f)
		}
	}
	appendExtraFaults(e.cupsFaults, SourceCUPS, "cups")
	appendExtraFaults(e.snmpFaults, SourceSNMP, "snmp")
	appendExtraFaults(e.ippFaults, SourceIPP, "ipp")

	if paperLow {
		e.nonblockingWarnings = appendUnique(e.nonblockingWarnings, "paper tray low")
		e.warningsReasons = append(e.warningsReasons, contracts.HealthReason{Code: "paper_low", Source: "reconciler", Message: "paper tray low"})
	}
	for _, w := range append(append([]string(nil), e.cupsWarnings...), append(e.snmpWarnings, e.ippWarnings...)...) {
		if !strings.Contains(w, "paper tray low") && !strings.Contains(w, "media-low") {
			e.nonblockingWarnings = appendUnique(e.nonblockingWarnings, w)
			e.warningsReasons = append(e.warningsReasons, contracts.HealthReason{Code: "warning", Source: "reconciler", Message: w})
		}
	}

	e.detectContradictions(paperJam, coverOpen, outputFull, paperEmpty)

	// Separate operational state determination
	hasHardwareFault := paperJam || coverOpen || outputFull || paperEmpty ||
		(e.snmpValid && (e.snmpHealth.State == "ERROR" || e.snmpDeviceDown || e.snmpCriticalAlert)) ||
		(e.ippValid && e.in.IPP.HasHardwareFault) ||
		(e.cupsValid && e.in.CUPS.State == "ERROR")

	var effectiveState string
	var online bool

	switch {
	case e.evalTime.IsZero():
		effectiveState = "UNKNOWN"
		online = false
	case hasHardwareFault:
		effectiveState = "ERROR"
		online = false
	case e.cupsValid && (!e.in.CUPS.Online || e.in.CUPS.State == "OFFLINE"):
		effectiveState = "OFFLINE"
		online = false
	case len(e.blockingFaults) > 0:
		effectiveState = "ERROR"
		online = false
	case e.cupsValid && e.in.CUPS.State == "PRINTING":
		effectiveState = "PRINTING"
		online = true
	case e.cupsValid && e.snmpValid && e.snmpHealth.Online && e.snmpHealth.State == "WARMING_UP":
		effectiveState = "WARMING_UP"
		online = true
	case e.cupsValid && (e.in.CUPS.State == "READY" || e.in.CUPS.State == "IDLE"):
		effectiveState = "IDLE"
		online = true
	case e.cupsValid && e.in.CUPS.State != "" && e.in.CUPS.State != "UNKNOWN":
		effectiveState = e.in.CUPS.State
		online = e.in.CUPS.Online
	default:
		effectiveState = "UNKNOWN"
		online = false
	}

	// Canonical EvaluatedHealth readiness (FAULT > UNVERIFIED > HEALTHY)
	evaluatedHealth := contracts.EvaluatedHealthUnverified
	switch {
	case len(e.blockingFaults) > 0 || hasHardwareFault || effectiveState == "ERROR" || effectiveState == "OFFLINE":
		evaluatedHealth = contracts.EvaluatedHealthFault
	case e.evalTime.IsZero() || effectiveState == "UNKNOWN" || effectiveState == "":
		evaluatedHealth = contracts.EvaluatedHealthUnverified
	case e.in.RequireCUPS && !e.cupsValid:
		evaluatedHealth = contracts.EvaluatedHealthUnverified
	case e.in.RequireIPP && (!e.ippValid || e.in.IPP.State == IPPPrinterStateUnknown):
		evaluatedHealth = contracts.EvaluatedHealthUnverified
	case e.in.RequireNativeIPP && e.in.IPP.Source != CapabilitySourceNativeIPP:
		evaluatedHealth = contracts.EvaluatedHealthUnverified
	case e.in.RequireSNMP && (!e.snmpValid || !e.snmpHealth.Online || e.snmpHealth.State == "UNKNOWN" || e.snmpHealth.State == ""):
		evaluatedHealth = contracts.EvaluatedHealthUnverified
	default:
		evaluatedHealth = contracts.EvaluatedHealthHealthy
	}

	estimatedSheets := 0
	if e.cupsValid {
		estimatedSheets = e.in.CUPS.EstimatedSheets
	}
	if estimatedSheets == 0 && e.snmpValid && e.snmpHealth.EstimatedSheets > 0 {
		estimatedSheets = e.snmpHealth.EstimatedSheets
	}

	var tonerPct, drumPct int
	var tonerKnown, drumKnown bool
	trayLvl, trayCap := -1, -1
	var trayLvlKnown, trayCapKnown bool

	if e.snmpValid && e.snmpHealth.Online {
		tonerPct = e.snmpHealth.TonerLevelPercent
		tonerKnown = e.snmpHealth.TonerLevelKnown || e.snmpHealth.TonerLevelPercent > 0
		drumPct = e.snmpHealth.DrumLifePercent
		drumKnown = e.snmpHealth.DrumLifeKnown || e.snmpHealth.DrumLifePercent > 0
		if e.snmpHealth.InputTrayLevelKnown {
			trayLvl = e.snmpHealth.InputTrayLevel
			trayLvlKnown = true
		}
		if e.snmpHealth.InputTrayCapacityKnown {
			trayCap = e.snmpHealth.InputTrayCapacity
			trayCapKnown = true
		}
	}

	snmpQueried := e.in.SNMPQueried || e.snmpHealth.Online || e.snmpHealth.ErrorMessage != "" || !e.snmpHealth.UpdatedAt.IsZero()
	cupsQueried := e.in.CUPSQueried || e.cupsValid || !e.in.CUPS.UpdatedAt.IsZero()

	attribution := SourceAttribution{
		CUPS: SubsystemAttribution{
			Queried:    cupsQueried,
			Available:  e.cupsValid,
			State:      e.in.CUPS.State,
			Faults:     e.cupsFaults,
			Warnings:   e.cupsWarnings,
			RawDetails: e.in.CUPS.ErrorMessage,
		},
		IPP: SubsystemAttribution{
			Queried:    !e.in.IPP.ObservedAt.IsZero() || e.in.IPP.EndpointReachable || e.in.IPP.Error != "",
			Available:  e.ippValid,
			State:      string(e.in.IPP.State),
			Faults:     e.ippFaults,
			Warnings:   e.ippWarnings,
			RawDetails: e.in.IPP.Error,
		},
		SNMP: SubsystemAttribution{
			Queried:    snmpQueried,
			Available:  e.snmpValid && e.snmpHealth.Online,
			State:      e.snmpHealth.State,
			Faults:     e.snmpFaults,
			Warnings:   e.snmpWarnings,
			RawDetails: e.snmpHealth.ErrorMessage,
		},
	}

	freshness := &contracts.HealthEvidenceFreshness{
		CUPS:      e.cupsFresh,
		NativeIPP: e.ippFresh,
		SNMP:      e.snmpFresh,
		MaxAgeSec: e.maxAge.Seconds(),
	}

	// Anchor effective readiness timestamp to the oldest required fresh source observation so
	// evaluation/reconciliation never extends source lifetime beyond 6s from oldest observation.
	effectiveUpdatedAt := e.evalTime
	for _, src := range []contracts.SourceFreshness{e.cupsFresh, e.ippFresh, e.snmpFresh} {
		if src.Available && src.ObservedAt != nil && !src.ObservedAt.IsZero() {
			if effectiveUpdatedAt.IsZero() || src.ObservedAt.Before(effectiveUpdatedAt) {
				effectiveUpdatedAt = *src.ObservedAt
			}
		}
	}

	return ReconciledHealthResult{
		EffectiveState:         effectiveState,
		EvaluatedHealth:        evaluatedHealth,
		Online:                 online,
		BlockingFaults:         e.blockingFaults,
		NonblockingWarnings:    e.nonblockingWarnings,
		UnknownConditions:      e.unknownConditions,
		Contradictions:         e.contradictions,
		SourceAttribution:      attribution,
		RawIPPReasons:          rawIPPReasons,
		PaperJam:               paperJam,
		CoverOpen:              coverOpen,
		OutputBinFull:          outputFull,
		PaperTrayStatus:        paperTrayStatus,
		EstimatedSheets:        estimatedSheets,
		TonerLevelPercent:      tonerPct,
		TonerLevelKnown:        tonerKnown,
		DrumLifePercent:        drumPct,
		DrumLifeKnown:          drumKnown,
		InputTrayLevel:         trayLvl,
		InputTrayLevelKnown:    trayLvlKnown,
		InputTrayCapacity:      trayCap,
		InputTrayCapacityKnown: trayCapKnown,
		BlockingReasons:        dedupeHealthReasons(e.blockingReasons),
		Warnings:               dedupeHealthReasons(e.warningsReasons),
		EvidenceFreshness:      freshness,
		EvaluatedAt:            e.evalTime,
		EffectiveUpdatedAt:     effectiveUpdatedAt,
		DiagnosticDetails:      e.diagnosticDetails,
	}
}

func isInherentlyCriticalIPPKeyword(kw string) bool {
	switch kw {
	case "media-jam", "paper-jam",
		"media-empty", "media-needed", "input-tray-missing",
		"cover-open", "door-open", "interlock-open",
		"output-bin-full", "output-area-full",
		"marker-supply-empty", "toner-empty", "ink-empty",
		"marker-waste-full",
		"fuser-over-temp", "fuser-under-temp":
		return true
	default:
		return false
	}
}

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

func sliceHasItemContaining(slice []string, substr string) bool {
	lowerSub := strings.ToLower(substr)
	for _, item := range slice {
		if strings.Contains(strings.ToLower(item), lowerSub) {
			return true
		}
	}
	return false
}
