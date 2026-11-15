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
	Online        bool   `json:"online"`
	State         string `json:"state"`
	HardwareFault bool   `json:"hardware_fault"`
	FaultReason   string `json:"fault_reason,omitempty"`
}

// ShadowHealthDifferences highlights discrepancies between shadow reconciliation and production decisions.
type ShadowHealthDifferences struct {
	StateDiff  bool     `json:"state_diff"`
	OnlineDiff bool     `json:"online_diff"`
	FaultDiff  bool     `json:"fault_diff"`
	Details    []string `json:"details,omitempty"`
}

// HasDifferences returns true if any discrepancy exists between shadow reconciliation and production.
func (d ShadowHealthDifferences) HasDifferences() bool {
	return d.StateDiff || d.OnlineDiff || d.FaultDiff || len(d.Details) > 0
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
	QueryErrors        []string                       `json:"query_errors,omitempty"`
}

// LatestShadowReport returns a copy of the latest shadow health report, or nil if none has run.
func (c *Client) LatestShadowReport() *ShadowHealthReport {
	c.shadowMu.RLock()
	defer c.shadowMu.RUnlock()
	return c.latestShadow
}

// LatestProductionHealth returns the latest health evaluation from the production poller.
func (c *Client) LatestProductionHealth() contracts.PrinterHealth {
	c.prodHealthMu.RLock()
	defer c.prodHealthMu.RUnlock()
	return c.latestProdHealth
}

// computeShadowDifferences compares the reconciled health against the production decision.
func computeShadowDifferences(rec printer.ReconciledHealthResult, prod ProductionHealthDecision) ShadowHealthDifferences {
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
		StateDiff:  stateDiff,
		OnlineDiff: onlineDiff,
		FaultDiff:  faultDiff,
		Details:    details,
	}
}

