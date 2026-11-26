package printer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// CapabilityStatus represents the tri-state support level of a printer feature.
type CapabilityStatus string

const (
	// CapabilityUnknown indicates the attribute was not present in the IPP response
	// or could not be determined.
	CapabilityUnknown CapabilityStatus = "UNKNOWN"
	// CapabilitySupported indicates the printer queue explicitly reports support for the feature.
	CapabilitySupported CapabilityStatus = "SUPPORTED"
	// CapabilityUnsupported indicates the printer queue explicitly omitted or reported no support.
	CapabilityUnsupported CapabilityStatus = "UNSUPPORTED"
)

const (
	// CapabilitySourceCUPSQueue identifies the capability origin as the local CUPS queue configuration.
	// This reflects the CUPS queue / PPD configuration, NOT verified physical hardware capabilities.
	CapabilitySourceCUPSQueue = "CUPS_QUEUE"
	// CapabilitySourceNativeIPP identifies telemetry queried directly from the physical printer's native IPP endpoint.
	CapabilitySourceNativeIPP = "NATIVE_IPP"
)

// PrinterCapabilities holds normalized and raw capability details discovered from a CUPS queue via IPP.
// Note: These capabilities represent the properties advertised by the CUPS queue and its driver/PPD.
// They do NOT constitute verified physical printer hardware capabilities.
type PrinterCapabilities struct {
	// Source identifies where these capabilities were queried from (e.g. CapabilitySourceCUPSQueue).
	Source string `json:"source"`
	// Queue is the name of the CUPS queue queried.
	Queue string `json:"queue"`

	// StatusCode is the IPP status code from the response (e.g. "successful-ok", "client-error-not-found").
	StatusCode string `json:"status_code,omitempty"`

	// Normalized capabilities (tri-state: SUPPORTED, UNSUPPORTED, UNKNOWN)
	PDFSupport             CapabilityStatus `json:"pdf_support"`
	A4Support              CapabilityStatus `json:"a4_support"`
	SimplexSupport         CapabilityStatus `json:"simplex_support"`
	DuplexLongEdgeSupport  CapabilityStatus `json:"duplex_long_edge_support"`
	DuplexShortEdgeSupport CapabilityStatus `json:"duplex_short_edge_support"`
	ColorSupport           CapabilityStatus `json:"color_support"`
	MonochromeSupport      CapabilityStatus `json:"monochrome_support"`

	// Printer identity attributes
	PrinterName         string `json:"printer_name,omitempty"`
	PrinterMakeAndModel string `json:"printer_make_and_model,omitempty"`
	PrinterInfo         string `json:"printer_info,omitempty"`
	PrinterState        string `json:"printer_state,omitempty"`

	// Raw multi-value attributes parsed from IPP response
	DocumentFormatSupported []string `json:"document_format_supported,omitempty"`
	SidesSupported          []string `json:"sides_supported,omitempty"`
	PrintColorModeSupported []string `json:"print_color_mode_supported,omitempty"`
	MediaSupported          []string `json:"media_supported,omitempty"`

	// Default attributes
	DocumentFormatDefault string `json:"document_format_default,omitempty"`
	SidesDefault          string `json:"sides_default,omitempty"`
	PrintColorModeDefault string `json:"print_color_mode_default,omitempty"`
	MediaDefault          string `json:"media_default,omitempty"`

	// RawAttributes contains all parsed IPP attributes
	RawAttributes map[string][]string `json:"raw_attributes,omitempty"`
}

// HasRecognizedAttributes returns true if at least one meaningful printer attribute was parsed.
func (c PrinterCapabilities) HasRecognizedAttributes() bool {
	return c.PrinterName != "" ||
		c.PrinterMakeAndModel != "" ||
		c.PrinterInfo != "" ||
		c.PrinterState != "" ||
		len(c.DocumentFormatSupported) > 0 ||
		len(c.SidesSupported) > 0 ||
		len(c.PrintColorModeSupported) > 0 ||
		len(c.MediaSupported) > 0 ||
		c.DocumentFormatDefault != "" ||
		c.SidesDefault != "" ||
		c.PrintColorModeDefault != "" ||
		c.MediaDefault != ""
}

var (
	ippAttributeRegex   = regexp.MustCompile(`^\s*([a-zA-Z0-9_\-]+)(?:\s*\([^\)]*\))?\s*=\s*(.*)$`)
	statusCodeTokenRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-]+`)
)

// parseIPPAttributes extracts normalized single-value attributes, multi-value attribute slices,
// and the lowercased IPP status code token from raw `ipptool -tv` output.
func parseIPPAttributes(out []byte, stripQuotes bool) (map[string]string, map[string][]string, string) {
	singleAttrs := make(map[string]string)
	rawAttrs := make(map[string][]string)

	if len(out) == 0 {
		return singleAttrs, rawAttrs, ""
	}

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

		parts := splitIPPValuesWithQuotes(val, stripQuotes)
		if len(parts) > 0 {
			rawAttrs[attrName] = append(rawAttrs[attrName], parts...)
		}
	}

	var statusCode string
	if v, ok := singleAttrs["status-code"]; ok {
		trimmed := strings.TrimSpace(v)
		if stripQuotes {
			trimmed = strings.Trim(trimmed, `"`)
		}
		if token := statusCodeTokenRegex.FindString(trimmed); token != "" {
			statusCode = strings.ToLower(token)
		} else {
			statusCode = strings.ToLower(trimmed)
		}
	}

	return singleAttrs, rawAttrs, statusCode
}

