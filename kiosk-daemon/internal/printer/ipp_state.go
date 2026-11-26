package printer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// IPPPrinterStateResult holds normalized and raw state details retrieved from either
// a local CUPS queue (CapabilitySourceCUPSQueue) or a physical printer's native IPP endpoint
// (CapabilitySourceNativeIPP). Callers must check Source to avoid treating CUPS driver state
// as independent physical printer telemetry.
type IPPPrinterStateResult struct {
	// Source identifies where this state was queried from (CapabilitySourceCUPSQueue or CapabilitySourceNativeIPP).
	Source string `json:"source"`
	// Queue is the name of the CUPS queue queried (for CUPS IPP queries).
	Queue string `json:"queue"`
	// TargetURI is the resolved endpoint URI queried (with any credentials stripped).
	TargetURI string `json:"target_uri,omitempty"`

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

	// EndpointReachable indicates whether the local IPP service responded at the transport layer,
	// proving the CUPS endpoint was reached (even if an IPP failure status such as client-error-not-found was returned).
	EndpointReachable bool `json:"endpoint_reachable"`

	// QueueAccessible indicates whether the specific CUPS queue was confirmed to exist and be accessible
	// (i.e. the endpoint was reached AND returned a successful IPP status for the queue).
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
// for a local CUPS queue (explicitly tagged with CapabilitySourceCUPSQueue).
func ParseIPPPrinterState(out []byte, queue string) IPPPrinterStateResult {
	return ParseIPPPrinterStateWithSource(out, CapabilitySourceCUPSQueue, queue, "")
}

// ParseNativeIPPPrinterState parses the output of `ipptool -tv ... get-printer-attributes.test`
// for a physical printer's native IPP endpoint (explicitly tagged with CapabilitySourceNativeIPP).
func ParseNativeIPPPrinterState(out []byte, targetURI string) IPPPrinterStateResult {
	return ParseIPPPrinterStateWithSource(out, CapabilitySourceNativeIPP, "", targetURI)
}

// ParseIPPPrinterStateWithSource parses `ipptool -tv` state output using canonical IPP semantics
// and tags the origin source explicitly.
func ParseIPPPrinterStateWithSource(out []byte, source, queue, targetURI string) IPPPrinterStateResult {
	now := time.Now().UTC()
	if source == "" {
		source = CapabilitySourceCUPSQueue
	}
	res := IPPPrinterStateResult{
		Source:        source,
		Queue:         queue,
		TargetURI:     stripURLCredentials(targetURI),
		State:         IPPPrinterStateUnknown,
		AcceptingJobs: AcceptingJobsUnknown,
		ObservedAt:    now,
	}

	if len(out) == 0 {
		return res
	}

	singleAttrs, rawAttrs, statusCode := parseIPPAttributes(out, false)
	res.StatusCode = statusCode

	if v, ok := singleAttrs["printer-state"]; ok {
		res.State, res.RawState = NormalizeIPPPrinterState(v)
	}
	if v, ok := singleAttrs["printer-is-accepting-jobs"]; ok {
		res.AcceptingJobs = NormalizeAcceptingJobs(v)
	}

	var rawReasons []string
	for _, p := range rawAttrs["printer-state-reasons"] {
		if !strings.EqualFold(p, "none") && p != "" {
			rawReasons = append(rawReasons, p)
		}
	}
	res.RawReasons = rawReasons
	applyIPPStateReasons(&res, rawReasons)

	return res
}

// applyIPPStateReasons classifies RFC 8011 printer-state-reasons by keyword semantics rather
// than severity suffix alone. Confirmed physical blocking faults (jam, media-empty, cover-open,
// output-bin-full, empty marker supplies, waste full) block even when reported with "-report"
// or "-warning" suffixes. Non-blocking supply/media low conditions remain warnings.
func applyIPPStateReasons(res *IPPPrinterStateResult, rawReasons []string) {
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
			res.HasHardwareFault = true
		case "media-empty", "media-needed", "input-tray-missing":
			res.MediaEmpty = true
			res.HasHardwareFault = true
		case "media-low":
			res.MediaLow = true
		case "door-open", "cover-open", "interlock-open":
			res.CoverOpen = true
			res.HasHardwareFault = true
		case "output-area-full", "output-bin-full":
			res.OutputBinFull = true
			res.HasHardwareFault = true
		case "marker-supply-empty", "toner-empty", "ink-empty":
			res.MarkerEmpty = true
			res.HasHardwareFault = true
		case "marker-supply-low", "toner-low", "ink-low":
			res.MarkerLow = true
		case "marker-waste-almost-full":
			res.OtherError = true
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
}

// QueryIPPPrinterState queries Get-Printer-Attributes via ipptool against local CUPS IPP
// (`ipp://localhost:631/printers/<queue>`). This observes CUPS queue state and driver-reported
// state, NOT independent physical printer telemetry.
func QueryIPPPrinterState(ctx context.Context, runner CommandRunner, queue string) (IPPPrinterStateResult, error) {
	queue = strings.TrimSpace(queue)
	if queue == "" {
		return IPPPrinterStateResult{
			Source:        CapabilitySourceCUPSQueue,
			State:         IPPPrinterStateUnknown,
			AcceptingJobs: AcceptingJobsUnknown,
			ObservedAt:    time.Now().UTC(),
			Error:         "cups queue name cannot be empty",
		}, errors.New("cups queue name cannot be empty")
	}

	ippURI := fmt.Sprintf("ipp://localhost:631/printers/%s", queue)
	return queryIPPEndpointState(ctx, runner, CapabilitySourceCUPSQueue, queue, ippURI)
}

// QueryNativeIPPPrinterState queries Get-Printer-Attributes directly from a physical printer's
// resolved native IPP endpoint (`targetURI`). It rejects empty URIs and local CUPS queue URIs
// so CUPS state cannot be misrepresented as independent physical IPP telemetry.
func QueryNativeIPPPrinterState(ctx context.Context, runner CommandRunner, targetURI string) (IPPPrinterStateResult, error) {
	trimmedURI := strings.TrimSpace(targetURI)
	if err := validateNativeIPPEndpointURI(trimmedURI); err != nil {
		return IPPPrinterStateResult{
			Source:        CapabilitySourceNativeIPP,
			TargetURI:     stripURLCredentials(trimmedURI),
			State:         IPPPrinterStateUnknown,
			AcceptingJobs: AcceptingJobsUnknown,
			ObservedAt:    time.Now().UTC(),
			Error:         err.Error(),
		}, err
	}
	return queryIPPEndpointState(ctx, runner, CapabilitySourceNativeIPP, "", trimmedURI)
}

func validateNativeIPPEndpointURI(rawURI string) error {
	if rawURI == "" {
		return errors.New("native IPP target URI cannot be empty")
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return fmt.Errorf("invalid native IPP URI %q: %w", stripURLCredentials(rawURI), err)
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	switch scheme {
	case "ipp", "ipps", "http", "https":
	default:
		return fmt.Errorf("unsupported native IPP URI scheme %q", u.Scheme)
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("native IPP URI %q lacks hostname", stripURLCredentials(rawURI))
	}
	// Guard against mistaking local CUPS /printers/<queue> on port 631 for native printer telemetry.
	if IsLoopbackHost(u.Hostname()) && (u.Port() == "631" || (u.Port() == "" && (scheme == "ipp" || scheme == "ipps"))) &&
		strings.HasPrefix(strings.ToLower(u.Path), "/printers/") {
		return fmt.Errorf("refusing local CUPS queue URI %q as native physical IPP endpoint", stripURLCredentials(rawURI))
	}
	return nil
}

// ResolveNativeIPPEndpoint resolves the physical printer's native IPP URI from an explicit
// override or the CUPS queue's configured device URI (`lpstat -v` + verified DNS-SD resolution).
// It never hardcodes a printer IP or guesses an unverified IPP resource path such as /ipp/print.
func ResolveNativeIPPEndpoint(ctx context.Context, runner CommandRunner, queue, explicitOverrideURI string) (string, error) {
	if override := strings.TrimSpace(explicitOverrideURI); override != "" {
		if err := validateNativeIPPEndpointURI(override); err != nil {
			return "", err
		}
		return override, nil
	}

	queue = strings.TrimSpace(queue)
	if queue == "" {
		return "", errors.New("cannot resolve native IPP endpoint: CUPS queue name is empty")
	}
	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		ctx = context.Background()
	}

	deviceURI, err := DiscoverQueueDeviceURI(ctx, runner, queue)
	if err != nil {
		return "", err
	}
	lowerURI := strings.ToLower(deviceURI)
	switch {
	case strings.HasPrefix(lowerURI, "ipp://") ||
		strings.HasPrefix(lowerURI, "ipps://") ||
		strings.HasPrefix(lowerURI, "http://") ||
		strings.HasPrefix(lowerURI, "https://"):
		if err := validateNativeIPPEndpointURI(deviceURI); err != nil {
			return "", err
		}
		return deviceURI, nil

	case strings.HasPrefix(lowerURI, "dnssd://"):
		endpoint, rErr := ResolveDNSSDTarget(ctx, runner, deviceURI)
		if rErr != nil {
			return "", fmt.Errorf("DNS-SD native IPP resolution failed for %q: %w", stripURLCredentials(deviceURI), rErr)
		}
		if !endpoint.PathVerified || strings.TrimSpace(endpoint.Path) == "" {
			return "", fmt.Errorf("DNS-SD resolved host %s:%d without a verified IPP resource path", endpoint.Host, endpoint.Port)
		}
		if endpoint.ExactURI != "" {
			return endpoint.ExactURI, nil
		}
		scheme := endpoint.Scheme
		if scheme == "" {
			scheme = "ipp"
		}
		return fmt.Sprintf("%s://%s:%d%s", scheme, endpoint.Host, endpoint.Port, endpoint.Path), nil

	default:
		return "", fmt.Errorf("CUPS device URI %q is not an IPP or DNS-SD endpoint", stripURLCredentials(deviceURI))
	}
}

func queryIPPEndpointState(ctx context.Context, runner CommandRunner, source, queue, ippURI string) (IPPPrinterStateResult, error) {
	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		ctx = context.Background()
	}

	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, runnerErr := runner(queryCtx, "ipptool", "-tv", ippURI, "get-printer-attributes.test")
	res := ParseIPPPrinterStateWithSource(out, source, queue, ippURI)

	hasIPPResponse := res.StatusCode != "" || res.HasRecognizedStateAttributes()
	if hasIPPResponse {
		res.EndpointReachable = true
	}

	targetDesc := fmt.Sprintf("CUPS queue %q", queue)
	if source == CapabilitySourceNativeIPP {
		targetDesc = fmt.Sprintf("native IPP endpoint %q", stripURLCredentials(ippURI))
	}

	if isIPPFailureStatus(res.StatusCode) {
		res.QueueAccessible = false
		res.Error = fmt.Sprintf("IPP state query returned failure status %q for %s", res.StatusCode, targetDesc)
		return res, errors.New(res.Error)
	}

	if runnerErr != nil {
		if !hasIPPResponse {
			res.EndpointReachable = false
		}
		res.QueueAccessible = false
		res.Error = runnerErr.Error()
		return res, fmt.Errorf("ipptool state query failed for %s: %w", targetDesc, runnerErr)
	}

	if !res.HasRecognizedStateAttributes() {
		res.EndpointReachable = false
		res.QueueAccessible = false
		res.Error = "no recognizable printer state attributes found in IPP response"
		return res, fmt.Errorf("no recognizable printer state attributes found in IPP response for %s", targetDesc)
	}

	res.EndpointReachable = true
	res.QueueAccessible = true
	return res, nil
}
