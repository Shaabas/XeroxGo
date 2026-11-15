package printer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IPPPrinterState represents the normalized operational state of a CUPS/IPP printer.
type IPPPrinterState string

const (
	IPPPrinterStateIdle       IPPPrinterState = "idle"
	IPPPrinterStateProcessing IPPPrinterState = "processing"
	IPPPrinterStateStopped    IPPPrinterState = "stopped"
	IPPPrinterStateUnknown    IPPPrinterState = "unknown"
)

// AcceptingJobsStatus represents the job admission state of a CUPS/IPP queue.
type AcceptingJobsStatus string

const (
	AcceptingJobsUnknown      AcceptingJobsStatus = "UNKNOWN"
	AcceptingJobsAccepting    AcceptingJobsStatus = "ACCEPTING"
	AcceptingJobsNotAccepting AcceptingJobsStatus = "NOT_ACCEPTING"
)

// ReasonSeverity represents the severity level of an IPP printer-state-reason.
type ReasonSeverity string

const (
	SeverityError   ReasonSeverity = "error"
	SeverityWarning ReasonSeverity = "warning"
	SeverityReport  ReasonSeverity = "report"
	SeverityNone    ReasonSeverity = "none"
)

// IPPPrinterReason contains a structured reason keyword, its extracted severity, and the raw string.
type IPPPrinterReason struct {
	Raw      string         `json:"raw"`
	Keyword  string         `json:"keyword"`
	Severity ReasonSeverity `json:"severity"`
}

// IPPPrinterStateResult holds normalized and raw state details retrieved from a CUPS queue via IPP.
// Note: This reflects the state reported by the CUPS queue and its driver,
// not verified physical hardware status.
type IPPPrinterStateResult struct {
	// Source identifies where this state was queried from (always CapabilitySourceCUPSQueue).
	Source string `json:"source"`
	// Queue is the name of the CUPS queue queried.
	Queue string `json:"queue"`

	// State is the normalized IPP operational state ("idle", "processing", "stopped", "unknown").
	State IPPPrinterState `json:"state"`
	// RawState is the exact raw string returned from IPP (e.g. "idle", "3", "stopped").
	RawState string `json:"raw_state,omitempty"`

	// AcceptingJobs indicates whether the CUPS queue is currently accepting print jobs.
	AcceptingJobs AcceptingJobsStatus `json:"accepting_jobs"`

	// RawReasons is the list of raw reason tokens parsed from printer-state-reasons.
	RawReasons []string `json:"raw_reasons,omitempty"`
	// ParsedReasons contains structured reasons with base keyword and severity.
	ParsedReasons []IPPPrinterReason `json:"parsed_reasons,omitempty"`

	// Recognized physical/hardware conditions parsed from exact reason keywords:
	PaperJam      bool `json:"paper_jam"`
	MediaEmpty    bool `json:"media_empty"`
	MediaLow      bool `json:"media_low"`
	CoverOpen     bool `json:"cover_open"`
	OutputBinFull bool `json:"output_bin_full"`
	MarkerEmpty   bool `json:"marker_empty"`
	MarkerLow     bool `json:"marker_low"`
	OtherError    bool `json:"other_error"`

	// HasHardwareFault indicates whether a blocking hardware error was detected in reasons.
	HasHardwareFault bool `json:"has_hardware_fault"`

	// QueueAccessible indicates whether CUPS responded to the IPP query without transport error.
	QueueAccessible bool `json:"queue_accessible"`

	// StatusCode is the IPP status code (e.g. "successful-ok", "client-error-not-found").
	StatusCode string `json:"status_code,omitempty"`

	// ObservedAt is the observation timestamp for this sample.
	ObservedAt time.Time `json:"observed_at"`

	// Error records any failure encountered during discovery.
	Error string `json:"error,omitempty"`
}

// HasRecognizedStateAttributes returns true if at least one meaningful printer state attribute was parsed.
func (r IPPPrinterStateResult) HasRecognizedStateAttributes() bool {
	return r.RawState != "" ||
		r.AcceptingJobs != AcceptingJobsUnknown ||
		len(r.RawReasons) > 0
}

