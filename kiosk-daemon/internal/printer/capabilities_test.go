package printer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParsePrinterCapabilities_TableDriven(t *testing.T) {
	tests := []struct {
		name       string
		queue      string
		output     string
		checkCaps  func(t *testing.T, caps PrinterCapabilities)
	}{
		{
			name:  "full capabilities with color and duplex",
			queue: "OfficePrinter",
			output: `
printer-name (nameWithoutLanguage) = OfficePrinter
printer-make-and-model (textWithoutLanguage) = HP LaserJet Enterprise M608
printer-info (textWithoutLanguage) = 2nd Floor Main Office
printer-state (enum) = idle
document-format-supported (1setOf mimeMediaType) = application/pdf,application/postscript,application/octet-stream
document-format-default (mimeMediaType) = application/pdf
sides-supported (1setOf keyword) = one-sided,two-sided-long-edge,two-sided-short-edge
sides-default (keyword) = two-sided-long-edge
print-color-mode-supported (1setOf keyword) = monochrome,color
print-color-mode-default (keyword) = monochrome
color-supported (boolean) = true
media-supported (1setOf keyword) = iso_a4_210x297mm,na_letter_8.5x11in
media-default (keyword) = iso_a4_210x297mm
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.Source != CapabilitySourceCUPSQueue {
					t.Errorf("expected Source %q, got %q", CapabilitySourceCUPSQueue, caps.Source)
				}
				if caps.Queue != "OfficePrinter" {
					t.Errorf("expected Queue OfficePrinter, got %q", caps.Queue)
				}
				if caps.PrinterName != "OfficePrinter" {
					t.Errorf("expected PrinterName OfficePrinter, got %q", caps.PrinterName)
				}
				if caps.PrinterMakeAndModel != "HP LaserJet Enterprise M608" {
					t.Errorf("expected MakeAndModel 'HP LaserJet Enterprise M608', got %q", caps.PrinterMakeAndModel)
				}
				if caps.PrinterInfo != "2nd Floor Main Office" {
					t.Errorf("expected PrinterInfo '2nd Floor Main Office', got %q", caps.PrinterInfo)
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
				if caps.DuplexShortEdgeSupport != CapabilitySupported {
					t.Errorf("expected DuplexShortEdgeSupport SUPPORTED, got %v", caps.DuplexShortEdgeSupport)
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
				if caps.SidesDefault != "two-sided-long-edge" {
					t.Errorf("expected SidesDefault two-sided-long-edge, got %q", caps.SidesDefault)
				}
				if caps.PrintColorModeDefault != "monochrome" {
					t.Errorf("expected PrintColorModeDefault monochrome, got %q", caps.PrintColorModeDefault)
				}
				if caps.MediaDefault != "iso_a4_210x297mm" {
					t.Errorf("expected MediaDefault iso_a4_210x297mm, got %q", caps.MediaDefault)
				}
			},
		},
		{
			name:  "monochrome only printer with color-supported false",
			queue: "BWPrinter",
			output: `
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
			name:  "monochrome only without color-supported boolean",
			queue: "MonoOnly",
			output: `
printer-name (nameWithoutLanguage) = MonoOnly
print-color-mode-supported (1setOf keyword) = monochrome
media-supported (1setOf keyword) = iso_a4_210x297mm
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.ColorSupport != CapabilityUnsupported {
					t.Errorf("expected ColorSupport UNSUPPORTED, got %v", caps.ColorSupport)
				}
				if caps.MonochromeSupport != CapabilitySupported {
					t.Errorf("expected MonochromeSupport SUPPORTED, got %v", caps.MonochromeSupport)
				}
				if caps.SimplexSupport != CapabilityUnknown {
					t.Errorf("expected SimplexSupport UNKNOWN, got %v", caps.SimplexSupport)
				}
			},
		},
		{
			name:  "simplex only printer without duplex",
			queue: "DeskjetSimplex",
			output: `
printer-name (nameWithoutLanguage) = DeskjetSimplex
document-format-supported (1setOf mimeMediaType) = application/pdf
sides-supported (keyword) = one-sided
media-supported (1setOf keyword) = na_letter_8.5x11in
print-color-mode-supported (1setOf keyword) = color
`,
			checkCaps: func(t *testing.T, caps PrinterCapabilities) {
				if caps.SimplexSupport != CapabilitySupported {
					t.Errorf("expected SimplexSupport SUPPORTED, got %v", caps.SimplexSupport)
				}
				if caps.DuplexLongEdgeSupport != CapabilityUnsupported {
					t.Errorf("expected DuplexLongEdgeSupport UNSUPPORTED, got %v", caps.DuplexLongEdgeSupport)
				}
				if caps.DuplexShortEdgeSupport != CapabilityUnsupported {
					t.Errorf("expected DuplexShortEdgeSupport UNSUPPORTED, got %v", caps.DuplexShortEdgeSupport)
				}
				if caps.A4Support != CapabilityUnsupported {
					t.Errorf("expected A4Support UNSUPPORTED (only Letter supported), got %v", caps.A4Support)
				}
				if caps.PDFSupport != CapabilitySupported {
					t.Errorf("expected PDFSupport SUPPORTED, got %v", caps.PDFSupport)
				}
			},
		},
		{
			name:  "missing attributes returns UNKNOWN not false",
			queue: "MinimalPrinter",
			output: `
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
			name:   "empty output returns all UNKNOWN without error",
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
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			// check if string contains context deadline
			if !errors.Is(errors.Unwrap(err), context.DeadlineExceeded) {
				t.Logf("got error: %v", err)
			}
		}
		if caps.Source != CapabilitySourceCUPSQueue {
			t.Errorf("expected Source %q, got %q", CapabilitySourceCUPSQueue, caps.Source)
		}
	})

	t.Run("successful discovery constructs correct URI and args", func(t *testing.T) {
		var invokedName string
		var invokedArgs []string

		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			invokedName = name
			invokedArgs = args
			return []byte(`
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
