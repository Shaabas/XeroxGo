package printer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Provenance labels identifying the origin of discovered data
const (
	SourceCUPSQueue    = "CUPS_QUEUE"
	SourceRemoteIPP    = "REMOTE_IPP"
	SourceSNMPDynamic  = "SNMP_DYNAMIC"
	SourceSNMPFallback = "SNMP_GET_FALLBACK"
)

// TelemetryRating rates the feasibility of verifying physical page delivery.
type TelemetryRating string

const (
	TelemetryRatingOdometerAndIPPSheetsAvailable TelemetryRating = "ODOMETER_AND_IPP_SHEETS_AVAILABLE_UNVERIFIED_EJECTION"
	TelemetryRatingOdometerAvailable            TelemetryRating = "ODOMETER_AVAILABLE_UNVERIFIED_EJECTION"
	TelemetryRatingQueueOnly                    TelemetryRating = "QUEUE_ONLY_UNCERTAIN_EJECTION"
	TelemetryRatingAbsent                       TelemetryRating = "ABSENT"

	// Deprecated aliases maintained for backward compatibility; read-only capability discovery
	// never establishes physical ejection correctness or post-jam recovery.
	TelemetryRatingVerifiedFull         = TelemetryRatingOdometerAndIPPSheetsAvailable
	TelemetryRatingVerifiedOdometer     = TelemetryRatingOdometerAvailable
	TelemetryRatingVerifiedOdometerOnly = TelemetryRatingOdometerAvailable
)

// TargetConfig specifies the endpoints and queues targeted for fingerprinting.
type TargetConfig struct {
	QueueName      string `json:"queue_name,omitempty"`
	QueueDeviceURI string `json:"queue_device_uri,omitempty"`
	DNSSDService   string `json:"dnssd_service,omitempty"`
	DNSSDResolved  bool   `json:"dnssd_resolved"`
	RemoteIPPURI   string `json:"remote_ipp_uri,omitempty"`
	SNMPHost       string `json:"snmp_host,omitempty"`
	SNMPPort       int    `json:"snmp_port,omitempty"`
	SNMPCommunity  string `json:"snmp_community,omitempty"`
}

// IPPMarkerSupply represents marker level information parsed from IPP.
type IPPMarkerSupply struct {
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Color     string `json:"color,omitempty"`
	Level     int    `json:"level"`
	HighLevel int    `json:"high_level,omitempty"`
	LowLevel  int    `json:"low_level,omitempty"`
}

// JobProgressIPP reports IPP job tracking attribute capabilities.
type JobProgressIPP struct {
	JobImpressionsCompleted CapabilityStatus `json:"job_impressions_completed"`
	JobMediaSheetsCompleted CapabilityStatus `json:"job_media_sheets_completed"`
	JobImpressions          CapabilityStatus `json:"job_impressions"`
	JobMediaSheets          CapabilityStatus `json:"job_media_sheets"`
	JobPagesCompleted       CapabilityStatus `json:"job_pages_completed"`
}

// QueueProbeResult contains findings specifically from the local CUPS queue.
type QueueProbeResult struct {
	QueueName      string              `json:"queue_name"`
	DeviceURI      string              `json:"device_uri"`
	QueueStatus    string              `json:"queue_status,omitempty"`
	AcceptingJobs  AcceptingJobsStatus `json:"accepting_jobs"`
	PrinterState   IPPPrinterState     `json:"printer_state"`
	StateReasons   []IPPPrinterReason  `json:"state_reasons,omitempty"`
	Capabilities   PrinterCapabilities `json:"capabilities"`
	ProbeError     string              `json:"probe_error,omitempty"`
}

// IPPProbeResult contains findings directly from the remote IPP endpoint.
type IPPProbeResult struct {
	TargetURI           string              `json:"target_uri"`
	StatusCode          string              `json:"status_code,omitempty"`
	PrinterName         string              `json:"printer_name,omitempty"`
	PrinterMakeAndModel string              `json:"printer_make_and_model,omitempty"`
	PrinterInfo         string              `json:"printer_info,omitempty"`
	PrinterState        IPPPrinterState     `json:"printer_state"`
	StateReasons        []IPPPrinterReason  `json:"state_reasons,omitempty"`
	AcceptingJobs       AcceptingJobsStatus `json:"accepting_jobs"`
	DocumentFormats     []string            `json:"document_formats_supported,omitempty"`
	MediaSupported      []string            `json:"media_supported,omitempty"`
	SidesSupported      []string            `json:"sides_supported,omitempty"`
	PrintColorModes     []string            `json:"print_color_modes_supported,omitempty"`
	PDFSupport          CapabilityStatus    `json:"pdf_support"`
	PWGRasterSupport    CapabilityStatus    `json:"pwg_raster_support"`
	URFSupport          CapabilityStatus    `json:"urf_support"`
	PCLmSupport         CapabilityStatus    `json:"pclm_support"`
	A4Support           CapabilityStatus    `json:"a4_support"`
	ColorSupport        CapabilityStatus    `json:"color_support"`
	MonochromeSupport   CapabilityStatus    `json:"monochrome_support"`
	SimplexSupport      CapabilityStatus    `json:"simplex_support"`
	DuplexLongEdge      CapabilityStatus    `json:"duplex_long_edge_support"`
	DuplexShortEdge     CapabilityStatus    `json:"duplex_short_edge_support"`
	JobProgressSupport  JobProgressIPP      `json:"job_progress_support"`
	Markers             []IPPMarkerSupply   `json:"markers,omitempty"`
	Alerts              []string            `json:"alerts,omitempty"`
	RawAttributes       map[string][]string `json:"raw_attributes,omitempty"`
	ProbeError          string              `json:"probe_error,omitempty"`
}

// FeatureComparison compares a capability between CUPS queue and Remote IPP.
type FeatureComparison struct {
	Feature     string           `json:"feature"`
	CUPSQueue   CapabilityStatus `json:"cups_queue"`
	RemoteIPP   CapabilityStatus `json:"remote_ipp"`
	Consensus   CapabilityStatus `json:"consensus"`
	Discrepancy bool             `json:"discrepancy"`
	Notes       string           `json:"notes,omitempty"`
}

// TelemetrySummary rates physical job delivery feasibility.
type TelemetrySummary struct {
	Rating                    TelemetryRating `json:"rating"`
	HasSNMPMarkerCounter      bool            `json:"has_snmp_marker_counter"`
	SNMPMarkerIndex           string          `json:"snmp_marker_index,omitempty"`
	SNMPMarkerUnitValid       bool            `json:"snmp_marker_unit_valid"`
	SNMPMarkerCounterUnit     string          `json:"snmp_marker_counter_unit,omitempty"`
	SNMPMarkerLifeCount       int64           `json:"snmp_marker_life_count,omitempty"`
	IPPJobProgressSupported   bool            `json:"ipp_job_progress_supported"`
	IPPMediaSheetsSupported   bool            `json:"ipp_media_sheets_supported"`
	PhysicalEjectionVerified  bool            `json:"physical_ejection_verified"`
	SafeForPhysicalCompletion bool            `json:"safe_for_physical_completion"`
	Warnings                  []string        `json:"warnings,omitempty"`
}

