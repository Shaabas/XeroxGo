package pdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"xeroxgo/pkg/contracts"
)

// ContinuationResult holds metadata about a generated recovery continuation artifact.
type ContinuationResult struct {
	RemainingPages int
	RemainingSheets int
	SHA256Checksum  string
	OutputPath      string
}

// GenerateContinuationPDF creates an immutable continuation PDF from the canonical
// print-ready input PDF using cloud-side qpdf.
//
// Key guarantees:
// 1. Strict sheet boundary alignment: in duplex mode, continuation always begins on
//    an odd-numbered canonical page (physical front side) and ends on an even-numbered page.
// 2. Multi-copy continuity: if printing failed midway through copy M of C, the continuation
//    artifact begins with the remaining sheets of copy M, followed by full copies (M+1)..C.
// 3. Imposition preservation: because slicing operates on the canonical print-ready PDF,
//    all 1-up, 2-up, 4-up, and 6-up page arrangements, rotations, and margins remain identical.
// 4. Zero Pi manipulation: the resulting PDF has start_from_page=1 and copies=1, requiring
//    no slicing or transformation on the kiosk.
func GenerateContinuationPDF(
	ctx context.Context,
	inputPDF string,
	outputPDF string,
	totalPages int,
	totalSheets int,
	copies int,
	duplex contracts.DuplexMode,
	confirmedSheetsPrinted int,
) (*ContinuationResult, error) {
	if ctx == nil {
		return nil, errors.New("generate continuation: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("generate continuation: context canceled: %w", err)
	}

	// 1. Path & parameter validation
	if strings.TrimSpace(inputPDF) == "" {
		return nil, errors.New("generate continuation: inputPDF cannot be empty")
	}
	if strings.TrimSpace(outputPDF) == "" {
		return nil, errors.New("generate continuation: outputPDF cannot be empty")
	}
	if totalPages < 1 {
		return nil, fmt.Errorf("generate continuation: totalPages must be >= 1, got %d", totalPages)
	}
	if totalSheets < 1 {
		return nil, fmt.Errorf("generate continuation: totalSheets must be >= 1, got %d", totalSheets)
	}
	if copies < 1 {
		return nil, fmt.Errorf("generate continuation: copies must be >= 1, got %d", copies)
	}
	if confirmedSheetsPrinted < 0 {
		return nil, fmt.Errorf("generate continuation: confirmedSheetsPrinted must be >= 0, got %d", confirmedSheetsPrinted)
	}
	if confirmedSheetsPrinted > totalSheets {
		return nil, fmt.Errorf("generate continuation: confirmedSheetsPrinted (%d) exceeds totalSheets (%d)", confirmedSheetsPrinted, totalSheets)
	}
	if confirmedSheetsPrinted == totalSheets {
		return nil, errors.New("generate continuation: all sheets already printed")
	}

	// Verify input file exists
	inStat, err := os.Stat(inputPDF)
	if err != nil {
		return nil, fmt.Errorf("generate continuation: stat input PDF: %w", err)
	}
	if !inStat.Mode().IsRegular() {
		return nil, fmt.Errorf("generate continuation: input PDF is not a regular file")
	}

	// 2. Determine actual page count of canonical input PDF using qpdf
	cmdPages := exec.CommandContext(ctx, "qpdf", "--show-npages", inputPDF)
	outPages, err := cmdPages.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("generate continuation: count input PDF pages with qpdf: %s: %w", strings.TrimSpace(string(outPages)), err)
	}
	actualPDFPages, err := strconv.Atoi(strings.TrimSpace(string(outPages)))
	if err != nil || actualPDFPages <= 0 {
		return nil, fmt.Errorf("generate continuation: invalid input PDF page count %q: %w", strings.TrimSpace(string(outPages)), err)
	}

	// 3. Physical sheets per copy
	sheetsPerCopy := totalSheets / copies
	if sheetsPerCopy <= 0 {
		return nil, fmt.Errorf("generate continuation: invalid sheetsPerCopy %d", sheetsPerCopy)
	}

	// For duplex mode, verify canonical PDF has an even page count
	isDuplex := (duplex != contracts.DuplexSimplex)
	if isDuplex {
		if actualPDFPages%2 != 0 {
			return nil, fmt.Errorf("generate continuation: duplex canonical PDF has odd page count %d (expected even)", actualPDFPages)
		}
		expectedPagesPerCopy := sheetsPerCopy * 2
		if actualPDFPages != expectedPagesPerCopy {
			return nil, fmt.Errorf("generate continuation: duplex canonical page count mismatch: expected %d, got %d", expectedPagesPerCopy, actualPDFPages)
		}
	} else {
		if actualPDFPages != sheetsPerCopy {
			return nil, fmt.Errorf("generate continuation: simplex canonical page count mismatch: expected %d, got %d", sheetsPerCopy, actualPDFPages)
		}
	}

	// 4. Calculate copy split and remaining sheets
	remainingSheets := totalSheets - confirmedSheetsPrinted
	copiesCompleted := confirmedSheetsPrinted / sheetsPerCopy
	sheetsInPartialCopy := confirmedSheetsPrinted % sheetsPerCopy

	// 5. Construct qpdf page range arguments
	qpdfArgs := []string{"--empty", "--pages"}

	// Partial copy (if jam/interruption occurred in the middle of a copy)
	if sheetsInPartialCopy > 0 {
		startSheet := sheetsInPartialCopy + 1
		endSheet := sheetsPerCopy

		var startPage, endPage int
		if isDuplex {
			startPage = 2*startSheet - 1
			endPage = 2 * endSheet
		} else {
			startPage = startSheet
			endPage = endSheet
		}
		rangeStr := fmt.Sprintf("%d-%d", startPage, endPage)
		qpdfArgs = append(qpdfArgs, inputPDF, rangeStr)
	}

	// Remaining full copies
	remainingFullCopies := copies - copiesCompleted
	if sheetsInPartialCopy > 0 {
		remainingFullCopies--
	}
	for i := 0; i < remainingFullCopies; i++ {
		fullRange := fmt.Sprintf("1-%d", actualPDFPages)
		qpdfArgs = append(qpdfArgs, inputPDF, fullRange)
	}

	qpdfArgs = append(qpdfArgs, "--", outputPDF)

	// 6. Execute qpdf to generate continuation PDF
	cmdQpdf := exec.CommandContext(ctx, "qpdf", qpdfArgs...)
	outQpdf, err := cmdQpdf.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("generate continuation: qpdf execution failed: %s: %w", strings.TrimSpace(string(outQpdf)), err)
	}

	// 7. Verify output structure using qpdf --check
	cmdCheck := exec.CommandContext(ctx, "qpdf", "--check", outputPDF)
	outCheck, err := cmdCheck.CombinedOutput()
	if err != nil {
		_ = os.Remove(outputPDF)
		return nil, fmt.Errorf("generate continuation: output verification failed: %s: %w", strings.TrimSpace(string(outCheck)), err)
	}

	// 8. Verify output page count using qpdf --show-npages
	cmdCheckPages := exec.CommandContext(ctx, "qpdf", "--show-npages", outputPDF)
	outCheckPages, err := cmdCheckPages.CombinedOutput()
	if err != nil {
		_ = os.Remove(outputPDF)
		return nil, fmt.Errorf("generate continuation: count output pages failed: %s: %w", strings.TrimSpace(string(outCheckPages)), err)
	}
	actualContinuationPages, err := strconv.Atoi(strings.TrimSpace(string(outCheckPages)))
	if err != nil || actualContinuationPages <= 0 {
		_ = os.Remove(outputPDF)
		return nil, fmt.Errorf("generate continuation: invalid output page count %q: %w", strings.TrimSpace(string(outCheckPages)), err)
	}

	var expectedContinuationPages int
	if isDuplex {
		expectedContinuationPages = remainingSheets * 2
	} else {
		expectedContinuationPages = remainingSheets
	}

	if actualContinuationPages != expectedContinuationPages {
		_ = os.Remove(outputPDF)
		return nil, fmt.Errorf("generate continuation: output page count mismatch: expected %d, got %d", expectedContinuationPages, actualContinuationPages)
	}

	// 9. Compute SHA256 checksum
	f, err := os.Open(outputPDF)
	if err != nil {
		return nil, fmt.Errorf("generate continuation: open output for hashing: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return nil, fmt.Errorf("generate continuation: compute sha256: %w", err)
	}
	checksumHex := hex.EncodeToString(hasher.Sum(nil))

	return &ContinuationResult{
		RemainingPages: actualContinuationPages,
		RemainingSheets: remainingSheets,
		SHA256Checksum:  checksumHex,
		OutputPath:      outputPDF,
	}, nil
}
