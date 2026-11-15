package printer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParsePrinterCapabilities_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		queue     string
		output    string
		checkCaps func(t *testing.T, caps PrinterCapabilities)
	}{
		{
			name:  "successful real-style ipptool -tv output with full capabilities",
			queue: "DCPT310",
			output: `"/usr/share/cups/ipptool/get-printer-attributes.test":
    Get-Printer-Attributes:
        attributes-charset (charset) = utf-8
        attributes-natural-language (naturalLanguage) = en
        printer-uri (uri) = ipp://localhost:631/printers/DCPT310
        requested-attributes (1setOf keyword) = all,media-col-database
    Get printer attributes using get-printer-attributes                  [PASS]
        RECEIVED: 17460 bytes in response
        status-code = successful-ok
        printer-name (nameWithoutLanguage) = DCPT310
        printer-make-and-model (textWithoutLanguage) = Brother DCP-T310 CUPS
        printer-info (textWithoutLanguage) = DCPT310 Front Desk
        printer-state (enum) = idle
        printer-is-accepting-jobs (boolean) = true
        document-format-supported (1setOf mimeMediaType) = application/octet-stream,application/pdf,application/postscript
        document-format-default (mimeMediaType) = application/pdf
        sides-supported (1setOf keyword) = one-sided,two-sided-long-edge
        sides-default (keyword) = one-sided
        print-color-mode-supported (1setOf keyword) = monochrome,color
        print-color-mode-default (keyword) = color
        color-supported (boolean) = true
        media-supported (1setOf keyword) = iso_a4_210x297mm,na_letter_8.5x11in
        media-default (keyword) = iso_a4_210x297mm
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.Source != CapabilitySourceCUPSQueue {
					t.Errorf("expected Source %q, got %q", CapabilitySourceCUPSQueue, caps.Source)
				}
				if caps.Queue != "DCPT310" {
					t.Errorf("expected Queue DCPT310, got %q", caps.Queue)
				}
				if caps.StatusCode != "successful-ok" {
					t.Errorf("expected StatusCode successful-ok, got %q", caps.StatusCode)
				}
				if caps.PrinterName != "DCPT310" {
					t.Errorf("expected PrinterName DCPT310, got %q", caps.PrinterName)
				}
				if caps.PrinterMakeAndModel != "Brother DCP-T310 CUPS" {
					t.Errorf("expected MakeAndModel 'Brother DCP-T310 CUPS', got %q", caps.PrinterMakeAndModel)
				}
				if caps.PrinterInfo != "DCPT310 Front Desk" {
					t.Errorf("expected PrinterInfo 'DCPT310 Front Desk', got %q", caps.PrinterInfo)
				}
				if caps.PrinterState != "idle" {
					t.Errorf("expected PrinterState idle, got %q", caps.PrinterState)
				}
				if caps.PDFSupport != CapabilitySupported {
					t.Errorf("expected PDFSupport SUPPORTED, got %v", caps.PDFSupport)
				}
				if caps.A4Support != CapabilitySupported {
					t.Errorf("expected A4Support SUPPORTED, got %v", caps.A4Support)
				}
				if caps.SimplexSupport != CapabilitySupported {
					t.Errorf("expected SimplexSupport SUPPORTED, got %v", caps.SimplexSupport)
				}
				if caps.DuplexLongEdgeSupport != CapabilitySupported {
					t.Errorf("expected DuplexLongEdgeSupport SUPPORTED, got %v", caps.DuplexLongEdgeSupport)
				}
				if caps.DuplexShortEdgeSupport != CapabilityUnsupported {
					t.Errorf("expected DuplexShortEdgeSupport UNSUPPORTED, got %v", caps.DuplexShortEdgeSupport)
				}
				if caps.ColorSupport != CapabilitySupported {
					t.Errorf("expected ColorSupport SUPPORTED, got %v", caps.ColorSupport)
				}
				if caps.MonochromeSupport != CapabilitySupported {
					t.Errorf("expected MonochromeSupport SUPPORTED, got %v", caps.MonochromeSupport)
				}
				if caps.DocumentFormatDefault != "application/pdf" {
					t.Errorf("expected DocumentFormatDefault application/pdf, got %q", caps.DocumentFormatDefault)
				}
				if caps.SidesDefault != "one-sided" {
					t.Errorf("expected SidesDefault one-sided, got %q", caps.SidesDefault)
				}
				if caps.PrintColorModeDefault != "color" {
					t.Errorf("expected PrintColorModeDefault color, got %q", caps.PrintColorModeDefault)
				}
				if caps.MediaDefault != "iso_a4_210x297mm" {
					t.Errorf("expected MediaDefault iso_a4_210x297mm, got %q", caps.MediaDefault)
				}
			},
		},
		{
			name:  "empty supported lists leave capabilities UNKNOWN not UNSUPPORTED",
			queue: "EmptyListsPrinter",
			output: `
status-code = successful-ok
printer-name (nameWithoutLanguage) = EmptyListsPrinter
document-format-supported = 
sides-supported = ,,,
media-supported = 
print-color-mode-supported =
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.PDFSupport != CapabilityUnknown {
					t.Errorf("expected PDFSupport UNKNOWN for empty list, got %v", caps.PDFSupport)
				}
				if caps.A4Support != CapabilityUnknown {
					t.Errorf("expected A4Support UNKNOWN for empty list, got %v", caps.A4Support)
				}
				if caps.SimplexSupport != CapabilityUnknown {
					t.Errorf("expected SimplexSupport UNKNOWN for empty list, got %v", caps.SimplexSupport)
				}
				if caps.DuplexLongEdgeSupport != CapabilityUnknown {
					t.Errorf("expected DuplexLongEdgeSupport UNKNOWN for empty list, got %v", caps.DuplexLongEdgeSupport)
				}
				if caps.DuplexShortEdgeSupport != CapabilityUnknown {
					t.Errorf("expected DuplexShortEdgeSupport UNKNOWN for empty list, got %v", caps.DuplexShortEdgeSupport)
				}
				if caps.ColorSupport != CapabilityUnknown {
					t.Errorf("expected ColorSupport UNKNOWN for empty list, got %v", caps.ColorSupport)
				}
				if caps.MonochromeSupport != CapabilityUnknown {
					t.Errorf("expected MonochromeSupport UNKNOWN for empty list, got %v", caps.MonochromeSupport)
				}
			},
		},
		{
			name:  "valid lists explicitly excluding features yield UNSUPPORTED",
			queue: "RestrictedPrinter",
			output: `
status-code = successful-ok
printer-name (nameWithoutLanguage) = RestrictedPrinter
document-format-supported (1setOf mimeMediaType) = text/plain,application/postscript
sides-supported (keyword) = one-sided
media-supported (1setOf keyword) = na_letter_8.5x11in,na_legal_8.5x14in
print-color-mode-supported (1setOf keyword) = monochrome
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.PDFSupport != CapabilityUnsupported {
					t.Errorf("expected PDFSupport UNSUPPORTED when list excludes PDF, got %v", caps.PDFSupport)
				}
				if caps.A4Support != CapabilityUnsupported {
					t.Errorf("expected A4Support UNSUPPORTED when list excludes A4, got %v", caps.A4Support)
				}
				if caps.SimplexSupport != CapabilitySupported {
					t.Errorf("expected SimplexSupport SUPPORTED, got %v", caps.SimplexSupport)
				}
				if caps.DuplexLongEdgeSupport != CapabilityUnsupported {
					t.Errorf("expected DuplexLongEdgeSupport UNSUPPORTED when list excludes duplex, got %v", caps.DuplexLongEdgeSupport)
				}
				if caps.DuplexShortEdgeSupport != CapabilityUnsupported {
					t.Errorf("expected DuplexShortEdgeSupport UNSUPPORTED when list excludes tumble, got %v", caps.DuplexShortEdgeSupport)
				}
				if caps.ColorSupport != CapabilityUnsupported {
					t.Errorf("expected ColorSupport UNSUPPORTED when list excludes color, got %v", caps.ColorSupport)
				}
				if caps.MonochromeSupport != CapabilitySupported {
					t.Errorf("expected MonochromeSupport SUPPORTED, got %v", caps.MonochromeSupport)
				}
			},
		},
		{
			name:  "monochrome only printer with color-supported false",
			queue: "BWPrinter",
			output: `
status-code = successful-ok
printer-name (nameWithoutLanguage) = BWPrinter
document-format-supported (1setOf mimeMediaType) = application/pdf
sides-supported (1setOf keyword) = one-sided,two-sided-long-edge
media-supported (1setOf keyword) = iso_a4_210x297mm
print-color-mode-supported (1setOf keyword) = monochrome
color-supported (boolean) = false
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.ColorSupport != CapabilityUnsupported {
					t.Errorf("expected ColorSupport UNSUPPORTED, got %v", caps.ColorSupport)
				}
				if caps.MonochromeSupport != CapabilitySupported {
					t.Errorf("expected MonochromeSupport SUPPORTED, got %v", caps.MonochromeSupport)
				}
				if caps.PDFSupport != CapabilitySupported {
					t.Errorf("expected PDFSupport SUPPORTED, got %v", caps.PDFSupport)
				}
				if caps.DuplexLongEdgeSupport != CapabilitySupported {
					t.Errorf("expected DuplexLongEdgeSupport SUPPORTED, got %v", caps.DuplexLongEdgeSupport)
				}
				if caps.DuplexShortEdgeSupport != CapabilityUnsupported {
					t.Errorf("expected DuplexShortEdgeSupport UNSUPPORTED, got %v", caps.DuplexShortEdgeSupport)
				}
			},
		},
		{
			name:  "missing attributes returns UNKNOWN not false",
			queue: "MinimalPrinter",
			output: `
status-code = successful-ok
printer-name (nameWithoutLanguage) = MinimalPrinter
printer-state (enum) = idle
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.PDFSupport != CapabilityUnknown {
					t.Errorf("expected PDFSupport UNKNOWN, got %v", caps.PDFSupport)
				}
				if caps.A4Support != CapabilityUnknown {
					t.Errorf("expected A4Support UNKNOWN, got %v", caps.A4Support)
				}
				if caps.SimplexSupport != CapabilityUnknown {
					t.Errorf("expected SimplexSupport UNKNOWN, got %v", caps.SimplexSupport)
				}
				if caps.DuplexLongEdgeSupport != CapabilityUnknown {
					t.Errorf("expected DuplexLongEdgeSupport UNKNOWN, got %v", caps.DuplexLongEdgeSupport)
				}
				if caps.DuplexShortEdgeSupport != CapabilityUnknown {
					t.Errorf("expected DuplexShortEdgeSupport UNKNOWN, got %v", caps.DuplexShortEdgeSupport)
				}
				if caps.ColorSupport != CapabilityUnknown {
					t.Errorf("expected ColorSupport UNKNOWN, got %v", caps.ColorSupport)
				}
				if caps.MonochromeSupport != CapabilityUnknown {
					t.Errorf("expected MonochromeSupport UNKNOWN, got %v", caps.MonochromeSupport)
				}
			},
		},
		{
			name:   "empty output returns all UNKNOWN",
			queue:  "EmptyQueue",
			output: "",
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.Source != CapabilitySourceCUPSQueue {
					t.Errorf("expected Source %q, got %q", CapabilitySourceCUPSQueue, caps.Source)
				}
				if caps.PDFSupport != CapabilityUnknown || caps.A4Support != CapabilityUnknown {
					t.Errorf("expected all capabilities UNKNOWN for empty output")
				}
			},
		},
		{
			name:  "malformed and noisy output without crashing",
			queue: "CorruptedQueue",
			output: `
Get printer attributes using get-printer-attributes                  [FAIL]
RECEIVED: 17460 bytes in response
status-code = client-error-bad-request ("media-supported": Bad keyword value "" - invalid character (RFC 8011 section 5.1.4).)
this is a random invalid line without equals sign
another invalid line =
= missing key
media-supported (1setOf keyword) = ,,iso_a4,,na_letter,,
document-format-supported = text/plain,application/octet-stream
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.StatusCode != "client-error-bad-request" {
					t.Errorf("expected StatusCode client-error-bad-request, got %q", caps.StatusCode)
				}
				if caps.A4Support != CapabilitySupported {
					t.Errorf("expected A4Support SUPPORTED from iso_a4 keyword, got %v", caps.A4Support)
				}
				if caps.PDFSupport != CapabilityUnsupported {
					t.Errorf("expected PDFSupport UNSUPPORTED when document-format-supported has no PDF, got %v", caps.PDFSupport)
				}
				if caps.SimplexSupport != CapabilityUnknown {
					t.Errorf("expected SimplexSupport UNKNOWN since sides-supported missing, got %v", caps.SimplexSupport)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := ParsePrinterCapabilities([]byte(tt.output), tt.queue)
			tt.checkCaps(t, caps)
		})
	}
}

