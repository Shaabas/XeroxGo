package printer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"xeroxgo/pkg/contracts"
)

// CommandRunner executes a command with context and returns combined output or stdout.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var defaultCommandRunner CommandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

var (
	cupsJobIDRegex = regexp.MustCompile(`(?i)request id is ([a-zA-Z0-9_\-\.]+)`)

	ippJobStateRegex        = regexp.MustCompile(`(?m)^\s*job-state\s*\([^\)]+\)\s*=\s*([a-zA-Z0-9_\-]+)`)
	ippJobStateReasonsRegex = regexp.MustCompile(`(?m)^\s*job-state-reasons\s*\([^\)]+\)\s*=\s*([^\r\n]+)`)
	ippSheetsCompletedRegex = regexp.MustCompile(`(?m)^\s*job-media-sheets-completed\s*\([^\)]+\)\s*=\s*([0-9]+)`)
	ippStatusCodeRegex      = regexp.MustCompile(`(?m)^\s*status-code\s*=\s*([a-zA-Z0-9_\-]+)`)
)

// CUPSJobState represents the authoritative normalized state of a CUPS/IPP print job.
type CUPSJobState string

const (
	CUPSJobStatePending           CUPSJobState = "pending"
	CUPSJobStateHeld              CUPSJobState = "pending-held"
	CUPSJobStateProcessing        CUPSJobState = "processing"
	CUPSJobStateProcessingStopped CUPSJobState = "processing-stopped"
	CUPSJobStateCompleted         CUPSJobState = "completed"
	CUPSJobStateCanceled          CUPSJobState = "canceled"
	CUPSJobStateAborted           CUPSJobState = "aborted"
	CUPSJobStateUnknown           CUPSJobState = "unknown"
)

// CUPSJobInfo holds status details retrieved via IPP Get-Job-Attributes or lpstat.
type CUPSJobInfo struct {
	State           CUPSJobState
	Reasons         []string
	SheetsCompleted int
	RawStatus       string
}

// QueryCUPSJobInfo queries the authoritative state of a CUPS job.
// It prefers IPP Get-Job-Attributes via ipptool, falling back to lpstat when ipptool is unavailable.
func QueryCUPSJobInfo(ctx context.Context, runner CommandRunner, queue string, cupsJobID string) (CUPSJobInfo, error) {
	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		return CUPSJobInfo{State: CUPSJobStateUnknown}, errors.New("context cannot be nil")
	}

	// 1. Preferred: Query via IPP Get-Job-Attributes (ipptool) if numeric job ID is extractable
	jobNum := extractJobNumber(cupsJobID)
	if jobNum != "" {
		ippURI := fmt.Sprintf("ipp://localhost:631/jobs/%s", jobNum)
		out, err := runner(ctx, "ipptool", "-tv", ippURI, "get-job-attributes.test")
		if err == nil {
			info, ok := parseIPPJobInfo(out)
			if ok {
				return info, nil
			}
		} else if isIPPNotFoundError(out, err) {
			// Job explicitly not found or already purged in CUPS
			return CUPSJobInfo{
				State:     CUPSJobStateUnknown,
				RawStatus: strings.TrimSpace(string(out)),
			}, nil
		}
	}

	// 2. Fallback: Query via lpstat
	return queryLPStatJobInfo(ctx, runner, queue, cupsJobID)
}