// FingerprintReport is the top-level report encompassing all probes.
type FingerprintReport struct {
	Timestamp   time.Time           `json:"timestamp"`
	Redacted    bool                `json:"redacted"`
	Targets     TargetConfig        `json:"targets"`
	CUPSQueue   *QueueProbeResult   `json:"cups_queue,omitempty"`
	RemoteIPP   *IPPProbeResult     `json:"remote_ipp,omitempty"`
	SNMP        *SNMPProbeResult    `json:"snmp,omitempty"`
	Comparisons []FeatureComparison `json:"comparisons,omitempty"`
	Telemetry   TelemetrySummary    `json:"telemetry"`
	RawEvidence map[string]any      `json:"raw_evidence,omitempty"`
}

// FingerprintConfig holds the input parameters for executing a capability fingerprint.
type FingerprintConfig struct {
	QueueName     string
	RemoteIPPURI  string
	SNMPHost      string
	SNMPPort      int
	SNMPCommunity string
	SNMPTimeout   time.Duration
	SNMPRetries   int
	Redact        bool
}

// RunFingerprint executes read-only capability probing across CUPS, Remote IPP, and SNMP.
// It enforces all safety constraints: no print submissions, no SNMP SET, and best-effort resolution.
func RunFingerprint(ctx context.Context, runner CommandRunner, snmpSessionFactory func(cfg SNMPConfig) (DynamicSNMPSession, error), cfg FingerprintConfig) (*FingerprintReport, error) {
	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		ctx = context.Background()
	}

	commReport := ""
	if cfg.SNMPCommunity != "" {
		commReport = "[REDACTED_COMMUNITY]"
	}

	report := &FingerprintReport{
		Timestamp:   time.Now().UTC(),
		Redacted:    cfg.Redact,
		RawEvidence: make(map[string]any),
		Targets: TargetConfig{
			QueueName:     cfg.QueueName,
			RemoteIPPURI:  stripURLCredentials(cfg.RemoteIPPURI),
			SNMPHost:      cfg.SNMPHost,
			SNMPPort:      cfg.SNMPPort,
			SNMPCommunity: commReport, // NEVER serialize plain community credentials even when redact=false
		},
	}

	// 1. Probe local CUPS queue if queue name is provided
	var cupsDeviceURI string
	if strings.TrimSpace(cfg.QueueName) != "" {
		qRes := probeCUPSQueue(ctx, runner, cfg.QueueName)
		report.CUPSQueue = qRes
		cupsDeviceURI = qRes.DeviceURI
		report.Targets.QueueDeviceURI = stripURLCredentials(cupsDeviceURI)

		if len(qRes.Capabilities.RawAttributes) > 0 {
			report.RawEvidence["cups_raw_attributes"] = qRes.Capabilities.RawAttributes
		}
	}

	// 2. Determine Remote IPP Target URI
	remoteIPPURI := strings.TrimSpace(cfg.RemoteIPPURI)
	var dnssdResolvedHost string
	if remoteIPPURI == "" && cupsDeviceURI != "" {
		if strings.HasPrefix(strings.ToLower(cupsDeviceURI), "ipp://") ||
			strings.HasPrefix(strings.ToLower(cupsDeviceURI), "ipps://") ||
			strings.HasPrefix(strings.ToLower(cupsDeviceURI), "http://") ||
			strings.HasPrefix(strings.ToLower(cupsDeviceURI), "https://") {
			remoteIPPURI = cupsDeviceURI
		} else if strings.HasPrefix(strings.ToLower(cupsDeviceURI), "dnssd://") {
			dnssdInfo, err := ParseDNSSDURI(cupsDeviceURI)
			if err == nil && dnssdInfo != nil {
				report.Targets.DNSSDService = dnssdInfo.ServiceName
			}
			endpoint, rErr := ResolveDNSSDTarget(ctx, runner, cupsDeviceURI)
			if rErr == nil && endpoint.Host != "" {
				report.Targets.DNSSDResolved = true
				dnssdResolvedHost = endpoint.Host
				if endpoint.PathVerified && endpoint.Path != "" {
					if endpoint.ExactURI != "" {
						remoteIPPURI = endpoint.ExactURI
					} else {
						remoteIPPURI = fmt.Sprintf("%s://%s:%d%s", endpoint.Scheme, endpoint.Host, endpoint.Port, endpoint.Path)
					}
				} else {
					// DNS-SD hostname resolution must not imply a verified IPP endpoint.
					// Verify the actual service URI/path or report remote IPP as UNKNOWN
					// and request an explicit override.
					remoteIPPURI = ""
					report.RemoteIPP = newUnverifiedIPPProbeResult(
						stripURLCredentials(cupsDeviceURI),
						fmt.Sprintf("DNS-SD resolved host %s:%d, but actual IPP service URI/path is unverified. Remote IPP reported as UNKNOWN. Provide explicit --ipp-uri override (e.g. '%s://%s:%d/ipp/print')", endpoint.Host, endpoint.Port, endpoint.Scheme, endpoint.Host, endpoint.Port),
					)
				}
			} else {
				report.Targets.DNSSDResolved = false
			}
		}
	}
	report.Targets.RemoteIPPURI = stripURLCredentials(remoteIPPURI)

	// 3. Probe Remote IPP Endpoint separately
	if remoteIPPURI != "" {
		ippRes := probeRemoteIPP(ctx, runner, remoteIPPURI)
		report.RemoteIPP = ippRes
		if len(ippRes.RawAttributes) > 0 {
			report.RawEvidence["remote_ipp_raw_attributes"] = ippRes.RawAttributes
		}
	} else if report.RemoteIPP == nil && strings.HasPrefix(strings.ToLower(cupsDeviceURI), "dnssd://") && !report.Targets.DNSSDResolved {
		report.RemoteIPP = newUnverifiedIPPProbeResult(
			stripURLCredentials(cupsDeviceURI),
			"DNS-SD target unresolved; explicit --ipp-uri required for direct remote probe",
		)
	}

	// 4. Determine SNMP Target Host
	snmpHost := strings.TrimSpace(cfg.SNMPHost)
	if snmpHost == "" {
		// Attempt resolution from remote IPP URI, DNS-SD resolved host, or CUPS device URI
		if remoteIPPURI != "" {
			if u, err := url.Parse(remoteIPPURI); err == nil && u.Hostname() != "" {
				snmpHost = u.Hostname()
			}
		} else if dnssdResolvedHost != "" {
			snmpHost = dnssdResolvedHost
		} else if cupsDeviceURI != "" {
			snmpHost = ExtractHostFromCUPSDeviceURI(cupsDeviceURI)
		}
	}
	report.Targets.SNMPHost = snmpHost
	snmpPort := cfg.SNMPPort
	if snmpPort <= 0 {
		snmpPort = 161
	}
	report.Targets.SNMPPort = snmpPort

	// 5. Probe Dynamic SNMP
	if snmpHost != "" && !IsLoopbackHost(snmpHost) || snmpHost != "" {
		snmpCfg := SNMPConfig{
			Target:    snmpHost,
			Port:      snmpPort,
			Community: cfg.SNMPCommunity,
			Timeout:   cfg.SNMPTimeout,
			Retries:   cfg.SNMPRetries,
		}
		if snmpCfg.Community == "" {
			snmpCfg.Community = "public"
		}

		if snmpSessionFactory != nil {
			session, sErr := snmpSessionFactory(snmpCfg)
			if sErr != nil {
				report.SNMP = &SNMPProbeResult{
					TargetHost:    snmpHost,
					TargetPort:    snmpPort,
					DiscoveryMode: "SNMP_FAILED",
					ProbeError:    fmt.Sprintf("SNMP session creation failed: %v", sErr),
				}
			} else {
				defer session.Close()
				snmpRes, dErr := DiscoverSNMPDynamic(ctx, session, snmpHost, snmpPort)
				if dErr != nil && snmpRes == nil {
					report.SNMP = &SNMPProbeResult{
						TargetHost:    snmpHost,
						TargetPort:    snmpPort,
						DiscoveryMode: "SNMP_FAILED",
						ProbeError:    dErr.Error(),
					}
				} else {
					report.SNMP = snmpRes
				}
			}
		}
	}

	// 6. Build Feature Comparisons between CUPS Queue and Remote IPP
	report.Comparisons = buildComparisons(report.CUPSQueue, report.RemoteIPP)

	// 7. Evaluate Telemetry & Physical Page Verification Feasibility
	report.Telemetry = evaluateTelemetry(report.CUPSQueue, report.RemoteIPP, report.SNMP)

	// 8. Sanitize report if redaction is requested
	if cfg.Redact {
		SanitizeReport(report)
	}

	return report, nil
}