// NormalizeIPPPrinterState normalizes raw IPP printer-state values,
// supporting strings ("idle", "processing", "stopped") and numeric enums (3, 4, 5).
func NormalizeIPPPrinterState(raw string) (IPPPrinterState, string) {
	rawTrimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(rawTrimmed)
	if lower == "" {
		return IPPPrinterStateUnknown, ""
	}

	switch {
	case lower == "idle" || lower == "3" || strings.HasPrefix(lower, "3 ") || strings.HasPrefix(lower, "3("):
		return IPPPrinterStateIdle, rawTrimmed
	case lower == "processing" || lower == "4" || strings.HasPrefix(lower, "4 ") || strings.HasPrefix(lower, "4("):
		return IPPPrinterStateProcessing, rawTrimmed
	case lower == "stopped" || lower == "5" || strings.HasPrefix(lower, "5 ") || strings.HasPrefix(lower, "5("):
		return IPPPrinterStateStopped, rawTrimmed
	default:
		return IPPPrinterStateUnknown, rawTrimmed
	}
}

// NormalizeAcceptingJobs normalizes printer-is-accepting-jobs boolean strings.
func NormalizeAcceptingJobs(raw string) AcceptingJobsStatus {
	lower := strings.ToLower(strings.TrimSpace(raw))
	switch lower {
	case "true", "1", "yes":
		return AcceptingJobsAccepting
	case "false", "0", "no":
		return AcceptingJobsNotAccepting
	default:
		return AcceptingJobsUnknown
	}
}

// parseIPPReason separates an IPP reason token into its base keyword and severity suffix.
func parseIPPReason(raw string) (IPPPrinterReason, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return IPPPrinterReason{}, false
	}

	lower := strings.ToLower(raw)
	var keyword string
	var severity ReasonSeverity

	switch {
	case strings.HasSuffix(lower, "-error"):
		keyword = strings.TrimSuffix(lower, "-error")
		severity = SeverityError
	case strings.HasSuffix(lower, "-warning"):
		keyword = strings.TrimSuffix(lower, "-warning")
		severity = SeverityWarning
	case strings.HasSuffix(lower, "-report"):
		keyword = strings.TrimSuffix(lower, "-report")
		severity = SeverityReport
	default:
		keyword = lower
		severity = SeverityNone
	}

	return IPPPrinterReason{
		Raw:      raw,
		Keyword:  keyword,
		Severity: severity,
	}, true
}

