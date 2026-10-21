package pdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"xeroxgo/pkg/contracts"
)

type NormalizedResult struct {
	TotalPages     int
	TotalSheets    int
	SHA256Checksum string
}

func NormalizeAndCount(
	ctx context.Context,
	inputPath, outputPath string,
	copies int,
	duplex contracts.DuplexMode,
) (*NormalizedResult, error) {
	// Step 1: Validate arguments and filesystem paths
	if ctx == nil {
		return nil, errors.New("context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if inputPath == "" {
		return nil, errors.New("inputPath must not be empty")
	}
	if outputPath == "" {
		return nil, errors.New("outputPath must not be empty")
	}
	if copies < 1 {
		return nil, fmt.Errorf("copies must be at least 1, got %d", copies)
	}

	switch duplex {
	case contracts.DuplexSimplex, contracts.DuplexLongEdge, contracts.DuplexShortEdge:
	default:
		return nil, fmt.Errorf("invalid duplex mode: %q", duplex)
	}

	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("input file stat failed: %w", err)
	}
	if !inputInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("input path is not a regular file: %s", inputPath)
	}

	absInput, err := filepath.Abs(inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute input path: %w", err)
	}
	absOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute output path: %w", err)
	}

	if filepath.Clean(absInput) == filepath.Clean(absOutput) {
		return nil, errors.New("input and output paths identify the same file")
	}

	if outputInfo, err := os.Stat(outputPath); err == nil {
		if os.SameFile(inputInfo, outputInfo) {
			return nil, errors.New("input and output paths identify the same file")
		}
	}

	// Step 2: Validate PDF structure using qpdf
	cmdCheck := exec.CommandContext(ctx, "qpdf", "--check", inputPath)
	outCheck, errCheck := cmdCheck.CombinedOutput()
	if errCheck != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(errCheck, &exitErr) {
			if exitErr.ExitCode() != 3 {
				return nil, fmt.Errorf("qpdf --check failed (exit code %d): %s: %w", exitErr.ExitCode(), strings.TrimSpace(string(outCheck)), errCheck)
			}
			// Exit status 3 is warning-only; accepted per specification
		} else {
			return nil, fmt.Errorf("failed to execute qpdf --check: %s: %w", strings.TrimSpace(string(outCheck)), errCheck)
		}
	}

	cmdEnc := exec.CommandContext(ctx, "qpdf", "--is-encrypted", inputPath)
	outEnc, errEnc := cmdEnc.CombinedOutput()
	if errEnc == nil {
		// Exit code 0 means the PDF is encrypted
		return nil, fmt.Errorf("pdf is encrypted: %s", inputPath)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var exitErrEnc *exec.ExitError
	if errors.As(errEnc, &exitErrEnc) {
		if exitErrEnc.ExitCode() != 2 {
			// Exit code 2 means not encrypted; any other code is an error
			return nil, fmt.Errorf("qpdf --is-encrypted failed (exit code %d): %s: %w", exitErrEnc.ExitCode(), strings.TrimSpace(string(outEnc)), errEnc)
		}
	} else {
		return nil, fmt.Errorf("failed to execute qpdf --is-encrypted: %s: %w", strings.TrimSpace(string(outEnc)), errEnc)
	}

	// Step 3: Normalize the PDF with Ghostscript
	cmdGS := exec.CommandContext(ctx, "gs",
		"-sDEVICE=pdfwrite",
		"-dCompatibilityLevel=1.7",
		"-dPDFSETTINGS=/prepress",
		"-sColorConversionStrategy=Gray",
		"-dProcessColorModel=/DeviceGray",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dPDFFitPage",
		"-dNOPAUSE",
		"-dQUIET",
		"-dBATCH",
		"-sOutputFile="+outputPath,
		inputPath,
	)
	outGS, errGS := cmdGS.CombinedOutput()
	if errGS != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ghostscript normalization failed: %s: %w", strings.TrimSpace(string(outGS)), errGS)
	}

	// Step 4: Count normalized pages
	cmdPages := exec.CommandContext(ctx, "qpdf", "--show-npages", outputPath)
	var stdoutPages, stderrPages strings.Builder
	cmdPages.Stdout = &stdoutPages
	cmdPages.Stderr = &stderrPages
	if err := cmdPages.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("qpdf --show-npages failed: %s: %w", strings.TrimSpace(stderrPages.String()), err)
	}

	totalPages, err := strconv.Atoi(strings.TrimSpace(stdoutPages.String()))
	if err != nil {
		return nil, fmt.Errorf("failed to parse page count %q: %w", stdoutPages.String(), err)
	}
	if totalPages <= 0 {
		return nil, fmt.Errorf("invalid normalized page count: %d", totalPages)
	}

	// Step 5: Calculate physical sheets
	var sheetsPerCopy int
	switch duplex {
	case contracts.DuplexSimplex:
		sheetsPerCopy = totalPages
	case contracts.DuplexLongEdge, contracts.DuplexShortEdge:
		sheetsPerCopy = totalPages/2 + totalPages%2
	}

	if copies > 0 && sheetsPerCopy > math.MaxInt/copies {
		return nil, errors.New("arithmetic overflow calculating total sheets")
	}
	totalSheets := sheetsPerCopy * copies

	// Step 6: Compute the SHA-256 checksum
	f, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open normalized output file for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("failed to compute checksum of normalized output file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("failed to close normalized output file: %w", err)
	}

	checksum := hex.EncodeToString(h.Sum(nil))

	// Step 7: Return the result
	return &NormalizedResult{
		TotalPages:     totalPages,
		TotalSheets:    totalSheets,
		SHA256Checksum: checksum,
	}, nil
}