// HoldCUPSJob places an active or queued CUPS job on hold so it will not print unmonitored.
func HoldCUPSJob(ctx context.Context, runner CommandRunner, cupsJobID string) error {
	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if strings.TrimSpace(cupsJobID) == "" {
		return errors.New("cupsJobID cannot be empty")
	}
	out, err := runner(ctx, "lp", "-i", cupsJobID, "-H", "hold")
	if err != nil {
		return fmt.Errorf("failed to hold CUPS job %s: %w (output: %s)", cupsJobID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HoldResult represents the verified outcome of attempting to hold a CUPS job.
type HoldResult struct {
	HoldRequested    bool
	HoldErr          error
	State            CUPSJobState
	JobInfo          CUPSJobInfo
	QueryErr         error
	ConfirmedHeld    bool
	StillProcessing  bool
	AlreadyCompleted bool
	IsUnknown        bool
}

// HoldAndVerifyCUPSJob requests that CUPS hold the job, authoritatively queries its resulting
// state, and distinguishes confirmed held, failed, unknown, already completed, and still processing.
// Note: Even when confirmed held, already-buffered sheets in printer hardware memory may continue
// printing before physical ejection ceases.
func HoldAndVerifyCUPSJob(ctx context.Context, runner CommandRunner, queue string, cupsJobID string) HoldResult {
	if runner == nil {
		runner = defaultCommandRunner
	}
	res := HoldResult{HoldRequested: true}

	if strings.TrimSpace(cupsJobID) == "" {
		res.HoldErr = errors.New("cupsJobID cannot be empty")
		res.IsUnknown = true
		return res
	}

	res.HoldErr = HoldCUPSJob(ctx, runner, cupsJobID)
	if res.HoldErr != nil {
		log.Printf("[CUPS] Warning: hold request for job %s failed: %v", cupsJobID, res.HoldErr)
	}

	info, qErr := QueryCUPSJobInfo(ctx, runner, queue, cupsJobID)
	res.QueryErr = qErr
	res.JobInfo = info
	res.State = info.State

	if qErr != nil {
		log.Printf("[CUPS] Warning: error querying job %s state after hold request: %v", cupsJobID, qErr)
		res.IsUnknown = true
		return res
	}

	switch info.State {
	case CUPSJobStateHeld:
		res.ConfirmedHeld = true
		log.Printf("[CUPS] Job %s confirmed HELD in CUPS (note: buffered sheets may continue printing)", cupsJobID)
	case CUPSJobStateProcessing, CUPSJobStatePending:
		res.StillProcessing = true
		log.Printf("[CUPS] Warning: Job %s still %s after hold request; unmonitored sheet ejection risk", cupsJobID, info.State)
	case CUPSJobStateCompleted:
		res.AlreadyCompleted = true
		log.Printf("[CUPS] Job %s was already COMPLETED in CUPS before or during hold request", cupsJobID)
	case CUPSJobStateProcessingStopped:
		log.Printf("[CUPS] Job %s is processing-stopped in CUPS: %v", cupsJobID, info.Reasons)
	case CUPSJobStateCanceled, CUPSJobStateAborted:
		log.Printf("[CUPS] Job %s is %s in CUPS", cupsJobID, info.State)
	default:
		res.IsUnknown = true
		log.Printf("[CUPS] Warning: Job %s state is %s (unknown/ambiguous) after hold request", cupsJobID, info.State)
	}

	return res
}

func extractJobNumber(cupsJobID string) string {
	parts := strings.Split(cupsJobID, "-")
	last := parts[len(parts)-1]
	if _, err := strconv.Atoi(last); err == nil {
		return last
	}
	return ""
}

func parseIPPJobInfo(out []byte) (CUPSJobInfo, bool) {
	m := ippJobStateRegex.FindSubmatch(out)
	if len(m) < 2 {
		return CUPSJobInfo{}, false
	}

	rawState := strings.ToLower(strings.TrimSpace(string(m[1])))
	var state CUPSJobState
	switch rawState {
	case "pending":
		state = CUPSJobStatePending
	case "pending-held", "held":
		state = CUPSJobStateHeld
	case "processing":
		state = CUPSJobStateProcessing
	case "processing-stopped":
		state = CUPSJobStateProcessingStopped
	case "completed":
		state = CUPSJobStateCompleted
	case "canceled", "cancelled":
		state = CUPSJobStateCanceled
	case "aborted":
		state = CUPSJobStateAborted
	default:
		state = CUPSJobStateUnknown
	}

	var reasons []string
	if rm := ippJobStateReasonsRegex.FindSubmatch(out); len(rm) > 1 {
		for _, r := range strings.Split(string(rm[1]), ",") {
			r = strings.TrimSpace(r)
			if r != "" && r != "none" {
				reasons = append(reasons, r)
			}
		}
	}

	sheets := 0
	if sm := ippSheetsCompletedRegex.FindSubmatch(out); len(sm) > 1 {
		sheets, _ = strconv.Atoi(string(sm[1]))
	}

	return CUPSJobInfo{
		State:           state,
		Reasons:         reasons,
		SheetsCompleted: sheets,
		RawStatus:       string(out),
	}, true
}

func isIPPNotFoundError(out []byte, err error) bool {
	if len(out) > 0 {
		lower := strings.ToLower(string(out))
		if strings.Contains(lower, "not-found") || strings.Contains(lower, "does not exist") {
			return true
		}
	}
	return false
}

func queryLPStatJobInfo(ctx context.Context, runner CommandRunner, queue string, cupsJobID string) (CUPSJobInfo, error) {
	// A. Check active (not-completed) queue
	activeOut, activeErr := runner(ctx, "lpstat", "-W", "not-completed", "-o", queue)
	if activeErr == nil && strings.Contains(string(activeOut), cupsJobID) {
		if strings.Contains(strings.ToLower(string(activeOut)), "held") {
			return CUPSJobInfo{
				State:     CUPSJobStateHeld,
				Reasons:   []string{"job-held"},
				RawStatus: strings.TrimSpace(string(activeOut)),
			}, nil
		}
		// Job is active in CUPS queue. Check if printer queue is stopped or actively printing.
		pOut, pErr := runner(ctx, "lpstat", "-p", queue)
		if pErr == nil {
			pRaw := strings.ToLower(string(pOut))
			if strings.Contains(pRaw, "disabled") || strings.Contains(pRaw, "stopped") {
				if strings.Contains(pRaw, "filter failed") || strings.Contains(pRaw, "filter-failed") {
					return CUPSJobInfo{
						State:     CUPSJobStateAborted,
						Reasons:   []string{strings.TrimSpace(string(pOut))},
						RawStatus: strings.TrimSpace(string(pOut)),
					}, nil
				}
				reason := "printer-stopped"
				if strings.Contains(pRaw, "out of paper") || strings.Contains(pRaw, "media-empty") {
					reason = "media-empty"
				} else if strings.Contains(pRaw, "jam") {
					reason = "media-jam"
				} else if strings.Contains(pRaw, "cover") || strings.Contains(pRaw, "door") {
					reason = "cover-open"
				} else if strings.Contains(pRaw, "paused") {
					reason = "printer-paused"
				}
				return CUPSJobInfo{
					State:     CUPSJobStateProcessingStopped,
					Reasons:   []string{reason},
					RawStatus: strings.TrimSpace(string(pOut)),
				}, nil
			}
			if strings.Contains(pRaw, "now printing") {
				return CUPSJobInfo{
					State:     CUPSJobStateProcessing,
					RawStatus: strings.TrimSpace(string(pOut)),
				}, nil
			}
		}
		// If queue is idle or job waiting its turn
		return CUPSJobInfo{
			State:     CUPSJobStatePending,
			RawStatus: "queued in not-completed",
		}, nil
	}

	// B. Job is not in not-completed. Check completed jobs list.
	compOut, compErr := runner(ctx, "lpstat", "-W", "completed", "-l", "-o", queue)
	if compErr != nil {
		compOut, compErr = runner(ctx, "lpstat", "-W", "completed", "-o", queue)
	}

	if compErr == nil && strings.Contains(string(compOut), cupsJobID) {
		jobBlock := extractJobBlock(string(compOut), cupsJobID)
		lower := strings.ToLower(jobBlock)
		if strings.Contains(lower, "job-completed-successfully") {
			return CUPSJobInfo{
				State:     CUPSJobStateCompleted,
				Reasons:   []string{"job-completed-successfully"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}
		if strings.Contains(lower, "job-canceled") || strings.Contains(lower, "canceled") || strings.Contains(lower, "cancelled") {
			return CUPSJobInfo{
				State:     CUPSJobStateCanceled,
				Reasons:   []string{"job-canceled"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}
		if strings.Contains(lower, "aborted") || strings.Contains(lower, "filter failed") || strings.Contains(lower, "filter-failed") {
			return CUPSJobInfo{
				State:     CUPSJobStateAborted,
				Reasons:   []string{"job-aborted"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}

		// Also check lpstat -W successful for explicit scheduler confirmation
		succOut, succErr := runner(ctx, "lpstat", "-W", "successful", "-o", queue)
		if succErr == nil && strings.Contains(string(succOut), cupsJobID) {
			return CUPSJobInfo{
				State:     CUPSJobStateCompleted,
				Reasons:   []string{"job-completed-successfully"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}

		// In completed list without completion alert: job did not finish successfully
		return CUPSJobInfo{
			State:     CUPSJobStateCanceled,
			Reasons:   []string{"job ended without completion confirmation"},
			RawStatus: strings.TrimSpace(jobBlock),
		}, nil
	}

	// Check if job appears in lpstat -W successful directly
	succOut, succErr := runner(ctx, "lpstat", "-W", "successful", "-o", queue)
	if succErr == nil && strings.Contains(string(succOut), cupsJobID) {
		return CUPSJobInfo{
			State:     CUPSJobStateCompleted,
			Reasons:   []string{"job-completed-successfully"},
			RawStatus: strings.TrimSpace(string(succOut)),
		}, nil
	}

	// C. If both active and completed queries succeeded (or returned clean output),
	// but the job is absent from both lists, state is UNKNOWN.
	if activeErr == nil && compErr == nil {
		return CUPSJobInfo{
			State:     CUPSJobStateUnknown,
			RawStatus: "job not listed in not-completed or completed",
		}, nil
	}

	// If commands had errors (e.g. CUPS communication failure)
	if activeErr != nil {
		return CUPSJobInfo{State: CUPSJobStateUnknown}, activeErr
	}
	return CUPSJobInfo{State: CUPSJobStateUnknown}, compErr
}

func extractJobBlock(output, jobID string) string {
	lines := strings.Split(output, "\n")
	var block []string
	found := false
	for _, line := range lines {
		if strings.Contains(line, jobID) {
			found = true
			block = append(block, line)
			continue
		}
		if found {
			// In lpstat -l, indented lines belong to the current job
			if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
				block = append(block, line)
			} else if strings.TrimSpace(line) != "" {
				// Next job started
				break
			}
		}
	}
	return strings.Join(block, "\n")
}

type cupsContextKey string

const (
	preserveCUPSJobKey cupsContextKey = "preserve_cups_job"
	preserveCheckerKey cupsContextKey = "preserve_checker"
)

// ContextWithPreserveCUPSJob returns a context carrying an atomic boolean flag.
// When the flag is true at cancellation time, MonitorCUPSJob will NOT invoke CUPS cancel.
func ContextWithPreserveCUPSJob(ctx context.Context, flag *atomic.Bool) context.Context {
	return context.WithValue(ctx, preserveCUPSJobKey, flag)
}

// ContextWithPreserveChecker returns a context carrying a dynamic predicate.
// When the predicate returns true at cancellation time, MonitorCUPSJob will NOT invoke CUPS cancel.
func ContextWithPreserveChecker(ctx context.Context, checker func() bool) context.Context {
	return context.WithValue(ctx, preserveCheckerKey, checker)
}

func shouldPreserveCUPSJob(ctx context.Context) bool {
	if flag, ok := ctx.Value(preserveCUPSJobKey).(*atomic.Bool); ok && flag != nil {
		if flag.Load() {
			return true
		}
	}
	if checker, ok := ctx.Value(preserveCheckerKey).(func() bool); ok && checker != nil {
		if checker() {
			return true
		}
	}
	return false
}

// CUPSPrintHooks provides optional callbacks and SNMP configuration during CUPS submission and lifecycle.
type CUPSPrintHooks struct {
	OnSubmitting        func()
	OnSubmitted         func(cupsJobID string)
	SNMPConfig          *SNMPConfig
	PreserveJobOnCancel func() bool
}

// PrintPDFWithCUPS submits a PDF file to the specified CUPS queue via the lp command
// and monitors its progress until completion.
func PrintPDFWithCUPS(
	ctx context.Context,
	queue string,
	cmd contracts.PrintJobCommand,
	pdfPath string,
	onPage func(sheetsCompleted int),
) error {
	return PrintPDFWithCUPSHooksCustom(ctx, defaultCommandRunner, queue, cmd, pdfPath, onPage, 500*time.Millisecond, nil)
}

// PrintPDFWithCUPSCustom allows injection of a custom command runner and polling interval for testing.
func PrintPDFWithCUPSCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	cmd contracts.PrintJobCommand,
	pdfPath string,
	onPage func(sheetsCompleted int),
	pollInterval time.Duration,
) error {
	return PrintPDFWithCUPSHooksCustom(ctx, runner, queue, cmd, pdfPath, onPage, pollInterval, nil)
}

// PrintPDFWithCUPSHooksCustom executes submission with optional pre- and post-submission hooks.
func PrintPDFWithCUPSHooksCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	cmd contracts.PrintJobCommand,
	pdfPath string,
	onPage func(sheetsCompleted int),
	pollInterval time.Duration,
	hooks *CUPSPrintHooks,
) error {
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(queue) == "" {
		return errors.New("queue name cannot be empty")
	}
	if strings.TrimSpace(pdfPath) == "" {
		return errors.New("pdf path cannot be empty")
	}
	if cmd.JobID == "" {
		return errors.New("job_id cannot be empty")
	}
	if cmd.Copies < 1 || cmd.Copies > 999 {
		return fmt.Errorf("copies must be between 1 and 999, got %d", cmd.Copies)
	}
	if cmd.TotalSheets < 1 {
		return fmt.Errorf("total_sheets must be >= 1, got %d", cmd.TotalSheets)
	}
	if runner == nil {
		runner = defaultCommandRunner
	}

	// 1. Build lp command arguments
	lpArgs := []string{
		"-d", queue,
		"-n", strconv.Itoa(cmd.Copies),
		"-t", cmd.JobID,
	}

	switch cmd.Duplex {
	case contracts.DuplexLongEdge:
		lpArgs = append(lpArgs, "-o", "sides=two-sided-long-edge")
	case contracts.DuplexShortEdge:
		lpArgs = append(lpArgs, "-o", "sides=two-sided-short-edge")
	}

	lpArgs = append(lpArgs, pdfPath)

	log.Printf("[CUPS] Submitting print job %s to queue %q: lp %s", cmd.JobID, queue, strings.Join(lpArgs, " "))

	if hooks != nil && hooks.OnSubmitting != nil {
		hooks.OnSubmitting()
	}

	// 2. Submit print job via lp
	out, err := runner(ctx, "lp", lpArgs...)
	if err != nil {
		return fmt.Errorf("failed to submit job to CUPS queue %q: %w (output: %s)", queue, err, strings.TrimSpace(string(out)))
	}

	outputStr := string(out)
	match := cupsJobIDRegex.FindStringSubmatch(outputStr)
	if len(match) < 2 {
		return fmt.Errorf("unable to parse CUPS request id from lp output: %q", strings.TrimSpace(outputStr))
	}
	cupsJobID := match[1]
	log.Printf("[CUPS] Job %s assigned CUPS request ID: %s", cmd.JobID, cupsJobID)

	if hooks != nil && hooks.OnSubmitted != nil {
		hooks.OnSubmitted(cupsJobID)
	}

	var snmpCfg *SNMPConfig
	if hooks != nil {
		snmpCfg = hooks.SNMPConfig
		if hooks.PreserveJobOnCancel != nil {
			ctx = ContextWithPreserveChecker(ctx, hooks.PreserveJobOnCancel)
		}
	}

	// 3. Monitor CUPS job until completed, failed, or context canceled
	return MonitorCUPSJobWithSNMPCustom(ctx, runner, queue, cmd, cupsJobID, onPage, pollInterval, snmpCfg)
}

// MonitorCUPSJobCustom monitors an active or queued CUPS job until completion, failure, or context cancellation.
func MonitorCUPSJobCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	cmd contracts.PrintJobCommand,
	cupsJobID string,
	onPage func(sheetsCompleted int),
	pollInterval time.Duration,
) error {
	return MonitorCUPSJobWithSNMPCustom(ctx, runner, queue, cmd, cupsJobID, onPage, pollInterval, nil)
}

// MonitorCUPSJobWithSNMPCustom monitors an active or queued CUPS job with optional SNMP hardware-truth verification.
func MonitorCUPSJobWithSNMPCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	cmd contracts.PrintJobCommand,
	cupsJobID string,
	onPage func(sheetsCompleted int),
	pollInterval time.Duration,
	snmpCfg *SNMPConfig,
) error {
	var poller func(ctx context.Context) HardwareTelemetry
	if snmpCfg != nil && strings.TrimSpace(snmpCfg.Target) != "" {
		poller = func(c context.Context) HardwareTelemetry {
			return PollHardwareTelemetry(c, *snmpCfg)
		}
	}
	return MonitorCUPSJobWithTelemetryCustom(ctx, runner, queue, cmd, cupsJobID, onPage, pollInterval, poller)
}

// MonitorCUPSJobWithTelemetryCustom monitors an active or queued CUPS job with hardware-truth verification from a telemetry poller.
func MonitorCUPSJobWithTelemetryCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	cmd contracts.PrintJobCommand,
	cupsJobID string,
	onPage func(sheetsCompleted int),
	pollInterval time.Duration,
	telemetryPoller func(ctx context.Context) HardwareTelemetry,
) error {
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if runner == nil {
		runner = defaultCommandRunner
	}
	if strings.TrimSpace(cupsJobID) == "" {
		return errors.New("cupsJobID cannot be empty")
	}
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}

	var initialLife int64 = -1
	var lastOdometerSheets int = 0
	hasOdometer := false

	if telemetryPoller != nil {
		initialTelem := telemetryPoller(ctx)
		if initialTelem.Online && initialTelem.HasLife {
			initialLife = initialTelem.LifeCount
			hasOdometer = true
			log.Printf("[CUPS-SNMP] Baseline hardware odometer for job %s: %d sheets", cmd.JobID, initialLife)
		}
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	consecutiveUnknown := 0
	const maxUnknownRetries = 3

	consecutivePollErrors := 0
	const maxPollErrors = 10

	lastSheetsReported := 0
	var lastState CUPSJobState

	for {
		select {
		case <-ctx.Done():
			if shouldPreserveCUPSJob(ctx) {
				log.Printf("[CUPS] Monitoring stopped for job %s (CUPS ID %s) with job preservation flag; holding job in CUPS for safe reconciliation", cmd.JobID, cupsJobID)
				holdCtx, holdTimeout := context.WithTimeout(context.Background(), 2*time.Second)
				holdRes := HoldAndVerifyCUPSJob(holdCtx, runner, queue, cupsJobID)
				holdTimeout()
				if holdRes.HoldErr != nil {
					log.Printf("[CUPS] Warning: hold request on cancellation failed for job %s: %v", cupsJobID, holdRes.HoldErr)
				}
				return ctx.Err()
			}
			// Attempt to cancel the job in CUPS on timeout or context cancel
			cancelCtx, cancelTimeout := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = runner(cancelCtx, "cancel", cupsJobID)
			cancelTimeout()
			return ctx.Err()

		case <-ticker.C:
			// 1. If hardware telemetry monitoring is active, check real-time hardware status
			if telemetryPoller != nil {
				telem := telemetryPoller(ctx)
				if telem.Online && telem.HasFlags {
					if telem.Flags.Jammed {
						log.Printf("[CUPS-SNMP] Hardware paper jam detected (error 0x04) during job %s; placing job on hold in CUPS", cupsJobID)
						holdCtx, holdTimeout := context.WithTimeout(context.Background(), 2*time.Second)
						holdRes := HoldAndVerifyCUPSJob(holdCtx, runner, queue, cupsJobID)
						holdTimeout()
						if holdRes.HoldErr != nil {
							log.Printf("[CUPS-SNMP] Warning: hold request failed during jam for job %s: %v", cupsJobID, holdRes.HoldErr)
						}
						return errors.New("hardware interruption: paper feed jam detected in printer")
					}
					if telem.Flags.DoorOpen {
						log.Printf("[CUPS-SNMP] Hardware cover open detected during job %s; placing job on hold in CUPS", cupsJobID)
						holdCtx, holdTimeout := context.WithTimeout(context.Background(), 2*time.Second)
						holdRes := HoldAndVerifyCUPSJob(holdCtx, runner, queue, cupsJobID)
						holdTimeout()
						if holdRes.HoldErr != nil {
							log.Printf("[CUPS-SNMP] Warning: hold request failed during cover-open for job %s: %v", cupsJobID, holdRes.HoldErr)
						}
						return errors.New("hardware interruption: printer cover is open")
					}
					if telem.Flags.NoPaper || telem.Flags.InputTrayEmpty {
						log.Printf("[CUPS-SNMP] Hardware out of paper detected during job %s; placing job on hold in CUPS", cupsJobID)
						holdCtx, holdTimeout := context.WithTimeout(context.Background(), 2*time.Second)
						holdRes := HoldAndVerifyCUPSJob(holdCtx, runner, queue, cupsJobID)
						holdTimeout()
						if holdRes.HoldErr != nil {
							log.Printf("[CUPS-SNMP] Warning: hold request failed during paper-empty for job %s: %v", cupsJobID, holdRes.HoldErr)
						}
						return errors.New("hardware interruption: printer paper tray is empty")
					}
				}

				if telem.Online && telem.HasLife && hasOdometer {
					curSheets := int(telem.LifeCount - initialLife)
					if curSheets > lastOdometerSheets && curSheets <= cmd.TotalSheets {
						lastOdometerSheets = curSheets
						log.Printf("[CUPS-SNMP] Verified physical sheets ejected via odometer: %d/%d", curSheets, cmd.TotalSheets)
						if onPage != nil {
							onPage(curSheets)
						}
					}
				}
			}

			// 2. Query CUPS job state
			info, err := QueryCUPSJobInfo(ctx, runner, queue, cupsJobID)
			if err != nil {
				consecutivePollErrors++
				log.Printf("[CUPS] Warning: transient error querying job %s state (%d/%d): %v", cupsJobID, consecutivePollErrors, maxPollErrors, err)
				if consecutivePollErrors >= maxPollErrors {
					return fmt.Errorf("CUPS polling failed repeatedly for queue %q job %s: %w", queue, cupsJobID, err)
				}
				continue
			}
			consecutivePollErrors = 0

			if info.State != lastState {
				log.Printf("[CUPS] Job %s (%s) state: %s (reasons: %v)", cmd.JobID, cupsJobID, info.State, info.Reasons)
				lastState = info.State
			}

			switch info.State {
			case CUPSJobStatePending, CUPSJobStateHeld:
				consecutiveUnknown = 0
				// Queued in CUPS; do not report synthetic progress.

			case CUPSJobStateProcessing:
				consecutiveUnknown = 0
				// If hardware odometer is available, odometer drives onPage.
				// Otherwise fall back to CUPS IPP sheets counter.
				if !hasOdometer {
					if info.SheetsCompleted > lastSheetsReported && info.SheetsCompleted <= cmd.TotalSheets {
						lastSheetsReported = info.SheetsCompleted
						if onPage != nil {
							onPage(info.SheetsCompleted)
						}
					}
				}

			case CUPSJobStateProcessingStopped:
				consecutiveUnknown = 0
				// Recoverable printer pause (e.g. out of paper, paper jam, door open)
				log.Printf("[CUPS] Job %s printer paused/stopped (recoverable): %s. Waiting for printer recovery...", cupsJobID, strings.Join(info.Reasons, ", "))

			case CUPSJobStateCompleted:
				log.Printf("[CUPS] Job %s (CUPS ID %s) confirmed completed by CUPS", cmd.JobID, cupsJobID)
				if !hasOdometer {
					// Fall back to CUPS IPP sheets completed counter if reported,
					// but NEVER fabricate full cmd.TotalSheets solely from CUPS completion.
					if info.SheetsCompleted > lastSheetsReported && info.SheetsCompleted <= cmd.TotalSheets {
						lastSheetsReported = info.SheetsCompleted
						if onPage != nil {
							onPage(info.SheetsCompleted)
						}
					}
				}
				return nil

			case CUPSJobStateCanceled:
				reason := strings.Join(info.Reasons, ", ")
				if reason == "" {
					reason = "job canceled in CUPS"
				}
				log.Printf("[CUPS] Job %s (CUPS ID %s) was canceled in CUPS: %s", cmd.JobID, cupsJobID, reason)
				return fmt.Errorf("CUPS job %s canceled: %s", cupsJobID, reason)

			case CUPSJobStateAborted:
				reason := strings.Join(info.Reasons, ", ")
				if reason == "" {
					reason = "filter failed or printer error"
				}
				log.Printf("[CUPS] Job %s (CUPS ID %s) aborted in CUPS: %s", cmd.JobID, cupsJobID, reason)
				return fmt.Errorf("CUPS printer %q is stopped/disabled: %s", queue, reason)

			case CUPSJobStateUnknown:
				consecutiveUnknown++
				log.Printf("[CUPS] Warning: Job %s state unknown (%d/%d)", cupsJobID, consecutiveUnknown, maxUnknownRetries)
				if consecutiveUnknown >= maxUnknownRetries {
					return fmt.Errorf("job %s state unknown: disappeared from CUPS without completion confirmation", cupsJobID)
				}
			}
		}
	}
}

// ExtractHostFromCUPSDeviceURI parses a CUPS device line like:
// "device for HPTank: ipp://10.42.0.210:631/ipp/print"
// and returns the target host or IP (e.g. "10.42.0.210").
func ExtractHostFromCUPSDeviceURI(rawURI string) string {
	rawURI = strings.TrimSpace(rawURI)
	if idx := strings.Index(rawURI, ":"); idx != -1 && strings.HasPrefix(rawURI, "device for") {
		rawURI = strings.TrimSpace(rawURI[idx+1:])
	}
	u, err := url.Parse(rawURI)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "ipp", "ipps", "socket", "lpd", "http", "https":
		return u.Hostname()
	default:
		return ""
	}
}

// DiscoverQueueHost queries CUPS via lpstat -v to find the device URI host for queue.
func DiscoverQueueHost(ctx context.Context, runner CommandRunner, queue string) string {
	if strings.TrimSpace(queue) == "" {
		return ""
	}
	if runner == nil {
		runner = defaultCommandRunner
	}
	out, err := runner(ctx, "lpstat", "-v", queue)
	if err != nil {
		return ""
	}
	return ExtractHostFromCUPSDeviceURI(string(out))
}

// PollCUPSHealth checks the operational status of a CUPS queue using lpstat.
func PollCUPSHealth(ctx context.Context, queue string, estimatedSheets int) contracts.PrinterHealth {
	return PollCUPSHealthCustom(ctx, defaultCommandRunner, queue, estimatedSheets)
}

// PollCUPSHealthCustom allows injection of a custom command runner for testing CUPS health checks.
// It queries CUPS using 'lpstat -l -p' to inspect extended printer status and state reasons (e.g. media-jam-error),
// falling back to 'lpstat -p' if extended status is unavailable.
func PollCUPSHealthCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	estimatedSheets int,
) contracts.PrinterHealth {
	now := time.Now().UTC()
	if strings.TrimSpace(queue) == "" {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperTrayStatus: "UNKNOWN",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if runner == nil {
		runner = defaultCommandRunner
	}

	out, err := runner(pollCtx, "lpstat", "-l", "-p", queue)
	if err != nil {
		out, err = runner(pollCtx, "lpstat", "-p", queue)
	}
	if err != nil {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			PaperTrayStatus: "UNKNOWN",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	raw := strings.ToLower(string(out))

	// Hardware fault indicators from printer status line and IPP printer-state-reasons
	isJam := strings.Contains(raw, "media-jam") || strings.Contains(raw, "jam")
	isCoverOpen := strings.Contains(raw, "cover-open") || strings.Contains(raw, "door-open") || strings.Contains(raw, "cover open") || strings.Contains(raw, "door open")
	isOutputBinFull := strings.Contains(raw, "output-bin-full") || strings.Contains(raw, "output-area-full") || strings.Contains(raw, "bin full")
	isMediaEmpty := strings.Contains(raw, "out of paper") || strings.Contains(raw, "media-empty") || strings.Contains(raw, "media-needed")
	isStopped := strings.Contains(raw, "disabled") || strings.Contains(raw, "stopped") || strings.Contains(raw, "error") || strings.Contains(raw, "filter failed")

	if isJam {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			CoverOpen:       isCoverOpen,
			OutputBinFull:   isOutputBinFull,
			PaperTrayStatus: "CHECK_PRINTER",
			EstimatedSheets: estimatedSheets,
			ErrorMessage:    "paper jam",
			UpdatedAt:       now,
		}
	}

	if isCoverOpen {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			CoverOpen:       true,
			PaperJam:        isJam,
			OutputBinFull:   isOutputBinFull,
			PaperTrayStatus: "CHECK_PRINTER",
			EstimatedSheets: estimatedSheets,
			ErrorMessage:    "printer cover is open",
			UpdatedAt:       now,
		}
	}

	if isOutputBinFull {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			OutputBinFull:   true,
			PaperJam:        isJam,
			CoverOpen:       isCoverOpen,
			PaperTrayStatus: "CHECK_PRINTER",
			EstimatedSheets: estimatedSheets,
			ErrorMessage:    "output bin full",
			UpdatedAt:       now,
		}
	}

	if isMediaEmpty {
		if isStopped || !strings.Contains(raw, "is idle") {
			return contracts.PrinterHealth{
				Online:          false,
				State:           "ERROR",
				PaperTrayStatus: "EMPTY",
				EstimatedSheets: estimatedSheets,
				ErrorMessage:    "out of paper",
				UpdatedAt:       now,
			}
		}
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "EMPTY",
			EstimatedSheets: estimatedSheets,
			ErrorMessage:    "out of paper",
			UpdatedAt:       now,
		}
	}

	if isStopped {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperTrayStatus: "CHECK_PRINTER",
			EstimatedSheets: estimatedSheets,
			ErrorMessage:    "printer is stopped or disabled",
			UpdatedAt:       now,
		}
	}

	if strings.Contains(raw, "now printing") {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	if strings.Contains(raw, "is idle") {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	// Default fallback for recognized queue
	return contracts.PrinterHealth{
		Online:          true,
		State:           "READY",
		PaperTrayStatus: "OK",
		EstimatedSheets: estimatedSheets,
		UpdatedAt:       now,
	}
}