// ParseIPPPrinterState parses the output of `ipptool -tv ... get-printer-attributes.test`
// for printer-state, printer-state-reasons, and printer-is-accepting-jobs.
func ParseIPPPrinterState(out []byte, queue string) IPPPrinterStateResult {
	now := time.Now().UTC()
	res := IPPPrinterStateResult{
		Source:        CapabilitySourceCUPSQueue,
		Queue:         queue,
		State:         IPPPrinterStateUnknown,
		AcceptingJobs: AcceptingJobsUnknown,
		ObservedAt:    now,
	}

	if len(out) == 0 {
		return res
	}

	singleAttrs := make(map[string]string)
	var rawReasons []string

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		m := ippAttributeRegex.FindStringSubmatch(line)
		if len(m) < 3 {
			continue
		}

		attrName := strings.ToLower(strings.TrimSpace(m[1]))
		val := strings.TrimSpace(m[2])

		if _, exists := singleAttrs[attrName]; !exists {
			singleAttrs[attrName] = val
		}

		if attrName == "printer-state-reasons" {
			parts := splitIPPValues(val)
			for _, p := range parts {
				if !strings.EqualFold(p, "none") && p != "" {
					rawReasons = append(rawReasons, p)
				}
			}
		}
	}

	// 1. Status Code
	if v, ok := singleAttrs["status-code"]; ok {
		if token := statusCodeTokenRegex.FindString(strings.TrimSpace(v)); token != "" {
			res.StatusCode = strings.ToLower(token)
		} else {
			res.StatusCode = strings.ToLower(strings.TrimSpace(v))
		}
	}

	// 2. printer-state
	if v, ok := singleAttrs["printer-state"]; ok {
		res.State, res.RawState = NormalizeIPPPrinterState(v)
	}

	// 3. printer-is-accepting-jobs
	if v, ok := singleAttrs["printer-is-accepting-jobs"]; ok {
		res.AcceptingJobs = NormalizeAcceptingJobs(v)
	}

	// 4. printer-state-reasons
	res.RawReasons = rawReasons
	for _, rawReason := range rawReasons {
		parsed, ok := parseIPPReason(rawReason)
		if !ok {
			continue
		}
		res.ParsedReasons = append(res.ParsedReasons, parsed)

		// Exact keyword matching - avoid substring collisions
		switch parsed.Keyword {
		case "media-jam", "paper-jam":
			res.PaperJam = true
			if parsed.Severity == SeverityError || parsed.Severity == SeverityNone {
				res.HasHardwareFault = true
			}
		case "media-empty", "media-needed", "input-tray-missing":
			res.MediaEmpty = true
			if parsed.Severity == SeverityError || parsed.Severity == SeverityNone {
				res.HasHardwareFault = true
			}
		case "media-low":
			res.MediaLow = true
		case "door-open", "cover-open", "interlock-open":
			res.CoverOpen = true
			if parsed.Severity == SeverityError || parsed.Severity == SeverityNone {
				res.HasHardwareFault = true
			}
		case "output-area-full", "output-bin-full":
			res.OutputBinFull = true
			if parsed.Severity == SeverityError || parsed.Severity == SeverityNone {
				res.HasHardwareFault = true
			}
		case "marker-supply-empty", "toner-empty", "ink-empty":
			res.MarkerEmpty = true
			if parsed.Severity == SeverityError || parsed.Severity == SeverityNone {
				res.HasHardwareFault = true
			}
		case "marker-supply-low", "toner-low", "ink-low":
			res.MarkerLow = true
		case "marker-waste-full":
			res.OtherError = true
			res.HasHardwareFault = true
		default:
			if parsed.Severity == SeverityError {
				res.OtherError = true
				res.HasHardwareFault = true
			}
		}
	}

	return res
}

// QueryIPPPrinterState queries Get-Printer-Attributes via ipptool for printer-state,
// printer-state-reasons, and printer-is-accepting-jobs.
// It returns a normalized result distinguishing queue accessibility, job acceptance,
// operational state, and recognized hardware fault indicators.
func QueryIPPPrinterState(ctx context.Context, runner CommandRunner, queue string) (IPPPrinterStateResult, error) {
	now := time.Now().UTC()
	queue = strings.TrimSpace(queue)
	if queue == "" {
		return IPPPrinterStateResult{
			Source:        CapabilitySourceCUPSQueue,
			State:         IPPPrinterStateUnknown,
			AcceptingJobs: AcceptingJobsUnknown,
			ObservedAt:    now,
			Error:         "cups queue name cannot be empty",
		}, errors.New("cups queue name cannot be empty")
	}

	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		ctx = context.Background()
	}

	ippURI := fmt.Sprintf("ipp://localhost:631/printers/%s", queue)
	out, runnerErr := runner(ctx, "ipptool", "-tv", ippURI, "get-printer-attributes.test")

	// Parse whatever attributes were received in output to preserve partial data
	res := ParseIPPPrinterState(out, queue)

	// 1. Runner execution error (absent binary, network failure, timeout, exit code)
	if runnerErr != nil {
		res.QueueAccessible = false
		res.Error = runnerErr.Error()
		return res, fmt.Errorf("ipptool state query failed for CUPS queue %q: %w", queue, runnerErr)
	}

	// 2. Explicit IPP failure status (e.g. client-error-not-found, server-error-internal-error)
	if isIPPFailureStatus(res.StatusCode) {
		res.QueueAccessible = false
		res.Error = fmt.Sprintf("IPP failure status %q", res.StatusCode)
		return res, fmt.Errorf("IPP state query returned failure status %q for CUPS queue %q", res.StatusCode, queue)
	}

	// 3. Lack of recognizable printer state attributes
	if !res.HasRecognizedStateAttributes() {
		res.QueueAccessible = false
		res.Error = "no recognizable printer state attributes found in IPP response"
		return res, fmt.Errorf("no recognizable printer state attributes found in IPP response for CUPS queue %q", queue)
	}

	res.QueueAccessible = true
	return res, nil
}
