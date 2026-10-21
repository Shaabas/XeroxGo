package pdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xeroxgo/pkg/contracts"
)

func generateThreePagePDF(t *testing.T, dir string) string {
	t.Helper()

	psPath := filepath.Join(dir, "fixture_input.ps")
	pdfPath := filepath.Join(dir, "fixture_input.pdf")

	psContent := `%!PS
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 1 of XeroxGo Fixture Document) show
showpage

/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 2 of XeroxGo Fixture Document) show
showpage

/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 3 of XeroxGo Fixture Document) show
showpage
`
	if err := os.WriteFile(psPath, []byte(psContent), 0600); err != nil {
		t.Fatalf("failed to write PostScript fixture: %v", err)
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		psPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate 3-page fixture PDF with Ghostscript: %s: %v", strings.TrimSpace(string(out)), err)
	}

	return pdfPath
}

func verifyChecksum(t *testing.T, filePath, reportedChecksum string) {
	t.Helper()

	if len(reportedChecksum) != 64 {
		t.Fatalf("expected 64-character checksum, got %d chars: %s", len(reportedChecksum), reportedChecksum)
	}
	if reportedChecksum != strings.ToLower(reportedChecksum) {
		t.Fatalf("expected lowercase checksum, got %s", reportedChecksum)
	}
	if _, err := hex.DecodeString(reportedChecksum); err != nil {
		t.Fatalf("checksum is not valid hex: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file for checksum verification: %v", err)
	}
	hash := sha256.Sum256(data)
	expectedHex := hex.EncodeToString(hash[:])
	if reportedChecksum != expectedHex {
		t.Fatalf("checksum mismatch: reported %s, expected %s", reportedChecksum, expectedHex)
	}
}

func TestNormalizeAndCount_Simplex(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "normalized_simplex.pdf")

	ctx := context.Background()
	res, err := NormalizeAndCount(ctx, inputPDF, outputPDF, 2, contracts.DuplexSimplex)
	if err != nil {
		t.Fatalf("NormalizeAndCount failed: %v", err)
	}

	if res.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3", res.TotalPages)
	}
	if res.TotalSheets != 6 {
		t.Errorf("TotalSheets = %d, want 6", res.TotalSheets)
	}

	outInfo, err := os.Stat(outputPDF)
	if err != nil {
		t.Fatalf("normalized output file stat failed: %v", err)
	}
	if outInfo.Size() == 0 {
		t.Fatalf("normalized output file is empty")
	}

	verifyChecksum(t, outputPDF, res.SHA256Checksum)
}

func TestNormalizeAndCount_Duplex(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)

	tests := []struct {
		name        string
		duplex      contracts.DuplexMode
		copies      int
		wantPages   int
		wantSheets  int
		outFileName string
	}{
		{
			name:        "duplex_long_edge_2_copies",
			duplex:      contracts.DuplexLongEdge,
			copies:      2,
			wantPages:   3,
			wantSheets:  4,
			outFileName: "normalized_duplex_long.pdf",
		},
		{
			name:        "duplex_short_edge_1_copy",
			duplex:      contracts.DuplexShortEdge,
			copies:      1,
			wantPages:   3,
			wantSheets:  2,
			outFileName: "normalized_duplex_short.pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputPDF := filepath.Join(tmpDir, tt.outFileName)
			ctx := context.Background()

			res, err := NormalizeAndCount(ctx, inputPDF, outputPDF, tt.copies, tt.duplex)
			if err != nil {
				t.Fatalf("NormalizeAndCount failed: %v", err)
			}

			if res.TotalPages != tt.wantPages {
				t.Errorf("TotalPages = %d, want %d", res.TotalPages, tt.wantPages)
			}
			if res.TotalSheets != tt.wantSheets {
				t.Errorf("TotalSheets = %d, want %d", res.TotalSheets, tt.wantSheets)
			}

			outInfo, err := os.Stat(outputPDF)
			if err != nil {
				t.Fatalf("output file stat failed: %v", err)
			}
			if outInfo.Size() == 0 {
				t.Fatalf("output file is empty")
			}

			verifyChecksum(t, outputPDF, res.SHA256Checksum)
		})
	}
}

func TestNormalizeAndCount_RejectsCorruptPDF(t *testing.T) {
	tmpDir := t.TempDir()
	corruptPDF := filepath.Join(tmpDir, "corrupt.pdf")
	outputPDF := filepath.Join(tmpDir, "output.pdf")

	if err := os.WriteFile(corruptPDF, []byte("%PDF-1.7 corrupt invalid data content"), 0600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	ctx := context.Background()
	_, err := NormalizeAndCount(ctx, corruptPDF, outputPDF, 1, contracts.DuplexSimplex)
	if err == nil {
		t.Fatalf("NormalizeAndCount expected error on corrupt PDF, got nil")
	}
}

func TestNormalizeAndCount_RejectsEncryptedPDF(t *testing.T) {
	tmpDir := t.TempDir()
	validPDF := generateThreePagePDF(t, tmpDir)
	encryptedPDF := filepath.Join(tmpDir, "encrypted.pdf")
	outputPDF := filepath.Join(tmpDir, "output.pdf")

	cmd := exec.Command("qpdf", "--encrypt", "user-password", "owner-password", "256", "--", validPDF, encryptedPDF)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate encrypted fixture PDF with qpdf: %s: %v", strings.TrimSpace(string(out)), err)
	}

	ctx := context.Background()
	_, err = NormalizeAndCount(ctx, encryptedPDF, outputPDF, 1, contracts.DuplexSimplex)
	if err == nil {
		t.Fatalf("NormalizeAndCount expected error on encrypted PDF, got nil")
	}
}

func TestNormalizeAndCount_RejectsInvalidArguments(t *testing.T) {
	tmpDir := t.TempDir()
	validPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "output.pdf")
	ctx := context.Background()

	tests := []struct {
		name       string
		ctx        context.Context
		inputPath  string
		outputPath string
		copies     int
		duplex     contracts.DuplexMode
	}{
		{
			name:       "nil_context",
			ctx:        nil,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "zero_copies",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     0,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "negative_copies",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     -1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "unknown_duplex_mode",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexMode("TRI_FOLD"),
		},
		{
			name:       "empty_input_path",
			ctx:        ctx,
			inputPath:  "",
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "empty_output_path",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: "",
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "same_input_and_output",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: validPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "nonexistent_input",
			ctx:        ctx,
			inputPath:  filepath.Join(tmpDir, "nonexistent.pdf"),
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "directory_as_input",
			ctx:        ctx,
			inputPath:  tmpDir,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeAndCount(tt.ctx, tt.inputPath, tt.outputPath, tt.copies, tt.duplex)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", tt.name)
			}
		})
	}
}

func TestNormalizeAndCount_RejectsCanceledContext(t *testing.T) {
	tmpDir := t.TempDir()
	validPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "output.pdf")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel context before calling

	_, err := NormalizeAndCount(ctx, validPDF, outputPDF, 1, contracts.DuplexSimplex)
	if err == nil {
		t.Fatalf("NormalizeAndCount expected error on canceled context, got nil")
	}
}