func TestDiscoverCUPSPrinterCapabilities_RunnerCases(t *testing.T) {
	t.Run("empty queue name returns error", func(t *testing.T) {
		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), nil, "   ")
		if err == nil {
			t.Fatal("expected error for empty queue name")
		}
		if caps.PDFSupport != CapabilityUnknown {
			t.Errorf("expected PDFSupport UNKNOWN, got %v", caps.PDFSupport)
		}
	})

	t.Run("empty output returns error", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("   \n\t  "), nil
		}

		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), runner, "EmptyQueue")
		if err == nil {
			t.Fatal("expected error when output has no recognizable attributes")
		}
		if !strings.Contains(err.Error(), "no recognizable printer attributes") {
			t.Errorf("expected error message about missing attributes, got: %v", err)
		}
		if caps.PDFSupport != CapabilityUnknown {
			t.Errorf("expected PDFSupport UNKNOWN, got %v", caps.PDFSupport)
		}
	})

	t.Run("diagnostic noise without attributes returns error", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(`
Get printer attributes using get-printer-attributes                  [FAIL]
connecting to localhost:631...
HTTP/1.1 500 Internal Server Error
`), nil
		}

		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), runner, "NoiseQueue")
		if err == nil {
			t.Fatal("expected error when output contains only diagnostic noise")
		}
		if caps.PDFSupport != CapabilityUnknown {
			t.Errorf("expected PDFSupport UNKNOWN, got %v", caps.PDFSupport)
		}
	})

	t.Run("explicit IPP client-error status returns error even with nil runner error", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			output := `
status-code = client-error-not-found (The printer or class does not exist.)
printer-name (nameWithoutLanguage) = GhostQueue
`
			return []byte(output), nil
		}

		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), runner, "GhostQueue")
		if err == nil {
			t.Fatal("expected error for explicit client-error-not-found status")
		}
		if !strings.Contains(err.Error(), "client-error-not-found") {
			t.Errorf("expected error mentioning status code, got: %v", err)
		}
		// Partial parsed attributes preserved
		if caps.PrinterName != "GhostQueue" {
			t.Errorf("expected PrinterName GhostQueue preserved, got %q", caps.PrinterName)
		}
		if caps.StatusCode != "client-error-not-found" {
			t.Errorf("expected StatusCode client-error-not-found, got %q", caps.StatusCode)
		}
	})

	t.Run("explicit IPP server-error status returns error even with nil runner error", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			output := `
status-code = server-error-internal-error
printer-name (nameWithoutLanguage) = BrokenServerQueue
`
			return []byte(output), nil
		}

		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), runner, "BrokenServerQueue")
		if err == nil {
			t.Fatal("expected error for explicit server-error status")
		}
		if !strings.Contains(err.Error(), "server-error-internal-error") {
			t.Errorf("expected error mentioning status code, got: %v", err)
		}
		if caps.PrinterName != "BrokenServerQueue" {
			t.Errorf("expected PrinterName preserved, got %q", caps.PrinterName)
		}
	})

	t.Run("command execution error returns error and partial capabilities", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			partialOutput := `
printer-name (nameWithoutLanguage) = PartialQueue
document-format-supported (1setOf mimeMediaType) = application/pdf
status-code = client-error-bad-request
`
			return []byte(partialOutput), errors.New("exit status 1")
		}

		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), runner, "PartialQueue")
		if err == nil {
			t.Fatal("expected error from runner")
		}
		// Partial capability data is preserved
		if caps.PrinterName != "PartialQueue" {
			t.Errorf("expected PrinterName PartialQueue, got %q", caps.PrinterName)
		}
		if caps.PDFSupport != CapabilitySupported {
			t.Errorf("expected PDFSupport SUPPORTED from partial output, got %v", caps.PDFSupport)
		}
		if caps.A4Support != CapabilityUnknown {
			t.Errorf("expected A4Support UNKNOWN, got %v", caps.A4Support)
		}
	})

	t.Run("absent ipptool command error", func(t *testing.T) {
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("exec: \"ipptool\": executable file not found in $PATH")
		}

		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), runner, "DCPT310")
		if err == nil {
			t.Fatal("expected error when ipptool is absent")
		}
		if caps.PDFSupport != CapabilityUnknown {
			t.Errorf("expected PDFSupport UNKNOWN on missing binary, got %v", caps.PDFSupport)
		}
		if caps.Source != CapabilitySourceCUPSQueue {
			t.Errorf("expected Source %q, got %q", CapabilitySourceCUPSQueue, caps.Source)
		}
	})

	t.Run("context cancellation and timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
				return []byte("printer-name (nameWithoutLanguage) = SlowPrinter"), nil
			}
		}

		caps, err := DiscoverCUPSPrinterCapabilities(ctx, runner, "SlowPrinter")
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if caps.Source != CapabilitySourceCUPSQueue {
			t.Errorf("expected Source %q, got %q", CapabilitySourceCUPSQueue, caps.Source)
		}
	})

	t.Run("successful real-style discovery constructs correct URI and args", func(t *testing.T) {
		var invokedName string
		var invokedArgs []string

		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			invokedName = name
			invokedArgs = args
			return []byte(`
status-code = successful-ok
printer-name (nameWithoutLanguage) = MyQueue
document-format-supported (1setOf mimeMediaType) = application/pdf
media-supported (1setOf keyword) = iso_a4_210x297mm
sides-supported (1setOf keyword) = one-sided,two-sided-long-edge
print-color-mode-supported (1setOf keyword) = monochrome,color
`), nil
		}

		caps, err := DiscoverCUPSPrinterCapabilities(context.Background(), runner, "MyQueue")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if invokedName != "ipptool" {
			t.Errorf("expected binary ipptool, got %q", invokedName)
		}
		expectedURI := "ipp://localhost:631/printers/MyQueue"
		if len(invokedArgs) < 3 || invokedArgs[0] != "-tv" || invokedArgs[1] != expectedURI || invokedArgs[2] != "get-printer-attributes.test" {
			t.Errorf("unexpected arguments: %v", invokedArgs)
		}

		if caps.PDFSupport != CapabilitySupported {
			t.Errorf("expected PDFSupport SUPPORTED, got %v", caps.PDFSupport)
		}
		if caps.A4Support != CapabilitySupported {
			t.Errorf("expected A4Support SUPPORTED, got %v", caps.A4Support)
		}
		if caps.SimplexSupport != CapabilitySupported {
			t.Errorf("expected SimplexSupport SUPPORTED, got %v", caps.SimplexSupport)
		}
		if caps.DuplexLongEdgeSupport != CapabilitySupported {
			t.Errorf("expected DuplexLongEdgeSupport SUPPORTED, got %v", caps.DuplexLongEdgeSupport)
		}
		if caps.DuplexShortEdgeSupport != CapabilityUnsupported {
			t.Errorf("expected DuplexShortEdgeSupport UNSUPPORTED, got %v", caps.DuplexShortEdgeSupport)
		}
		if caps.ColorSupport != CapabilitySupported {
			t.Errorf("expected ColorSupport SUPPORTED, got %v", caps.ColorSupport)
		}
		if caps.MonochromeSupport != CapabilitySupported {
			t.Errorf("expected MonochromeSupport SUPPORTED, got %v", caps.MonochromeSupport)
		}
	})
}