// RunShadowHealthCheck executes a single diagnostic-only shadow health check cycle:
// 1. Verifies that a CUPS queue is configured. In raw PJL mode (empty queue), returns nil immediately.
// 2. Polls CUPS health independently (not from combined poller).
// 3. Polls SNMP health independently (not from combined poller).
// 4. Queries fresh IPP printer state via QueryIPPPrinterState.
// 5. Evaluates sample ages against evalTime; rejects stale or missing data from current evidence.
// 6. Calls printer.ReconcilePrinterHealth(cups, ipp, snmp, evalTime).
// 7. Compares the reconciled diagnostic result against current production health decisions.
// 8. Stores the latest ShadowHealthReport and logs structured source-attributed diagnostics.
//
// Invariants:
// - Read-only: never modifies hardwareFault, fault counters, screens, cloud messages, or active jobs.
// - Bounded: all queries are timeout-bounded and honor context cancellation.
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
	var cupsHealth contracts.PrinterHealth
	var cupsErr error
	if c.cfg.MockCUPSHealth != nil {
		cupsHealth = c.cfg.MockCUPSHealth(cupsCtx, sheets)
	} else if c.cfg.CUPSRunner != nil {
		cupsHealth = printer.PollCUPSHealthCustom(cupsCtx, c.cfg.CUPSRunner, cupsQ, sheets)
	} else {
		cupsHealth = printer.PollCUPSHealth(cupsCtx, cupsQ, sheets)
	}
	cupsCancel()
	if cupsCtx.Err() != nil {
		cupsErr = cupsCtx.Err()
	} else if !cupsHealth.Online && cupsHealth.ErrorMessage != "" {
		cupsErr = errors.New(cupsHealth.ErrorMessage)
	}
	if cupsErr != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("CUPS query: %v", cupsErr))
	}

	// 2. Independent SNMP query
	snmpCtx, snmpCancel := context.WithTimeout(ctx, queryTimeout)
	var snmpHealth contracts.PrinterHealth
	var snmpErr error
	snmpTarget := strings.TrimSpace(c.cfg.SNMPTarget)
	snmpQueried := snmpTarget != "" || c.cfg.MockSNMPHealth != nil

	if c.cfg.MockSNMPHealth != nil {
		snmpHealth = c.cfg.MockSNMPHealth(snmpCtx, sheets)
	} else if snmpTarget != "" {
		snmpCfg := printer.DefaultSNMPConfig()
		snmpCfg.Target = snmpTarget
		if c.cfg.SNMPPort > 0 {
			snmpCfg.Port = c.cfg.SNMPPort
		}
		if c.cfg.SNMPCommunity != "" {
			snmpCfg.Community = c.cfg.SNMPCommunity
		}
		snmpHealth = printer.PollHealthConfig(snmpCtx, snmpCfg, sheets)
	} else {
		snmpHealth = contracts.PrinterHealth{
			Online:       false,
			State:        "UNKNOWN",
			ErrorMessage: "SNMP target not configured",
		}
	}
	snmpCancel()
	if snmpQueried {
		if snmpCtx.Err() != nil {
			snmpErr = snmpCtx.Err()
		} else if !snmpHealth.Online && snmpHealth.ErrorMessage != "" {
			snmpErr = errors.New(snmpHealth.ErrorMessage)
		}
		if snmpErr != nil {
			queryErrors = append(queryErrors, fmt.Sprintf("SNMP query: %v", snmpErr))
		}
	}

	// 3. Fresh IPP query
	ippCtx, ippCancel := context.WithTimeout(ctx, queryTimeout)
	var ippState printer.IPPPrinterStateResult
	var ippErr error
	if c.cfg.MockIPPState != nil {
		ippState, ippErr = c.cfg.MockIPPState(ippCtx, cupsQ)
	} else {
		ippState, ippErr = printer.QueryIPPPrinterState(ippCtx, c.cfg.CUPSRunner, cupsQ)
	}
	ippCancel()
	if ippErr != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("IPP query: %v", ippErr))
	}

	// 4. Sample Age & Staleness Evaluation
	evalTime := time.Now().UTC()

	// Analyze CUPS sample
	cupsSample := SubsystemSampleInfo{
		Queried:   true,
		Timestamp: cupsHealth.UpdatedAt,
		State:     cupsHealth.State,
	}
	if cupsHealth.UpdatedAt.IsZero() {
		cupsSample.Stale = true
		cupsSample.Available = false
		cupsSample.Error = "missing observation timestamp"
		cupsHealth.Online = false
		cupsHealth.State = "UNKNOWN"
		cupsHealth.ErrorMessage = "CUPS observation missing timestamp"
	} else {
		cupsAge := evalTime.Sub(cupsHealth.UpdatedAt)
		cupsSample.SampleAge = cupsAge
		if cupsAge > printer.MaxIPPObservationAge || cupsAge < -printer.FutureTimestampTolerance {
			cupsSample.Stale = true
			cupsSample.Available = false
			cupsSample.Error = fmt.Sprintf("stale observation: age %v exceeds limit %v", cupsAge, printer.MaxIPPObservationAge)
			cupsHealth.Online = false
			cupsHealth.State = "UNKNOWN"
			cupsHealth.ErrorMessage = fmt.Sprintf("stale CUPS observation (age %v)", cupsAge)
		} else {
			cupsSample.Available = cupsHealth.Online
			if cupsErr != nil && cupsSample.Error == "" {
				cupsSample.Error = cupsErr.Error()
			}
		}
	}

	// Analyze SNMP sample
	snmpSample := SubsystemSampleInfo{
		Queried:   snmpQueried,
		Timestamp: snmpHealth.UpdatedAt,
		State:     snmpHealth.State,
	}
	if !snmpQueried {
		snmpSample.Available = false
		snmpSample.State = "UNCONFIGURED"
	} else if snmpHealth.UpdatedAt.IsZero() {
		snmpSample.Stale = true
		snmpSample.Available = false
		snmpSample.Error = "missing observation timestamp"
		snmpHealth.Online = false
		snmpHealth.State = "UNKNOWN"
		snmpHealth.ErrorMessage = "SNMP observation missing timestamp"
	} else {
		snmpAge := evalTime.Sub(snmpHealth.UpdatedAt)
		snmpSample.SampleAge = snmpAge
		if snmpAge > printer.MaxIPPObservationAge || snmpAge < -printer.FutureTimestampTolerance {
			snmpSample.Stale = true
			snmpSample.Available = false
			snmpSample.Error = fmt.Sprintf("stale observation: age %v exceeds limit %v", snmpAge, printer.MaxIPPObservationAge)
			snmpHealth.Online = false
			snmpHealth.State = "UNKNOWN"
			snmpHealth.ErrorMessage = fmt.Sprintf("stale SNMP observation (age %v)", snmpAge)
		} else {
			snmpSample.Available = snmpHealth.Online
			if snmpErr != nil && snmpSample.Error == "" {
				snmpSample.Error = snmpErr.Error()
			}
		}
	}

	// Analyze IPP sample
	ippSample := SubsystemSampleInfo{
		Queried:   true,
		Timestamp: ippState.ObservedAt,
		State:     string(ippState.State),
	}
	if ippState.ObservedAt.IsZero() {
		ippSample.Stale = true
		ippSample.Available = false
		ippSample.Error = "missing observation timestamp"
	} else {
		ippAge := evalTime.Sub(ippState.ObservedAt)
		ippSample.SampleAge = ippAge
		if ippAge > printer.MaxIPPObservationAge || ippAge < -printer.FutureTimestampTolerance {
			ippSample.Stale = true
			ippSample.Available = false
			ippSample.Error = fmt.Sprintf("stale observation: age %v exceeds limit %v", ippAge, printer.MaxIPPObservationAge)
		} else if !ippState.QueueAccessible || ippState.Error != "" {
			ippSample.Available = false
			ippSample.Error = ippState.Error
			if ippErr != nil && ippSample.Error == "" {
				ippSample.Error = ippErr.Error()
			}
		} else {
			ippSample.Available = true
		}
	}

	// 5. Reconcile observations using explicit evaluation timestamp
	reconciled := printer.ReconcilePrinterHealth(cupsHealth, ippState, snmpHealth, evalTime)

	// 6. Compare against current production health decision
	prodHealth := c.LatestProductionHealth()
	if prodHealth.UpdatedAt.IsZero() && c.healthPoller != nil {
		prodHealth = c.healthPoller(ctx, sheets)
	}
	isFault, reason, _ := isBlockingHardwareFault(prodHealth)
	prodFault := isFault || c.isHardwareFaultActive()
	prodReason := reason
	if prodReason == "" {
		prodReason = c.getFaultReason()
	}

	prodDecision := ProductionHealthDecision{
		Online:        prodHealth.Online,
		State:         prodHealth.State,
		HardwareFault: prodFault,
		FaultReason:   prodReason,
	}

	diffs := computeShadowDifferences(reconciled, prodDecision)

	report := &ShadowHealthReport{
		EvaluatedAt:        evalTime,
		CUPSQueue:          cupsQ,
		Reconciled:         reconciled,
		ProductionDecision: prodDecision,
		Differences:        diffs,
		CUPSSample:         cupsSample,
		SNMPSample:         snmpSample,
		IPPSample:          ippSample,
		QueryErrors:        queryErrors,
	}

	c.shadowMu.Lock()
	c.latestShadow = report
	c.shadowMu.Unlock()

	// 7. Structured diagnostic logging
	hasDifferences := diffs.HasDifferences() || len(reconciled.Contradictions) > 0
	if hasDifferences {
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

	return report
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