func probeCUPSQueue(ctx context.Context, runner CommandRunner, queue string) *QueueProbeResult {
	res := &QueueProbeResult{
		QueueName:     queue,
		AcceptingJobs: AcceptingJobsUnknown,
		PrinterState:  IPPPrinterStateUnknown,
	}

	// Discover device URI via lpstat -v
	if out, err := runner(ctx, "lpstat", "-v", queue); err == nil {
		line := strings.TrimSpace(string(out))
		if idx := strings.Index(line, ":"); idx != -1 {
			res.DeviceURI = stripURLCredentials(strings.TrimSpace(line[idx+1:]))
		} else {
			res.DeviceURI = stripURLCredentials(line)
		}
	}

	// Discover queue status via lpstat -p
	if out, err := runner(ctx, "lpstat", "-p", queue); err == nil {
		res.QueueStatus = strings.TrimSpace(string(out))
	}

	// Query capabilities via ipptool on local CUPS printer endpoint
	caps, capsErr := DiscoverCUPSPrinterCapabilities(ctx, runner, queue)
	res.Capabilities = caps
	if capsErr != nil {
		res.ProbeError = capsErr.Error()
	}

	// Also parse operational state
	stateRes, _ := QueryIPPPrinterState(ctx, runner, queue)
	res.AcceptingJobs = stateRes.AcceptingJobs
	res.PrinterState = stateRes.State
	res.StateReasons = stateRes.ParsedReasons

	return res
}

func newUnverifiedIPPProbeResult(targetURI, probeError string) *IPPProbeResult {
	return &IPPProbeResult{
		TargetURI:         stripURLCredentials(targetURI),
		AcceptingJobs:     AcceptingJobsUnknown,
		PrinterState:      IPPPrinterStateUnknown,
		PDFSupport:        CapabilityUnknown,
		PWGRasterSupport:  CapabilityUnknown,
		URFSupport:        CapabilityUnknown,
		PCLmSupport:       CapabilityUnknown,
		A4Support:         CapabilityUnknown,
		ColorSupport:      CapabilityUnknown,
		MonochromeSupport: CapabilityUnknown,
		SimplexSupport:    CapabilityUnknown,
		DuplexLongEdge:    CapabilityUnknown,
		DuplexShortEdge:   CapabilityUnknown,
		JobProgressSupport: JobProgressIPP{
			JobImpressionsCompleted: CapabilityUnknown,
			JobMediaSheetsCompleted: CapabilityUnknown,
			JobImpressions:          CapabilityUnknown,
			JobMediaSheets:          CapabilityUnknown,
			JobPagesCompleted:       CapabilityUnknown,
		},
		ProbeError: probeError,
	}
}