// ParsePrinterCapabilities parses the output of `ipptool -tv ... get-printer-attributes.test`
// and returns normalized printer capabilities.
// Unknown or missing attributes are kept distinct from unsupported attributes (never assumed false).
// Empty or unusable lists leave their normalized capabilities UNKNOWN, not UNSUPPORTED.
// UNSUPPORTED is assigned only when a valid, nonempty supported list explicitly excludes a feature.
func ParsePrinterCapabilities(out []byte, queue string) PrinterCapabilities {
	caps := PrinterCapabilities{
		Source:                 CapabilitySourceCUPSQueue,
		Queue:                  queue,
		PDFSupport:             CapabilityUnknown,
		A4Support:              CapabilityUnknown,
		SimplexSupport:         CapabilityUnknown,
		DuplexLongEdgeSupport:  CapabilityUnknown,
		DuplexShortEdgeSupport: CapabilityUnknown,
		ColorSupport:           CapabilityUnknown,
		MonochromeSupport:      CapabilityUnknown,
		RawAttributes:          make(map[string][]string),
	}

	if len(out) == 0 {
		return caps
	}

	singleAttrs, rawAttrs, statusCode := parseIPPAttributes(out, false)
	caps.RawAttributes = rawAttrs
	caps.StatusCode = statusCode

	// 1. Identity attributes
	if v, ok := singleAttrs["printer-name"]; ok {
		caps.PrinterName = v
	}
	if v, ok := singleAttrs["printer-make-and-model"]; ok {
		caps.PrinterMakeAndModel = v
	}
	if v, ok := singleAttrs["printer-info"]; ok {
		caps.PrinterInfo = v
	}
	if v, ok := singleAttrs["printer-state"]; ok {
		caps.PrinterState = v
	}

	// 2. Default attributes
	if v, ok := singleAttrs["document-format-default"]; ok {
		caps.DocumentFormatDefault = v
	}
	if v, ok := singleAttrs["sides-default"]; ok {
		caps.SidesDefault = v
	}
	if v, ok := singleAttrs["print-color-mode-default"]; ok {
		caps.PrintColorModeDefault = v
	}
	if v, ok := singleAttrs["media-default"]; ok {
		caps.MediaDefault = v
	}

	// 3. Raw slices
	caps.DocumentFormatSupported = caps.RawAttributes["document-format-supported"]
	caps.SidesSupported = caps.RawAttributes["sides-supported"]
	caps.PrintColorModeSupported = caps.RawAttributes["print-color-mode-supported"]
	caps.MediaSupported = caps.RawAttributes["media-supported"]

	// 4. Normalized: PDF support
	// An empty or unusable list leaves capability UNKNOWN.
	// UNSUPPORTED is assigned only when a valid, nonempty list explicitly excludes PDF.
	if len(caps.DocumentFormatSupported) > 0 {
		if containsCaseInsensitive(caps.DocumentFormatSupported, "application/pdf") {
			caps.PDFSupport = CapabilitySupported
		} else {
			caps.PDFSupport = CapabilityUnsupported
		}
	}

	// 5. Normalized: A4 support
	// An empty or unusable list leaves capability UNKNOWN.
	// UNSUPPORTED is assigned only when a valid, nonempty list explicitly excludes A4.
	if len(caps.MediaSupported) > 0 {
		if hasA4Media(caps.MediaSupported) {
			caps.A4Support = CapabilitySupported
		} else {
			caps.A4Support = CapabilityUnsupported
		}
	}

	// 6. Normalized: Sides / Duplex support
	// An empty or unusable list leaves capabilities UNKNOWN.
	// UNSUPPORTED is assigned only when a valid, nonempty list explicitly excludes the feature.
	if len(caps.SidesSupported) > 0 {
		if containsCaseInsensitive(caps.SidesSupported, "one-sided") || containsCaseInsensitive(caps.SidesSupported, "simplex") {
			caps.SimplexSupport = CapabilitySupported
		} else {
			caps.SimplexSupport = CapabilityUnsupported
		}

		if containsCaseInsensitive(caps.SidesSupported, "two-sided-long-edge") || containsCaseInsensitive(caps.SidesSupported, "duplex") {
			caps.DuplexLongEdgeSupport = CapabilitySupported
		} else {
			caps.DuplexLongEdgeSupport = CapabilityUnsupported
		}

		if containsCaseInsensitive(caps.SidesSupported, "two-sided-short-edge") || containsCaseInsensitive(caps.SidesSupported, "tumble") {
			caps.DuplexShortEdgeSupport = CapabilitySupported
		} else {
			caps.DuplexShortEdgeSupport = CapabilityUnsupported
		}
	}

	// 7. Normalized: Color and Monochrome support
	// Empty or unusable lists leave capabilities UNKNOWN.
	colorSupportedVal := strings.ToLower(strings.TrimSpace(singleAttrs["color-supported"]))
	hasValidModes := len(caps.PrintColorModeSupported) > 0

	if colorSupportedVal == "false" {
		caps.ColorSupport = CapabilityUnsupported
		caps.MonochromeSupport = CapabilitySupported
	} else if colorSupportedVal == "true" {
		caps.ColorSupport = CapabilitySupported
		if hasValidModes {
			if containsAnyCaseInsensitive(caps.PrintColorModeSupported, "monochrome", "process-monochrome", "auto", "bi-level", "monochrome-only") {
				caps.MonochromeSupport = CapabilitySupported
			} else {
				caps.MonochromeSupport = CapabilityUnsupported
			}
		}
	} else if hasValidModes {
		if containsCaseInsensitive(caps.PrintColorModeSupported, "color") {
			caps.ColorSupport = CapabilitySupported
		} else {
			caps.ColorSupport = CapabilityUnsupported
		}

		if containsAnyCaseInsensitive(caps.PrintColorModeSupported, "monochrome", "process-monochrome", "auto", "bi-level", "monochrome-only") {
			caps.MonochromeSupport = CapabilitySupported
		} else {
			caps.MonochromeSupport = CapabilityUnsupported
		}
	}

	return caps
}

