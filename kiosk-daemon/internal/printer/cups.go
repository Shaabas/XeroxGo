package printer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// CommandRunner executes a command with context and returns combined output or stdout.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var defaultCommandRunner CommandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

var cupsJobIDRegex = regexp.MustCompile(`(?i)request id is ([a-zA-Z0-9_\-\.]+)`)

// PrintPDFWithCUPS submits a PDF file to the specified CUPS queue via the lp command
// and monitors its progress until completion.
func PrintPDFWithCUPS(
	ctx context.Context,
	queue string,
	cmd contracts.PrintJobCommand,
	pdfPath string,
	onPage func(sheetsCompleted int),
) error {
	return PrintPDFWithCUPSCustom(ctx, defaultCommandRunner, queue, cmd, pdfPath, onPage, 500*time.Millisecond)
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
	default:
		lpArgs = append(lpArgs, "-o", "sides=one-sided")
	}

	lpArgs = append(lpArgs, pdfPath)

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

	// 3. Monitor CUPS job until completed or context canceled
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Attempt to cancel the job in CUPS on timeout or context cancel
			cancelCtx, cancelTimeout := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = runner(cancelCtx, "cancel", cupsJobID)
			cancelTimeout()
			return ctx.Err()

		case <-ticker.C:
			statOut, statErr := runner(ctx, "lpstat", "-W", "not-completed", "-o", queue)
			if statErr != nil {
				// If lpstat fails intermittently, retry unless context cancelled
				continue
			}

			// If the job is no longer listed in not-completed, it finished printing
			if !strings.Contains(string(statOut), cupsJobID) {
				if onPage != nil {
					onPage(cmd.TotalSheets)
				}
				return nil
			}

			// Job is still active in the queue
			if onPage != nil && cmd.TotalSheets > 1 {
				// Signal partial progress (halfway) while processing
				onPage(cmd.TotalSheets / 2)
			}
		}
	}
}

// PollCUPSHealth checks the operational status of a CUPS queue using lpstat.
func PollCUPSHealth(ctx context.Context, queue string, estimatedSheets int) contracts.PrinterHealth {
	return PollCUPSHealthCustom(ctx, defaultCommandRunner, queue, estimatedSheets)
}

// PollCUPSHealthCustom allows injection of a custom command runner for testing CUPS health checks.
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

	out, err := runner(pollCtx, "lpstat", "-p", queue)
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

	if strings.Contains(raw, "disabled") {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperTrayStatus: "CHECK_PRINTER",
			EstimatedSheets: estimatedSheets,
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
		paperStatus := "OK"
		if strings.Contains(raw, "out of paper") || strings.Contains(raw, "media-empty") {
			paperStatus = "EMPTY"
		}
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: paperStatus,
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