func probeRemoteIPP(ctx context.Context, runner CommandRunner, remoteURI string) *IPPProbeResult {
	res := &IPPProbeResult{
		TargetURI:          stripURLCredentials(remoteURI),
		AcceptingJobs:      AcceptingJobsUnknown,
		PrinterState:       IPPPrinterStateUnknown,
		PDFSupport:         CapabilityUnknown,
		PWGRasterSupport:   CapabilityUnknown,
		URFSupport:         CapabilityUnknown,
		PCLmSupport:        CapabilityUnknown,
		A4Support:          CapabilityUnknown,
		ColorSupport:       CapabilityUnknown,
		MonochromeSupport:  CapabilityUnknown,
		SimplexSupport:     CapabilityUnknown,
		DuplexLongEdge:     CapabilityUnknown,
		DuplexShortEdge:    CapabilityUnknown,
		JobProgressSupport: JobProgressIPP{
			JobImpressionsCompleted: CapabilityUnknown,
			JobMediaSheetsCompleted: CapabilityUnknown,
			JobImpressions:          CapabilityUnknown,
			JobMediaSheets:          CapabilityUnknown,
			JobPagesCompleted:       CapabilityUnknown,
		},
		RawAttributes: make(map[string][]string),
	}

	out, err := runner(ctx, "ipptool", "-tv", remoteURI, "get-printer-attributes.test")
	if err != nil {
		res.ProbeError = fmt.Sprintf("ipptool query failed for remote IPP URI: %v", err)
	}

	// Parse raw attributes
	singleAttrs := make(map[string]string)
	var rawReasons []string

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		m := ippAttributeRegex.FindStringSubmatch(line)
		if len(m) < 3 {
			continue
		}
		attrName := strings.ToLower(strings.TrimSpace(m[1]))
		val := strings.TrimSpace(m[2])

		if _, exists := singleAttrs[attrName]; !exists {
			singleAttrs[attrName] = val
		}
		parts := splitCleanValues(val)
		if len(parts) > 0 {
			res.RawAttributes[attrName] = append(res.RawAttributes[attrName], parts...)
		}
		if attrName == "printer-state-reasons" {
			for _, p := range parts {
				if !strings.EqualFold(p, "none") && p != "" {
					rawReasons = append(rawReasons, p)
				}
			}
		}
	}

	// Capture identity and status metadata if available in output
	res.StatusCode = strings.Trim(strings.TrimSpace(singleAttrs["status-code"]), `"`)
	if v, ok := singleAttrs["printer-name"]; ok {
		res.PrinterName = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if v, ok := singleAttrs["printer-make-and-model"]; ok {
		res.PrinterMakeAndModel = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if v, ok := singleAttrs["printer-info"]; ok {
		res.PrinterInfo = strings.Trim(strings.TrimSpace(v), `"`)
	}

	// When ipptool execution fails (err != nil), do not derive supported/unsupported capabilities from partial output.
	// Failure details and sanitized raw evidence are preserved, but all unverified capabilities remain UNKNOWN.
	if err != nil {
		return res
	}

	scLower := strings.ToLower(res.StatusCode)
	if strings.HasPrefix(scLower, "client-error") || strings.HasPrefix(scLower, "server-error") {
		if res.ProbeError == "" {
			res.ProbeError = fmt.Sprintf("IPP server returned error status: %s", res.StatusCode)
		}
		// Missing or failed IPP observations must never yield unsupported or verified progress claims
		return res
	}
	if len(singleAttrs) == 0 && res.ProbeError != "" {
		return res
	}

	// State & reasons
	if v, ok := singleAttrs["printer-state"]; ok {
		res.PrinterState, _ = NormalizeIPPPrinterState(v)
	}
	if v, ok := singleAttrs["printer-is-accepting-jobs"]; ok {
		res.AcceptingJobs = NormalizeAcceptingJobs(v)
	}
	for _, r := range rawReasons {
		if parsed, ok := parseIPPReason(r); ok {
			res.StateReasons = append(res.StateReasons, parsed)
		}
	}

	// Formats & Media
	res.DocumentFormats = res.RawAttributes["document-format-supported"]
	res.MediaSupported = res.RawAttributes["media-supported"]
	res.SidesSupported = res.RawAttributes["sides-supported"]
	res.PrintColorModes = res.RawAttributes["print-color-mode-supported"]

	// Normalize formats
	if len(res.DocumentFormats) > 0 {
		if containsCaseInsensitive(res.DocumentFormats, "application/pdf") {
			res.PDFSupport = CapabilitySupported
		} else {
			res.PDFSupport = CapabilityUnsupported
		}
		if containsCaseInsensitive(res.DocumentFormats, "image/pwg-raster") {
			res.PWGRasterSupport = CapabilitySupported
		} else {
			res.PWGRasterSupport = CapabilityUnsupported
		}
		if containsCaseInsensitive(res.DocumentFormats, "image/urf") {
			res.URFSupport = CapabilitySupported
		} else {
			res.URFSupport = CapabilityUnsupported
		}
		if containsAnyCaseInsensitive(res.DocumentFormats, "application/pclm", "application/vnd.hp-pcl", "application/vnd.hp-pclms") {
			res.PCLmSupport = CapabilitySupported
		} else {
			res.PCLmSupport = CapabilityUnsupported
		}
	}

	// Normalize media
	if len(res.MediaSupported) > 0 {
		if hasA4Media(res.MediaSupported) {
			res.A4Support = CapabilitySupported
		} else {
			res.A4Support = CapabilityUnsupported
		}
	}

	// Normalize sides / duplex
	if len(res.SidesSupported) > 0 {
		if containsCaseInsensitive(res.SidesSupported, "one-sided") || containsCaseInsensitive(res.SidesSupported, "simplex") {
			res.SimplexSupport = CapabilitySupported
		} else {
			res.SimplexSupport = CapabilityUnsupported
		}
		if containsCaseInsensitive(res.SidesSupported, "two-sided-long-edge") || containsCaseInsensitive(res.SidesSupported, "duplex") {
			res.DuplexLongEdge = CapabilitySupported
		} else {
			res.DuplexLongEdge = CapabilityUnsupported
		}
		if containsCaseInsensitive(res.SidesSupported, "two-sided-short-edge") || containsCaseInsensitive(res.SidesSupported, "tumble") {
			res.DuplexShortEdge = CapabilitySupported
		} else {
			res.DuplexShortEdge = CapabilityUnsupported
		}
	}

	// Normalize color / monochrome
	colorVal := strings.ToLower(strings.TrimSpace(singleAttrs["color-supported"]))
	if colorVal == "false" {
		res.ColorSupport = CapabilityUnsupported
		res.MonochromeSupport = CapabilitySupported
	} else if colorVal == "true" {
		res.ColorSupport = CapabilitySupported
		res.MonochromeSupport = CapabilitySupported
	} else if len(res.PrintColorModes) > 0 {
		if containsCaseInsensitive(res.PrintColorModes, "color") {
			res.ColorSupport = CapabilitySupported
		} else {
			res.ColorSupport = CapabilityUnsupported
		}
		if containsAnyCaseInsensitive(res.PrintColorModes, "monochrome", "process-monochrome", "auto", "bi-level", "monochrome-only") {
			res.MonochromeSupport = CapabilitySupported
		} else {
			res.MonochromeSupport = CapabilityUnsupported
		}
	}

	// Job Progress Attribute Support
	// Check printer-attributes-supported or job-creation-attributes-supported
	var supportedJobAttrs []string
	if attrs, ok := res.RawAttributes["job-creation-attributes-supported"]; ok {
		supportedJobAttrs = append(supportedJobAttrs, attrs...)
	}
	if attrs, ok := res.RawAttributes["printer-attributes-supported"]; ok {
		supportedJobAttrs = append(supportedJobAttrs, attrs...)
	}
	if attrs, ok := res.RawAttributes["job-settable-attributes-supported"]; ok {
		supportedJobAttrs = append(supportedJobAttrs, attrs...)
	}

	// Missing entries in attribute lists do NOT imply UNSUPPORTED.
	// IPP Everywhere requires Get-Job-Attributes to report progress, but printers do not
	// list job-impressions-completed in job-creation-attributes-supported.
	// Only set CapabilitySupported on positive evidence; leave as CapabilityUnknown otherwise.
	if containsCaseInsensitive(supportedJobAttrs, "job-impressions-completed") ||
		containsCaseInsensitive(res.RawAttributes["job-impressions-completed-supported"], "true") {
		res.JobProgressSupport.JobImpressionsCompleted = CapabilitySupported
	}
	if containsCaseInsensitive(supportedJobAttrs, "job-media-sheets-completed") ||
		containsCaseInsensitive(res.RawAttributes["job-media-sheets-completed-supported"], "true") {
		res.JobProgressSupport.JobMediaSheetsCompleted = CapabilitySupported
	}
	if containsCaseInsensitive(supportedJobAttrs, "job-impressions") ||
		containsCaseInsensitive(res.RawAttributes["job-impressions-supported"], "true") {
		res.JobProgressSupport.JobImpressions = CapabilitySupported
	}
	if containsCaseInsensitive(supportedJobAttrs, "job-media-sheets") ||
		containsCaseInsensitive(res.RawAttributes["job-media-sheets-supported"], "true") {
		res.JobProgressSupport.JobMediaSheets = CapabilitySupported
	}
	if containsCaseInsensitive(supportedJobAttrs, "job-pages-completed") ||
		containsCaseInsensitive(res.RawAttributes["job-pages-completed-supported"], "true") {
		res.JobProgressSupport.JobPagesCompleted = CapabilitySupported
	}

	// Markers / Supplies from IPP
	names := res.RawAttributes["marker-names"]
	types := res.RawAttributes["marker-types"]
	colors := res.RawAttributes["marker-colors"]
	levels := res.RawAttributes["marker-levels"]
	highs := res.RawAttributes["marker-high-levels"]
	lows := res.RawAttributes["marker-low-levels"]

	for i, name := range names {
		marker := IPPMarkerSupply{Name: name}
		if i < len(types) {
			marker.Type = types[i]
		}
		if i < len(colors) {
			marker.Color = colors[i]
		}
		if i < len(levels) {
			var l int
			if _, err := fmt.Sscanf(levels[i], "%d", &l); err == nil {
				marker.Level = l
			}
		}
		if i < len(highs) {
			var h int
			if _, err := fmt.Sscanf(highs[i], "%d", &h); err == nil {
				marker.HighLevel = h
			}
		}
		if i < len(lows) {
			var lw int
			if _, err := fmt.Sscanf(lows[i], "%d", &lw); err == nil {
				marker.LowLevel = lw
			}
		}
		res.Markers = append(res.Markers, marker)
	}

	// Alerts from IPP
	if alerts, ok := res.RawAttributes["printer-alert"]; ok {
		res.Alerts = append(res.Alerts, alerts...)
	}

	return res
}

func buildComparisons(cups *QueueProbeResult, remote *IPPProbeResult) []FeatureComparison {
	var comps []FeatureComparison

	type pair struct {
		name      string
		cupsVal   CapabilityStatus
		remoteVal CapabilityStatus
		note      string
	}

	cupsPDF, cupsA4, cupsColor, cupsMono := CapabilityUnknown, CapabilityUnknown, CapabilityUnknown, CapabilityUnknown
	cupsSimplex, cupsDuplexLong, cupsDuplexShort := CapabilityUnknown, CapabilityUnknown, CapabilityUnknown

	if cups != nil {
		cupsPDF = cups.Capabilities.PDFSupport
		cupsA4 = cups.Capabilities.A4Support
		cupsColor = cups.Capabilities.ColorSupport
		cupsMono = cups.Capabilities.MonochromeSupport
		cupsSimplex = cups.Capabilities.SimplexSupport
		cupsDuplexLong = cups.Capabilities.DuplexLongEdgeSupport
		cupsDuplexShort = cups.Capabilities.DuplexShortEdgeSupport
	}

	remotePDF, remoteA4, remoteColor, remoteMono := CapabilityUnknown, CapabilityUnknown, CapabilityUnknown, CapabilityUnknown
	remoteSimplex, remoteDuplexLong, remoteDuplexShort := CapabilityUnknown, CapabilityUnknown, CapabilityUnknown

	if remote != nil {
		remotePDF = remote.PDFSupport
		remoteA4 = remote.A4Support
		remoteColor = remote.ColorSupport
		remoteMono = remote.MonochromeSupport
		remoteSimplex = remote.SimplexSupport
		remoteDuplexLong = remote.DuplexLongEdge
		remoteDuplexShort = remote.DuplexShortEdge
	}

	pairs := []pair{
		{"PDF Direct Printing", cupsPDF, remotePDF, "CUPS queue handles raster conversion if remote direct PDF is unsupported"},
		{"A4 Media Support", cupsA4, remoteA4, "Standard A4 paper size support"},
		{"Color Output Support", cupsColor, remoteColor, "Color printing capability"},
		{"Monochrome Output Support", cupsMono, remoteMono, "Black and white printing capability"},
		{"Simplex (1-sided) Printing", cupsSimplex, remoteSimplex, "Single-sided print output"},
		{"Duplex (2-sided Long Edge)", cupsDuplexLong, remoteDuplexLong, "Automatic 2-sided printing along long edge; beware manual duplex"},
		{"Duplex (2-sided Short Edge)", cupsDuplexShort, remoteDuplexShort, "Automatic 2-sided printing along short edge"},
	}

	for _, p := range pairs {
		discrepancy := false
		consensus := CapabilityUnknown

		if p.cupsVal == p.remoteVal && p.cupsVal != CapabilityUnknown {
			consensus = p.cupsVal
		} else if p.cupsVal != CapabilityUnknown && p.remoteVal == CapabilityUnknown {
			consensus = p.cupsVal
		} else if p.remoteVal != CapabilityUnknown && p.cupsVal == CapabilityUnknown {
			consensus = p.remoteVal
		} else if p.cupsVal != CapabilityUnknown && p.remoteVal != CapabilityUnknown && p.cupsVal != p.remoteVal {
			discrepancy = true
			consensus = CapabilityUnknown
		}

		note := p.note
		if discrepancy {
			if strings.Contains(p.name, "Duplex") {
				note = "DISCREPANCY: CUPS driver configuration contradicts remote IPP capability. Do not assume automatic duplex!"
			} else {
				note = fmt.Sprintf("DISCREPANCY: CUPS (%s) contradicts Remote IPP (%s)", p.cupsVal, p.remoteVal)
			}
		}

		comps = append(comps, FeatureComparison{
			Feature:     p.name,
			CUPSQueue:   p.cupsVal,
			RemoteIPP:   p.remoteVal,
			Consensus:   consensus,
			Discrepancy: discrepancy,
			Notes:       note,
		})
	}

	return comps
}

func evaluateTelemetry(cups *QueueProbeResult, remote *IPPProbeResult, snmp *SNMPProbeResult) TelemetrySummary {
	summary := TelemetrySummary{
		Rating:                    TelemetryRatingAbsent,
		PhysicalEjectionVerified:  false, // Read-only capability discovery cannot establish actual ejection correctness
		SafeForPhysicalCompletion: false, // Read-only discovery cannot prove safe completion or verify post-jam recovery
	}

	// 1. Check SNMP Marker Odometer and Counter Unit
	if snmp != nil && len(snmp.Markers) > 0 {
		// Sort markers deterministically by row index
		sortedMarkers := make([]SNMPMarker, len(snmp.Markers))
		copy(sortedMarkers, snmp.Markers)
		sort.Slice(sortedMarkers, func(i, j int) bool {
			return sortedMarkers[i].Index < sortedMarkers[j].Index
		})

		var selectedMarker *SNMPMarker
		// Prioritize deterministic valid page counter with verified lifetime count
		for i := range sortedMarkers {
			m := &sortedMarkers[i]
			if m.HasLifeCount && m.LifeCount >= 0 && m.IsPageCounter {
				selectedMarker = m
				break
			}
		}
		// If no valid page counter found, fall back to first counter with verified lifetime count for diagnostics
		if selectedMarker == nil {
			for i := range sortedMarkers {
				m := &sortedMarkers[i]
				if m.HasLifeCount && m.LifeCount >= 0 {
					selectedMarker = m
					break
				}
			}
		}

		if selectedMarker != nil {
			summary.HasSNMPMarkerCounter = true
			summary.SNMPMarkerIndex = selectedMarker.Index
			summary.SNMPMarkerLifeCount = selectedMarker.LifeCount
			summary.SNMPMarkerCounterUnit = selectedMarker.CounterUnitName
			summary.SNMPMarkerUnitValid = selectedMarker.IsPageCounter
		}
	}

	// 2. Check Remote IPP Job Progress Attributes
	if remote != nil {
		if remote.JobProgressSupport.JobImpressionsCompleted == CapabilitySupported {
			summary.IPPJobProgressSupported = true
		}
		if remote.JobProgressSupport.JobMediaSheetsCompleted == CapabilitySupported {
			summary.IPPMediaSheetsSupported = true
		}
	}

	// 3. Rate Telemetry Feasibility using explicit available/unverified terminology
	if summary.IPPMediaSheetsSupported && summary.SNMPMarkerUnitValid {
		summary.Rating = TelemetryRatingOdometerAndIPPSheetsAvailable
		summary.Warnings = append(summary.Warnings, "SNMP marker counter and IPP media sheets are available for active polling; read-only capability discovery cannot establish actual ejection correctness or verify post-jam recovery.")
	} else if summary.SNMPMarkerUnitValid {
		summary.Rating = TelemetryRatingOdometerAvailable
		summary.Warnings = append(summary.Warnings, "SNMP marker counter odometer is available for polling, but physical page ejection cannot be verified by read-only discovery; IPP job-media-sheets-completed is unavailable.")
	} else if cups != nil && cups.QueueStatus != "" {
		summary.Rating = TelemetryRatingQueueOnly
		summary.Warnings = append(summary.Warnings, "CUPS queue completed status does NOT verify physical page delivery; marker counter is absent or has unvalidated units.")
	} else {
		summary.Rating = TelemetryRatingAbsent
		summary.Warnings = append(summary.Warnings, "No telemetry or printer monitoring channels are available.")
	}

	if summary.HasSNMPMarkerCounter && !summary.SNMPMarkerUnitValid {
		summary.Warnings = append(summary.Warnings, fmt.Sprintf("SNMP marker counter unit is %q (not sheets/impressions). Cannot be used as physical page counter.", summary.SNMPMarkerCounterUnit))
	}

	return summary
}

// SanitizeReport redacts IP addresses, serial numbers, MAC addresses, and community strings from reports.
func SanitizeReport(rep *FingerprintReport) {
	if rep == nil {
		return
	}

	// Community string is NEVER exposed
	if rep.Targets.SNMPCommunity != "" {
		rep.Targets.SNMPCommunity = "[REDACTED_COMMUNITY]"
	}

	// Redact targets IP/Host
	rep.Targets.QueueName = sanitizeString(rep.Targets.QueueName)
	rep.Targets.DNSSDService = sanitizeString(rep.Targets.DNSSDService)
	rep.Targets.SNMPHost = sanitizeHostOrIP(rep.Targets.SNMPHost)
	rep.Targets.RemoteIPPURI = sanitizeURI(rep.Targets.RemoteIPPURI)
	rep.Targets.QueueDeviceURI = sanitizeURI(rep.Targets.QueueDeviceURI)

	// Redact Remote IPP
	if rep.RemoteIPP != nil {
		rep.RemoteIPP.TargetURI = sanitizeURI(rep.RemoteIPP.TargetURI)
		rep.RemoteIPP.PrinterName = sanitizeString(rep.RemoteIPP.PrinterName)
		rep.RemoteIPP.PrinterMakeAndModel = sanitizeString(rep.RemoteIPP.PrinterMakeAndModel)
		rep.RemoteIPP.PrinterInfo = sanitizeString(rep.RemoteIPP.PrinterInfo)
		rep.RemoteIPP.StatusCode = sanitizeString(rep.RemoteIPP.StatusCode)
		rep.RemoteIPP.ProbeError = sanitizeString(rep.RemoteIPP.ProbeError)
		for i := range rep.RemoteIPP.Alerts {
			rep.RemoteIPP.Alerts[i] = sanitizeString(rep.RemoteIPP.Alerts[i])
		}
		for k, vals := range rep.RemoteIPP.RawAttributes {
			for i, v := range vals {
				rep.RemoteIPP.RawAttributes[k][i] = sanitizeString(v)
			}
		}
	}

	// Redact CUPS Queue
	if rep.CUPSQueue != nil {
		rep.CUPSQueue.DeviceURI = sanitizeURI(rep.CUPSQueue.DeviceURI)
		rep.CUPSQueue.QueueStatus = sanitizeString(rep.CUPSQueue.QueueStatus)
		rep.CUPSQueue.ProbeError = sanitizeString(rep.CUPSQueue.ProbeError)
		rep.CUPSQueue.Capabilities.PrinterName = sanitizeString(rep.CUPSQueue.Capabilities.PrinterName)
		rep.CUPSQueue.Capabilities.PrinterMakeAndModel = sanitizeString(rep.CUPSQueue.Capabilities.PrinterMakeAndModel)
		for k, vals := range rep.CUPSQueue.Capabilities.RawAttributes {
			for i, v := range vals {
				rep.CUPSQueue.Capabilities.RawAttributes[k][i] = sanitizeString(v)
			}
		}
	}

	// Redact SNMP Result
	if rep.SNMP != nil {
		rep.SNMP.TargetHost = sanitizeHostOrIP(rep.SNMP.TargetHost)
		rep.SNMP.SysDescr = sanitizeString(rep.SNMP.SysDescr)
		rep.SNMP.PrinterName = sanitizeString(rep.SNMP.PrinterName)
		rep.SNMP.ProbeError = sanitizeString(rep.SNMP.ProbeError)
		for i := range rep.SNMP.Covers {
			rep.SNMP.Covers[i].Description = sanitizeString(rep.SNMP.Covers[i].Description)
		}
		for i := range rep.SNMP.Supplies {
			rep.SNMP.Supplies[i].Description = sanitizeString(rep.SNMP.Supplies[i].Description)
		}
		for i := range rep.SNMP.Alerts {
			rep.SNMP.Alerts[i].Description = sanitizeString(rep.SNMP.Alerts[i].Description)
		}
	}

	// Redact Telemetry Warnings
	for i := range rep.Telemetry.Warnings {
		rep.Telemetry.Warnings[i] = sanitizeString(rep.Telemetry.Warnings[i])
	}

	// Redact Raw Evidence
	if rep.RawEvidence != nil {
		sanitizeRawEvidence(rep.RawEvidence)
	}
}

var (
	macRegex         = regexp.MustCompile(`(?i)\b([0-9a-f]{2}[:-]){5}([0-9a-f]{2})\b`)
	serialRegex      = regexp.MustCompile(`(?i)\b(?:sn|serial|s/n|serialno|vnc)[:= ]*([a-z0-9\-_]{5,})\b`)
	ipv4Regex        = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b`)
	ipv6Regex        = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}\b|\b(?:[0-9a-f]{1,4}:){1,7}:[0-9a-f]{0,4}\b|\b::1\b`)
	uuidRegex        = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b|uuid=[a-zA-Z0-9\-_]+`)
	querySecretRegex = regexp.MustCompile(`(?i)([?&][^=\s]*(?:token|key|secret|password|pass|auth|api_key|serial|uuid)[^=\s]*=)[^& \t\r\n]+`)
	credentialRegex  = regexp.MustCompile(`(?i)\b(?:community|pass|passwd|password|secret|token)[:= ]*(\S+)\b`)
)

func isSensitiveQueryKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}
	if k == "token" || k == "secret" || k == "password" || k == "passwd" || k == "pass" || k == "pwd" ||
		k == "auth" || k == "key" || k == "apikey" || k == "api_key" || k == "credential" || k == "credentials" ||
		k == "signature" || k == "sig" {
		return true
	}
	parts := strings.FieldsFunc(k, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for _, p := range parts {
		if p == "token" || p == "secret" || p == "password" || p == "passwd" || p == "pass" || p == "pwd" ||
			p == "auth" || p == "key" || p == "apikey" || p == "cred" || p == "credential" || p == "credentials" {
			return true
		}
	}
	if strings.Contains(k, "token") || strings.Contains(k, "secret") || strings.Contains(k, "password") ||
		strings.Contains(k, "passwd") || strings.Contains(k, "auth") || strings.Contains(k, "apikey") ||
		strings.Contains(k, "api_key") || strings.Contains(k, "credential") {
		return true
	}
	return false
}

func stripURLCredentials(rawURI string) string {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return ""
	}
	credRegex := regexp.MustCompile(`(?i)(://)(?:[^:/@\s]+(?::[^/@\s]*)?@)`)
	rawURI = credRegex.ReplaceAllString(rawURI, "$1")

	u, err := url.Parse(rawURI)
	if err != nil {
		return rawURI
	}
	u.User = nil

	if u.RawQuery != "" {
		var keptParams []string
		pairs := strings.FieldsFunc(u.RawQuery, func(r rune) bool {
			return r == '&' || r == ';'
		})
		for _, pair := range pairs {
			if pair == "" {
				continue
			}
			parts := strings.SplitN(pair, "=", 2)
			paramKey := parts[0]
			if !isSensitiveQueryKey(paramKey) {
				keptParams = append(keptParams, pair)
			}
		}
		u.RawQuery = strings.Join(keptParams, "&")
	}

	return u.String()
}

func sanitizeString(s string) string {
	if s == "" {
		return ""
	}
	s = querySecretRegex.ReplaceAllString(s, "$1[REDACTED]")
	s = uuidRegex.ReplaceAllString(s, "[REDACTED_UUID]")
	s = macRegex.ReplaceAllString(s, "[REDACTED_MAC]")
	s = serialRegex.ReplaceAllString(s, "SN:[REDACTED_SERIAL]")
	s = ipv6Regex.ReplaceAllString(s, "[REDACTED_IPV6]")
	s = ipv4Regex.ReplaceAllStringFunc(s, func(match string) string {
		if match == "127.0.0.1" {
			return match
		}
		return "[REDACTED_IP]"
	})
	s = credentialRegex.ReplaceAllString(s, "[REDACTED_CREDENTIAL]")
	return s
}

func sanitizeHostOrIP(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if IsLoopbackHost(host) {
		return "127.0.0.1 (loopback)"
	}
	return "[REDACTED_HOST]"
}

func sanitizeURI(rawURI string) string {
	rawURI = stripURLCredentials(rawURI)
	if rawURI == "" {
		return ""
	}
	rawURI = querySecretRegex.ReplaceAllString(rawURI, "$1[REDACTED]")
	rawURI = uuidRegex.ReplaceAllString(rawURI, "[REDACTED_UUID]")
	u, err := url.Parse(rawURI)
	if err != nil || u.Host == "" {
		return sanitizeString(rawURI)
	}
	u.User = nil
	if !IsLoopbackHost(u.Hostname()) {
		port := u.Port()
		if port != "" {
			u.Host = fmt.Sprintf("[REDACTED_HOST]:%s", port)
		} else {
			u.Host = "[REDACTED_HOST]"
		}
	}
	return sanitizeString(u.String())
}

// SaveReportFile writes diagnostic report bytes with restrictive permissions (0600 file, 0700 dir).
func SaveReportFile(outPath string, data []byte) error {
	outPath = strings.TrimSpace(outPath)
	if outPath == "" {
		return errors.New("empty output path")
	}
	dir := filepath.Dir(outPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}
	f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()
	if err := os.Chmod(outPath, 0600); err != nil {
		return fmt.Errorf("failed to chmod file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}
	return nil
}

func sanitizeRawEvidence(evidence map[string]any) {
	for k, v := range evidence {
		switch val := v.(type) {
		case string:
			evidence[k] = sanitizeString(val)
		case []string:
			sanitized := make([]string, len(val))
			for i, item := range val {
				sanitized[i] = sanitizeString(item)
			}
			evidence[k] = sanitized
		case map[string][]string:
			sanitizedMap := make(map[string][]string)
			for subK, subVals := range val {
				sanitizedSub := make([]string, len(subVals))
				for i, item := range subVals {
					sanitizedSub[i] = sanitizeString(item)
				}
				sanitizedMap[subK] = sanitizedSub
			}
			evidence[k] = sanitizedMap
		case map[string]any:
			sanitizeRawEvidence(val)
		}
	}
}

// FormatHumanReadable formats the capability fingerprint into a clear terminal report.
func FormatHumanReadable(rep *FingerprintReport) string {
	var b bytes.Buffer

	fmt.Fprintln(&b, "================================================================================")
	fmt.Fprintln(&b, " XeroxGo — Printer Capability Fingerprint Report")
	fmt.Fprintln(&b, "================================================================================")
	fmt.Fprintf(&b, "Timestamp:  %s\n", rep.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(&b, "Sanitized:  %v (Identifiers, IPs, credentials redacted)\n\n", rep.Redacted)

	// Targets
	fmt.Fprintln(&b, "[TARGET ENDPOINTS]")
	if rep.Targets.QueueName != "" {
		fmt.Fprintf(&b, "  CUPS Queue:        %s\n", rep.Targets.QueueName)
		if rep.Targets.QueueDeviceURI != "" {
			fmt.Fprintf(&b, "  Device URI:        %s\n", rep.Targets.QueueDeviceURI)
		}
		if rep.Targets.DNSSDService != "" {
			resolvedStr := "UNRESOLVED"
			if rep.Targets.DNSSDResolved {
				resolvedStr = "RESOLVED"
			}
			fmt.Fprintf(&b, "  DNS-SD Service:    %s (%s)\n", rep.Targets.DNSSDService, resolvedStr)
		}
	}
	if rep.Targets.RemoteIPPURI != "" {
		fmt.Fprintf(&b, "  Remote IPP Target: %s\n", rep.Targets.RemoteIPPURI)
	}
	if rep.Targets.SNMPHost != "" {
		comm := rep.Targets.SNMPCommunity
		if comm == "" {
			comm = "[REDACTED]"
		}
		fmt.Fprintf(&b, "  SNMP Target:       %s:%d (Community: %s)\n", rep.Targets.SNMPHost, rep.Targets.SNMPPort, comm)
	}
	fmt.Fprintln(&b)

	// Identity
	fmt.Fprintln(&b, "[PRINTER IDENTITY]")
	makeModel := "Unknown"
	name := "Unknown"
	state := "Unknown"
	accepting := "Unknown"

	if rep.RemoteIPP != nil && rep.RemoteIPP.PrinterMakeAndModel != "" {
		makeModel = rep.RemoteIPP.PrinterMakeAndModel
	} else if rep.CUPSQueue != nil && rep.CUPSQueue.Capabilities.PrinterMakeAndModel != "" {
		makeModel = rep.CUPSQueue.Capabilities.PrinterMakeAndModel
	} else if rep.SNMP != nil && rep.SNMP.SysDescr != "" {
		makeModel = rep.SNMP.SysDescr
	}

	if rep.RemoteIPP != nil && rep.RemoteIPP.PrinterName != "" {
		name = rep.RemoteIPP.PrinterName
	} else if rep.CUPSQueue != nil && rep.CUPSQueue.Capabilities.PrinterName != "" {
		name = rep.CUPSQueue.Capabilities.PrinterName
	}

	if rep.RemoteIPP != nil && rep.RemoteIPP.PrinterState != "" {
		state = string(rep.RemoteIPP.PrinterState)
		accepting = string(rep.RemoteIPP.AcceptingJobs)
	} else if rep.CUPSQueue != nil && rep.CUPSQueue.PrinterState != "" {
		state = string(rep.CUPSQueue.PrinterState)
		accepting = string(rep.CUPSQueue.AcceptingJobs)
	}

	fmt.Fprintf(&b, "  Make & Model:      %s\n", makeModel)
	fmt.Fprintf(&b, "  Printer Name:      %s\n", name)
	fmt.Fprintf(&b, "  Operational State: %s (Accepting Jobs: %s)\n\n", state, accepting)

	// Comparisons
	fmt.Fprintln(&b, "[CAPABILITY CONSENSUS & DISCREPANCIES]")
	fmt.Fprintf(&b, "  %-28s  %-12s  %-12s  %-12s\n", "Feature", "CUPS Queue", "Remote IPP", "Consensus")
	fmt.Fprintln(&b, "  ----------------------------------------------------------------------")
	for _, c := range rep.Comparisons {
		fmt.Fprintf(&b, "  %-28s  %-12s  %-12s  %-12s\n", c.Feature, c.CUPSQueue, c.RemoteIPP, c.Consensus)
		if c.Discrepancy {
			fmt.Fprintf(&b, "    └─> WARNING: %s\n", c.Notes)
		}
	}
	fmt.Fprintln(&b)

	// Job Progress & Telemetry
	fmt.Fprintln(&b, "[JOB PROGRESS & PHYSICAL TELEMETRY EVALUATION]")
	fmt.Fprintf(&b, "  Telemetry Rating:            %s\n", rep.Telemetry.Rating)
	fmt.Fprintf(&b, "  Physical Ejection Verified:  %v (Discovery is read-only; requires active lifecycle validation)\n", rep.Telemetry.PhysicalEjectionVerified)
	fmt.Fprintf(&b, "  Safe For Physical Delivery:  %v (Discovery is read-only; physical completion unverified)\n", rep.Telemetry.SafeForPhysicalCompletion)
	fmt.Fprintf(&b, "  IPP impressions-completed:   %s\n", progressCapStr(rep.RemoteIPP, "impressions"))
	fmt.Fprintf(&b, "  IPP media-sheets-completed:  %s\n", progressCapStr(rep.RemoteIPP, "sheets"))
	if rep.Telemetry.HasSNMPMarkerCounter {
		idxStr := ""
		if rep.Telemetry.SNMPMarkerIndex != "" {
			idxStr = fmt.Sprintf("Index: %s, ", rep.Telemetry.SNMPMarkerIndex)
		}
		fmt.Fprintf(&b, "  SNMP Marker Life Count:      %d (%sUnit: %s, Validated Page Unit: %v)\n",
			rep.Telemetry.SNMPMarkerLifeCount, idxStr, rep.Telemetry.SNMPMarkerCounterUnit, rep.Telemetry.SNMPMarkerUnitValid)
	} else {
		fmt.Fprintln(&b, "  SNMP Marker Life Count:      ABSENT")
	}
	for _, w := range rep.Telemetry.Warnings {
		fmt.Fprintf(&b, "  [!] %s\n", w)
	}
	fmt.Fprintln(&b)

	// Supplies (Dynamic SNMP)
	if rep.SNMP != nil && len(rep.SNMP.Supplies) > 0 {
		fmt.Fprintln(&b, "[SUPPLIES (DYNAMIC SNMP DISCOVERY)]")
		fmt.Fprintf(&b, "  %-6s  %-26s  %-12s  %-10s  %-10s  %-8s\n", "Index", "Description", "Type", "Unit", "Level", "Capacity")
		fmt.Fprintln(&b, "  --------------------------------------------------------------------------------")
		for _, s := range rep.SNMP.Supplies {
			capStr := fmt.Sprintf("%d", s.MaxCapacity)
			if s.MaxCapacity <= 0 {
				capStr = "N/A"
			}
			fmt.Fprintf(&b, "  %-6s  %-26s  %-12s  %-10s  %-10s  %-8s\n",
				s.Index, truncateStr(s.Description, 26), s.TypeName, s.UnitName, s.LevelDesc, capStr)
		}
		fmt.Fprintln(&b)
	} else if rep.RemoteIPP != nil && len(rep.RemoteIPP.Markers) > 0 {
		fmt.Fprintln(&b, "[SUPPLIES (IPP MARKERS)]")
		fmt.Fprintf(&b, "  %-26s  %-12s  %-8s  %-10s\n", "Name", "Type", "Color", "Level")
		fmt.Fprintln(&b, "  ------------------------------------------------------------------")
		for _, m := range rep.RemoteIPP.Markers {
			fmt.Fprintf(&b, "  %-26s  %-12s  %-8s  %d%%\n", truncateStr(m.Name, 26), m.Type, m.Color, m.Level)
		}
		fmt.Fprintln(&b)
	}

	// Active Alerts
	alertCount := 0
	if rep.SNMP != nil {
		alertCount += len(rep.SNMP.Alerts)
	}
	if rep.RemoteIPP != nil {
		alertCount += len(rep.RemoteIPP.Alerts)
	}
	if alertCount > 0 {
		fmt.Fprintln(&b, "[ACTIVE PRINTER ALERTS]")
		if rep.SNMP != nil {
			for _, a := range rep.SNMP.Alerts {
				fmt.Fprintf(&b, "  SNMP Alert [%s / %s]: %s (code %d, group %s)\n",
					a.SeverityName, a.TrainingName, a.Description, a.Code, a.GroupName)
			}
		}
		if rep.RemoteIPP != nil {
			for _, a := range rep.RemoteIPP.Alerts {
				fmt.Fprintf(&b, "  IPP Alert: %s\n", a)
			}
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "================================================================================")
	return b.String()
}

func progressCapStr(remote *IPPProbeResult, kind string) string {
	if remote == nil {
		return "UNKNOWN"
	}
	if kind == "impressions" {
		return string(remote.JobProgressSupport.JobImpressionsCompleted)
	}
	return string(remote.JobProgressSupport.JobMediaSheetsCompleted)
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// FormatJSON produces indented JSON bytes for the report.
func FormatJSON(rep *FingerprintReport) ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

func splitCleanValues(val string) []string {
	parts := strings.Split(val, ",")
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		trimmed = strings.Trim(trimmed, `"`)
		trimmed = strings.Trim(trimmed, `'`)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// SortSuppliesByIndex sorts supplies by row index.
func SortSuppliesByIndex(supplies []SNMPSupply) {
	sort.Slice(supplies, func(i, j int) bool {
		return supplies[i].Index < supplies[j].Index
	})
}