// DiscoverCUPSPrinterCapabilities queries Get-Printer-Attributes via ipptool for the given CUPS queue.
// It parses the response into normalized and raw capabilities.
// If ipptool is missing, times out, returns an execution error, returns an IPP failure status,
// or returns output lacking recognizable printer attributes, an error is returned along with
// any partial capabilities parsed from the response.
func DiscoverCUPSPrinterCapabilities(ctx context.Context, runner CommandRunner, queue string) (PrinterCapabilities, error) {
	queue = strings.TrimSpace(queue)
	if queue == "" {
		return PrinterCapabilities{
			Source:                 CapabilitySourceCUPSQueue,
			PDFSupport:             CapabilityUnknown,
			A4Support:              CapabilityUnknown,
			SimplexSupport:         CapabilityUnknown,
			DuplexLongEdgeSupport:  CapabilityUnknown,
			DuplexShortEdgeSupport: CapabilityUnknown,
			ColorSupport:           CapabilityUnknown,
			MonochromeSupport:      CapabilityUnknown,
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
	caps := ParsePrinterCapabilities(out, queue)

	// 1. Runner execution error (e.g. absent ipptool, exit error, timeout)
	if runnerErr != nil {
		return caps, fmt.Errorf("ipptool query failed for CUPS queue %q: %w", queue, runnerErr)
	}

	// 2. Explicit IPP failure status (e.g. client-error-not-found, server-error-internal-error)
	if isIPPFailureStatus(caps.StatusCode) {
		return caps, fmt.Errorf("IPP query returned failure status %q for CUPS queue %q", caps.StatusCode, queue)
	}

	// 3. Lack of recognizable printer attributes (diagnostic noise or empty output)
	if !caps.HasRecognizedAttributes() {
		return caps, fmt.Errorf("no recognizable printer attributes found in IPP response for CUPS queue %q", queue)
	}

	return caps, nil
}

func isIPPFailureStatus(statusCode string) bool {
	sc := strings.ToLower(strings.TrimSpace(statusCode))
	if sc == "" {
		return false
	}
	if strings.HasPrefix(sc, "client-error-") || strings.HasPrefix(sc, "server-error-") {
		return true
	}
	if !strings.HasPrefix(sc, "successful-") && !strings.HasPrefix(sc, "informational-") {
		return true
	}
	return false
}

func splitIPPValues(val string) []string {
	return splitIPPValuesWithQuotes(val, false)
}

func splitIPPValuesWithQuotes(val string, stripQuotes bool) []string {
	parts := strings.Split(val, ",")
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if stripQuotes {
			trimmed = strings.Trim(trimmed, `"`)
			trimmed = strings.Trim(trimmed, `'`)
		}
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func containsCaseInsensitive(slice []string, target string) bool {
	for _, item := range slice {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}

func containsAnyCaseInsensitive(slice []string, targets ...string) bool {
	for _, target := range targets {
		if containsCaseInsensitive(slice, target) {
			return true
		}
	}
	return false
}

func hasA4Media(mediaList []string) bool {
	for _, m := range mediaList {
		lower := strings.ToLower(strings.TrimSpace(m))
		if lower == "iso_a4_210x297mm" || lower == "iso-a4" || lower == "a4" || lower == "iso_a4" ||
			strings.HasPrefix(lower, "iso_a4") || strings.HasPrefix(lower, "iso-a4") {
			return true
		}
	}
	return false
}
