package spooler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/pkg/contracts"
)

// SubsystemSampleInfo records diagnostics, staleness, and age for a single subsystem sample.
type SubsystemSampleInfo struct {
	Queried   bool          `json:"queried"`
	Available bool          `json:"available"`
	Stale     bool          `json:"stale"`
	SampleAge time.Duration `json:"sample_age"`
	Timestamp time.Time     `json:"timestamp"`
	State     string        `json:"state,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// ProductionHealthDecision records the production poller's operational and hardware fault decision.
type ProductionHealthDecision struct {
	Available     bool          `json:"available"`
	Online        bool          `json:"online"`
	State         string        `json:"state"`
	HardwareFault bool          `json:"hardware_fault"`
	FaultReason   string        `json:"fault_reason,omitempty"`
	Timestamp     time.Time     `json:"timestamp"`
	SampleAge     time.Duration `json:"sample_age"`
	Reason        string        `json:"reason,omitempty"`
}

// ShadowHealthDifferences highlights discrepancies between shadow reconciliation and production decisions.
type ShadowHealthDifferences struct {
	BaselineAvailable bool     `json:"baseline_available"`
	StateDiff         bool     `json:"state_diff"`
	OnlineDiff        bool     `json:"online_diff"`
	FaultDiff         bool     `json:"fault_diff"`
	Details           []string `json:"details,omitempty"`
}

// HasDifferences returns true only if a valid production baseline is available AND discrepancies exist.
func (d ShadowHealthDifferences) HasDifferences() bool {
	return d.BaselineAvailable && (d.StateDiff || d.OnlineDiff || d.FaultDiff)
}

// ShadowHealthReport contains the complete diagnostic report produced by a shadow health check cycle.
type ShadowHealthReport struct {
	EvaluatedAt        time.Time                      `json:"evaluated_at"`
	CUPSQueue          string                         `json:"cups_queue"`
	Reconciled         printer.ReconciledHealthResult `json:"reconciled"`
	ProductionDecision ProductionHealthDecision       `json:"production_decision"`
	Differences        ShadowHealthDifferences        `json:"differences"`
	CUPSSample         SubsystemSampleInfo            `json:"cups_sample"`
	SNMPSample         SubsystemSampleInfo            `json:"snmp_sample"`
	IPPSample          SubsystemSampleInfo            `json:"ipp_sample"`
	RawCUPS            contracts.PrinterHealth        `json:"raw_cups,omitempty"`
	RawSNMP            contracts.PrinterHealth        `json:"raw_snmp,omitempty"`
	RawIPP             printer.IPPPrinterStateResult  `json:"raw_ipp,omitempty"`
	QueryErrors        []string                       `json:"query_errors,omitempty"`
}

func copyStringSlice(src []string) []string {
	if src == nil {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

func copyDiagnosticItems(src []printer.DiagnosticItem) []printer.DiagnosticItem {
	if src == nil {
		return nil
	}
	dst := make([]printer.DiagnosticItem, len(src))
	copy(dst, src)
	return dst
}

func copyIPPReasons(src []printer.IPPPrinterReason) []printer.IPPPrinterReason {
	if src == nil {
		return nil
	}
	dst := make([]printer.IPPPrinterReason, len(src))
	copy(dst, src)
	return dst
}

// DeepCopy returns an independent, defensive deep copy of the ShadowHealthReport.
func (r *ShadowHealthReport) DeepCopy() *ShadowHealthReport {
	if r == nil {
		return nil
	}
	cp := *r

	cp.QueryErrors = copyStringSlice(r.QueryErrors)
	cp.Differences.Details = copyStringSlice(r.Differences.Details)

	rec := r.Reconciled
	rec.BlockingFaults = copyStringSlice(r.Reconciled.BlockingFaults)
	rec.NonblockingWarnings = copyStringSlice(r.Reconciled.NonblockingWarnings)
	rec.UnknownConditions = copyStringSlice(r.Reconciled.UnknownConditions)
	rec.Contradictions = copyStringSlice(r.Reconciled.Contradictions)
	rec.RawIPPReasons = copyStringSlice(r.Reconciled.RawIPPReasons)
	rec.DiagnosticDetails = copyDiagnosticItems(r.Reconciled.DiagnosticDetails)

	rec.SourceAttribution.CUPS.Faults = copyStringSlice(r.Reconciled.SourceAttribution.CUPS.Faults)
	rec.SourceAttribution.CUPS.Warnings = copyStringSlice(r.Reconciled.SourceAttribution.CUPS.Warnings)
	rec.SourceAttribution.IPP.Faults = copyStringSlice(r.Reconciled.SourceAttribution.IPP.Faults)
	rec.SourceAttribution.IPP.Warnings = copyStringSlice(r.Reconciled.SourceAttribution.IPP.Warnings)
	rec.SourceAttribution.SNMP.Faults = copyStringSlice(r.Reconciled.SourceAttribution.SNMP.Faults)
	rec.SourceAttribution.SNMP.Warnings = copyStringSlice(r.Reconciled.SourceAttribution.SNMP.Warnings)
	cp.Reconciled = rec

	rawIPP := r.RawIPP
	rawIPP.RawReasons = copyStringSlice(r.RawIPP.RawReasons)
	rawIPP.ParsedReasons = copyIPPReasons(r.RawIPP.ParsedReasons)
	cp.RawIPP = rawIPP

	return &cp
}

// LatestShadowReport returns a defensive deep copy of the latest shadow health report, or nil if none has run.
func (c *Client) LatestShadowReport() *ShadowHealthReport {
	c.shadowMu.RLock()
	defer c.shadowMu.RUnlock()
	if c.latestShadow == nil {
		return nil
	}
	return c.latestShadow.DeepCopy()
}

// LatestProductionHealth returns the latest health evaluation from the production poller.
func (c *Client) LatestProductionHealth() contracts.PrinterHealth {
	c.prodHealthMu.RLock()
	defer c.prodHealthMu.RUnlock()
	return c.latestProdHealth
}

// computeShadowDifferences compares the reconciled health against the production decision.
func computeShadowDifferences(rec printer.ReconciledHealthResult, prod ProductionHealthDecision) ShadowHealthDifferences {
	if !prod.Available {
		return ShadowHealthDifferences{
			BaselineAvailable: false,
			Details:           []string{"production baseline unavailable; difference classification skipped"},
		}
	}

	var details []string

	// Compare operational state, normalizing READY and IDLE as equivalent
	normRecState := rec.EffectiveState
	if normRecState == "READY" {
		normRecState = "IDLE"
	}
	normProdState := prod.State
	if normProdState == "READY" {
		normProdState = "IDLE"
	}

	stateDiff := normRecState != normProdState
	if stateDiff {
		details = append(details, fmt.Sprintf("state mismatch: shadow reconciled=%s vs production=%s", rec.EffectiveState, prod.State))
	}

	onlineDiff := rec.Online != prod.Online
	if onlineDiff {
		details = append(details, fmt.Sprintf("online mismatch: shadow reconciled=%t vs production=%t", rec.Online, prod.Online))
	}

	faultDiff := rec.HasBlockingFaults() != prod.HardwareFault
	if faultDiff {
		details = append(details, fmt.Sprintf("fault mismatch: shadow blocking_faults=%v vs production hardware_fault=%t (reason=%q)",
			rec.BlockingFaults, prod.HardwareFault, prod.FaultReason))
	}

	return ShadowHealthDifferences{
		BaselineAvailable: true,
		StateDiff:         stateDiff,
		OnlineDiff:        onlineDiff,
		FaultDiff:         faultDiff,
		Details:           details,
	}
}

// RunShadowHealthCheck executes a single diagnostic-only shadow health check cycle:
// 1. Verifies that a CUPS queue is configured. In raw PJL mode (empty queue), returns nil immediately.
// 2. Polls CUPS health independently (not from combined poller).
// 3. Polls SNMP health independently (not from combined poller).
// 4. Queries fresh IPP printer state via QueryIPPPrinterState.
// 5. Evaluates sample ages against evalTime; rejects stale or invalid CUPS/SNMP observations
//    from active reconciliation evidence (including fault flags, paper status, and error text inference).
// 6. Calls printer.ReconcilePrinterHealth(reconciledCUPS, rawIPP, reconciledSNMP, evalTime).
// 7. Compares against cached production health baseline only (never calling healthPoller directly);
//    if baseline is absent or stale, marks it unavailable and skips authoritative difference classification.
// 8. Stores a defensive deep copy of the ShadowHealthReport and logs structured source-attributed diagnostics.
//
// Invariants:
// - Read-only: never modifies hardwareFault, fault counters, screens, cloud messages, or active jobs.
// - Bounded: all queries are timeout-bounded and honor context cancellation without false cancellation errors.
// - Nonblocking: query errors never crash or block the daemon.
func (c *Client) RunShadowHealthCheck(ctx context.Context) *ShadowHealthReport {
	if ctx == nil || ctx.Err() != nil {
		return nil
	}

	cupsQ := strings.TrimSpace(c.cfg.CUPSQueue)
	if cupsQ == "" {
		return nil
	}

	sheets := 0
	if c.cfg.StateStore != nil {
		if s, err := c.cfg.StateStore.GetEstimatedSheets(ctx); err == nil {
			sheets = s
		}
	}

	const queryTimeout = 3 * time.Second
	var queryErrors []string

	// 1. Independent CUPS query
	cupsCtx, cupsCancel := context.WithTimeout(ctx, queryTimeout)
	var rawCUPS contracts.PrinterHealth
	if c.cfg.MockCUPSHealth != nil {
		rawCUPS = c.cfg.MockCUPSHealth(cupsCtx, sheets)
	} else if c.cfg.CUPSRunner != nil {
		rawCUPS = printer.PollCUPSHealthCustom(cupsCtx, c.cfg.CUPSRunner, cupsQ, sheets)
	} else {
		rawCUPS = printer.PollCUPSHealth(cupsCtx, cupsQ, sheets)
	}
	cupsCtxErr := cupsCtx.Err() // Capture context error BEFORE calling cancel()
	cupsCancel()

	var cupsErr error
	if cupsCtxErr != nil {
		if errors.Is(cupsCtxErr, context.DeadlineExceeded) {
			cupsErr = fmt.Errorf("CUPS query timed out: %w", cupsCtxErr)
		} else if errors.Is(cupsCtxErr, context.Canceled) {
			cupsErr = fmt.Errorf("CUPS query canceled: %w", cupsCtxErr)
		} else {
			cupsErr = fmt.Errorf("CUPS query context error: %w", cupsCtxErr)
		}
	} else if !rawCUPS.Online && rawCUPS.ErrorMessage != "" {
		cupsErr = fmt.Errorf("CUPS query error: %s", rawCUPS.ErrorMessage)
	}
	if cupsErr != nil {
		queryErrors = append(queryErrors, cupsErr.Error())
	}

	// 2. Independent SNMP query
	snmpCtx, snmpCancel := context.WithTimeout(ctx, queryTimeout)
	var rawSNMP contracts.PrinterHealth
	snmpTarget := strings.TrimSpace(c.cfg.SNMPTarget)
	snmpQueried := snmpTarget != "" || c.cfg.MockSNMPHealth != nil

	if c.cfg.MockSNMPHealth != nil {
		rawSNMP = c.cfg.MockSNMPHealth(snmpCtx, sheets)
	} else if snmpTarget != "" {
		snmpCfg := printer.DefaultSNMPConfig()
		snmpCfg.Target = snmpTarget
		if c.cfg.SNMPPort > 0 {
			snmpCfg.Port = c.cfg.SNMPPort
		}
		if c.cfg.SNMPCommunity != "" {
			snmpCfg.Community = c.cfg.SNMPCommunity
		}
		rawSNMP = printer.PollHealthConfig(snmpCtx, snmpCfg, sheets)
	} else {
		rawSNMP = contracts.PrinterHealth{
			Online:       false,
			State:        "UNKNOWN",
			ErrorMessage: "SNMP target not configured",
		}
	}
	snmpCtxErr := snmpCtx.Err() // Capture context error BEFORE calling cancel()
	snmpCancel()

	var snmpErr error
	if snmpQueried {
		if snmpCtxErr != nil {
			if errors.Is(snmpCtxErr, context.DeadlineExceeded) {
				snmpErr = fmt.Errorf("SNMP query timed out: %w", snmpCtxErr)
			} else if errors.Is(snmpCtxErr, context.Canceled) {
				snmpErr = fmt.Errorf("SNMP query canceled: %w", snmpCtxErr)
			} else {
				snmpErr = fmt.Errorf("SNMP query context error: %w", snmpCtxErr)
			}
		} else if !rawSNMP.Online && rawSNMP.ErrorMessage != "" {
			snmpErr = fmt.Errorf("SNMP query error: %s", rawSNMP.ErrorMessage)
		}
		if snmpErr != nil {
			queryErrors = append(queryErrors, snmpErr.Error())
		}
	}

	// 3. Fresh IPP query
	ippCtx, ippCancel := context.WithTimeout(ctx, queryTimeout)
	var rawIPP printer.IPPPrinterStateResult
	var ippExecErr error
	if c.cfg.MockIPPState != nil {
		rawIPP, ippExecErr = c.cfg.MockIPPState(ippCtx, cupsQ)
	} else {
		rawIPP, ippExecErr = printer.QueryIPPPrinterState(ippCtx, c.cfg.CUPSRunner, cupsQ)
	}
	ippCtxErr := ippCtx.Err() // Capture context error BEFORE calling cancel()
	ippCancel()

	var ippErr error
	if ippCtxErr != nil {
		if errors.Is(ippCtxErr, context.DeadlineExceeded) {
			ippErr = fmt.Errorf("IPP query timed out: %w", ippCtxErr)
		} else if errors.Is(ippCtxErr, context.Canceled) {
			ippErr = fmt.Errorf("IPP query canceled: %w", ippCtxErr)
		} else {
			ippErr = fmt.Errorf("IPP query context error: %w", ippCtxErr)
		}
	} else if ippExecErr != nil {
		ippErr = fmt.Errorf("IPP query error: %w", ippExecErr)
	} else if !rawIPP.QueueAccessible && rawIPP.Error != "" {
		ippErr = fmt.Errorf("IPP query queue inaccessible: %s", rawIPP.Error)
	}
	if ippErr != nil {
		queryErrors = append(queryErrors, ippErr.Error())
	}

	// 4. Sample Age & Staleness Evaluation
	evalTime := time.Now().UTC()

	// Analyze CUPS sample and prepare active evidence
	cupsSample := SubsystemSampleInfo{
		Queried:   true,
		Timestamp: rawCUPS.UpdatedAt,
		State:     rawCUPS.State,
	}
	reconciledCUPS := rawCUPS

	if rawCUPS.UpdatedAt.IsZero() {
		cupsSample.Stale = true
		cupsSample.Available = false
		cupsSample.Error = "missing observation timestamp"

		// Reject missing timestamp from active reconciliation evidence:
		// Strip all fault flags, paper status, and error text fault inference.
		reconciledCUPS = contracts.PrinterHealth{
			Online:          false,
			State:           "UNKNOWN",
			PaperTrayStatus: "UNKNOWN",
			ErrorMessage:     "CUPS observation missing timestamp; unverified",
		}
	} else {
		cupsAge := evalTime.Sub(rawCUPS.UpdatedAt)
		cupsSample.SampleAge = cupsAge
		if cupsAge > printer.MaxIPPObservationAge || cupsAge < -printer.FutureTimestampTolerance {
			cupsSample.Stale = true
			cupsSample.Available = false
			cupsSample.Error = fmt.Sprintf("stale observation: age %v exceeds limit %v", cupsAge.Round(time.Millisecond), printer.MaxIPPObservationAge)

			// Reject stale observation from active reconciliation evidence:
			// Strip all fault flags, paper status, and error text fault inference.
			reconciledCUPS = contracts.PrinterHealth{
				Online:          false,
				State:           "UNKNOWN",
				PaperTrayStatus: "UNKNOWN",
				ErrorMessage:     fmt.Sprintf("CUPS observation stale (age %v); unverified", cupsAge.Round(time.Millisecond)),
				UpdatedAt:        rawCUPS.UpdatedAt,
			}
		} else {
			cupsSample.Available = rawCUPS.Online
			if cupsErr != nil && cupsSample.Error == "" {
				cupsSample.Error = cupsErr.Error()
			}
		}
	}

	// Analyze SNMP sample and prepare active evidence
	snmpSample := SubsystemSampleInfo{
		Queried:   snmpQueried,
		Timestamp: rawSNMP.UpdatedAt,
		State:     rawSNMP.State,
	}
	reconciledSNMP := rawSNMP

	if !snmpQueried {
		snmpSample.Available = false
		snmpSample.State = "UNCONFIGURED"
		reconciledSNMP = contracts.PrinterHealth{
			Online:          false,
			State:           "UNKNOWN",
			PaperTrayStatus: "UNKNOWN",
			ErrorMessage:     "SNMP target not configured",
		}
	} else if rawSNMP.UpdatedAt.IsZero() {
		snmpSample.Stale = true
		snmpSample.Available = false
		snmpSample.Error = "missing observation timestamp"

		// Reject missing timestamp from active reconciliation evidence:
		// Strip all fault flags, paper status, consumable levels, and error text fault inference.
		reconciledSNMP = contracts.PrinterHealth{
			Online:          false,
			State:           "UNKNOWN",
			PaperTrayStatus: "UNKNOWN",
			ErrorMessage:     "SNMP observation missing timestamp; unverified",
		}
	} else {
		snmpAge := evalTime.Sub(rawSNMP.UpdatedAt)
		snmpSample.SampleAge = snmpAge
		if snmpAge > printer.MaxIPPObservationAge || snmpAge < -printer.FutureTimestampTolerance {
			snmpSample.Stale = true
			snmpSample.Available = false
			snmpSample.Error = fmt.Sprintf("stale observation: age %v exceeds limit %v", snmpAge.Round(time.Millisecond), printer.MaxIPPObservationAge)

			// Reject stale observation from active reconciliation evidence:
			// Strip all fault flags, paper status, consumable levels, and error text fault inference.
			reconciledSNMP = contracts.PrinterHealth{
				Online:          false,
				State:           "UNKNOWN",
				PaperTrayStatus: "UNKNOWN",
				ErrorMessage:     fmt.Sprintf("SNMP observation stale (age %v); unverified", snmpAge.Round(time.Millisecond)),
				UpdatedAt:        rawSNMP.UpdatedAt,
			}
		} else {
			snmpSample.Available = rawSNMP.Online
			if snmpErr != nil && snmpSample.Error == "" {
				snmpSample.Error = snmpErr.Error()
			}
		}
	}

	// Analyze IPP sample
	ippSample := SubsystemSampleInfo{
		Queried:   true,
		Timestamp: rawIPP.ObservedAt,
		State:     string(rawIPP.State),
	}
	if rawIPP.ObservedAt.IsZero() {
		ippSample.Stale = true
		ippSample.Available = false
		ippSample.Error = "missing observation timestamp"
	} else {
		ippAge := evalTime.Sub(rawIPP.ObservedAt)
		ippSample.SampleAge = ippAge
		if ippAge > printer.MaxIPPObservationAge || ippAge < -printer.FutureTimestampTolerance {
			ippSample.Stale = true
			ippSample.Available = false
			ippSample.Error = fmt.Sprintf("stale observation: age %v exceeds limit %v", ippAge.Round(time.Millisecond), printer.MaxIPPObservationAge)
		} else if !rawIPP.QueueAccessible || rawIPP.Error != "" {
			ippSample.Available = false
			ippSample.Error = rawIPP.Error
			if ippErr != nil && ippSample.Error == "" {
				ippSample.Error = ippErr.Error()
			}
		} else {
			ippSample.Available = true
		}
	}

	// 5. Reconcile observations using explicit evaluation timestamp
	// Stale CUPS/SNMP observations have been sanitized of fault flags and active health claims.
	// Raw observations are preserved separately in rawCUPS/rawSNMP/rawIPP for diagnostics.
	reconciled := printer.ReconcilePrinterHealth(reconciledCUPS, rawIPP, reconciledSNMP, evalTime)

	// 6. Compare against cached production health baseline ONLY
	// Do NOT call c.healthPoller(); compare only against c.LatestProductionHealth().
	prodHealth := c.LatestProductionHealth()

	var prodAge time.Duration
	if !prodHealth.UpdatedAt.IsZero() {
		prodAge = evalTime.Sub(prodHealth.UpdatedAt)
	}

	baselineAvailable := !prodHealth.UpdatedAt.IsZero() &&
		prodAge <= printer.MaxIPPObservationAge &&
		prodAge >= -printer.FutureTimestampTolerance

	var prodDecision ProductionHealthDecision
	var diffs ShadowHealthDifferences

	if baselineAvailable {
		isFault, reason, _ := isBlockingHardwareFault(prodHealth)
		prodFault := isFault || c.isHardwareFaultActive()
		prodReason := reason
		if prodReason == "" {
			prodReason = c.getFaultReason()
		}

		prodDecision = ProductionHealthDecision{
			Available:     true,
			Online:        prodHealth.Online,
			State:         prodHealth.State,
			HardwareFault: prodFault,
			FaultReason:   prodReason,
			Timestamp:     prodHealth.UpdatedAt,
			SampleAge:     prodAge,
		}
		diffs = computeShadowDifferences(reconciled, prodDecision)
	} else {
		baselineReason := "production health baseline absent (no check has run yet)"
		if !prodHealth.UpdatedAt.IsZero() {
			baselineReason = fmt.Sprintf("production health baseline stale (age %v exceeds limit %v)", prodAge.Round(time.Millisecond), printer.MaxIPPObservationAge)
		}
		prodDecision = ProductionHealthDecision{
			Available: false,
			Timestamp: prodHealth.UpdatedAt,
			SampleAge: prodAge,
			Reason:    baselineReason,
		}
		// Do not classify an unavailable baseline as a real mismatch
		diffs = ShadowHealthDifferences{
			BaselineAvailable: false,
			StateDiff:         false,
			OnlineDiff:        false,
			FaultDiff:         false,
			Details:           []string{baselineReason + "; difference classification skipped"},
		}
	}

	report := &ShadowHealthReport{
		EvaluatedAt:        evalTime,
		CUPSQueue:          cupsQ,
		Reconciled:         reconciled,
		ProductionDecision: prodDecision,
		Differences:        diffs,
		CUPSSample:         cupsSample,
		SNMPSample:         snmpSample,
		IPPSample:          ippSample,
		RawCUPS:            rawCUPS,
		RawSNMP:            rawSNMP,
		RawIPP:             rawIPP,
		QueryErrors:        queryErrors,
	}

	// Defensive deep copy before persisting in client state
	deepCopy := report.DeepCopy()
	c.shadowMu.Lock()
	c.latestShadow = deepCopy
	c.shadowMu.Unlock()

	// 7. Structured diagnostic logging
	if !prodDecision.Available {
		log.Printf("[SHADOW-HEALTH] Queue %q Reconciled: state=%s, online=%t, faults=%v, warnings=%v | Production baseline unavailable (%s) | Sources[CUPS=(state=%s, age=%s), SNMP=(state=%s, age=%s), IPP=(state=%s, age=%s)]",
			cupsQ, reconciled.EffectiveState, reconciled.Online, reconciled.BlockingFaults, reconciled.NonblockingWarnings,
			prodDecision.Reason,
			cupsSample.State, cupsSample.SampleAge.Round(time.Millisecond),
			snmpSample.State, snmpSample.SampleAge.Round(time.Millisecond),
			ippSample.State, ippSample.SampleAge.Round(time.Millisecond),
		)
	} else if diffs.HasDifferences() || len(reconciled.Contradictions) > 0 {
		log.Printf("[SHADOW-HEALTH] DIFFERENCE DETECTED for queue %q: Reconciled(state=%s, online=%t, faults=%v, warnings=%v) | Production(state=%s, online=%t, fault=%t, reason=%q) | DiffDetails=%v | Contradictions=%v | Sources[CUPS=(state=%s, avail=%t, stale=%t, age=%s), SNMP=(state=%s, avail=%t, stale=%t, age=%s), IPP=(state=%s, avail=%t, stale=%t, age=%s)]",
			cupsQ, reconciled.EffectiveState, reconciled.Online, reconciled.BlockingFaults, reconciled.NonblockingWarnings,
			prodDecision.State, prodDecision.Online, prodDecision.HardwareFault, prodDecision.FaultReason,
			diffs.Details, reconciled.Contradictions,
			cupsSample.State, cupsSample.Available, cupsSample.Stale, cupsSample.SampleAge.Round(time.Millisecond),
			snmpSample.State, snmpSample.Available, snmpSample.Stale, snmpSample.SampleAge.Round(time.Millisecond),
			ippSample.State, ippSample.Available, ippSample.Stale, ippSample.SampleAge.Round(time.Millisecond),
		)
	} else {
		log.Printf("[SHADOW-HEALTH] Queue %q Reconciled: state=%s, online=%t, faults=%v, warnings=%v | Production match: state=%s, online=%t, fault=%t | Sources[CUPS=(state=%s, age=%s), SNMP=(state=%s, age=%s), IPP=(state=%s, age=%s)]",
			cupsQ, reconciled.EffectiveState, reconciled.Online, reconciled.BlockingFaults, reconciled.NonblockingWarnings,
			prodDecision.State, prodDecision.Online, prodDecision.HardwareFault,
			cupsSample.State, cupsSample.SampleAge.Round(time.Millisecond),
			snmpSample.State, snmpSample.SampleAge.Round(time.Millisecond),
			ippSample.State, ippSample.SampleAge.Round(time.Millisecond),
		)
	}

	return report.DeepCopy()
}

// shadowHealthLoop executes periodic shadow health checks until context cancellation.
func (c *Client) shadowHealthLoop(ctx context.Context) {
	interval := c.cfg.ShadowHealthInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial check on loop startup
	c.RunShadowHealthCheck(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.RunShadowHealthCheck(ctx)
		}
	}
}
