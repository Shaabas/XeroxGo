package pdf_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xeroxgo/cloud-backend/internal/pdf"
	"xeroxgo/pkg/contracts"
)

// generateTestCanonicalPDF creates an N-page test PDF using ghostscript.
func generateTestCanonicalPDF(t *testing.T, dir string, numPages int) string {
	t.Helper()
	outPath := filepath.Join(dir, fmt.Sprintf("test_doc_%d.pdf", numPages))

	var gsCommands strings.Builder
	for i := 1; i <= numPages; i++ {
		gsCommands.WriteString(fmt.Sprintf("0 0 0 setrgbcolor /Helvetica findfont 36 scalefont setfont 100 500 moveto (Page %d) show showpage ", i))
	}

	args := []string{
		"-dNOPAUSE", "-dBATCH", "-sDEVICE=pdfwrite",
		"-sOutputFile=" + outPath,
		"-c", gsCommands.String(),
	}
	cmd := exec.Command("gs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to create test canonical PDF: %s: %v", strings.TrimSpace(string(out)), err)
	}
	return outPath
}

func TestGenerateContinuationPDF_Simplex(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 5-page document, 1 copy => 5 sheets
	docPath := generateTestCanonicalPDF(t, tmpDir, 5)

	t.Run("resume after 2 sheets printed (mid-job)", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_simp_2.pdf")
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			5, 5, 1, contracts.DuplexSimplex, 2,
		)
		if err != nil {
			t.Fatalf("GenerateContinuationPDF failed: %v", err)
		}
		if res.RemainingSheets != 3 {
			t.Fatalf("expected 3 remaining sheets, got %d", res.RemainingSheets)
		}
		if res.RemainingPages != 3 {
			t.Fatalf("expected 3 remaining pages, got %d", res.RemainingPages)
		}
		if res.SHA256Checksum == "" {
			t.Fatal("expected non-empty checksum")
		}

		// Verify page count of output
		cmd := exec.Command("qpdf", "--show-npages", outPath)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "3" {
			t.Fatalf("expected 3 pages in output PDF, got %s (err: %v)", string(out), err)
		}
	})

	t.Run("full reprint from start (0 sheets printed)", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_simp_0.pdf")
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			5, 5, 1, contracts.DuplexSimplex, 0,
		)
		if err != nil {
			t.Fatalf("GenerateContinuationPDF failed: %v", err)
		}
		if res.RemainingSheets != 5 || res.RemainingPages != 5 {
			t.Fatalf("expected 5 sheets/pages, got %d/%d", res.RemainingSheets, res.RemainingPages)
		}
	})

	t.Run("all sheets already printed returns error", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_simp_all.pdf")
		_, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			5, 5, 1, contracts.DuplexSimplex, 5,
		)
		if err == nil {
			t.Fatal("expected error when all sheets already printed")
		}
	})
}

func TestGenerateContinuationPDF_Duplex(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 6-page canonical document (3 duplex sheets), 1 copy
	docPath := generateTestCanonicalPDF(t, tmpDir, 6)

	t.Run("resume after 1 sheet printed", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_dup_1.pdf")
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			6, 3, 1, contracts.DuplexLongEdge, 1,
		)
		if err != nil {
			t.Fatalf("GenerateContinuationPDF failed: %v", err)
		}
		if res.RemainingSheets != 2 {
			t.Fatalf("expected 2 remaining sheets, got %d", res.RemainingSheets)
		}
		if res.RemainingPages != 4 {
			t.Fatalf("expected 4 remaining pages (2 sheets * 2), got %d", res.RemainingPages)
		}

		// Verify page count of output
		cmd := exec.Command("qpdf", "--show-npages", outPath)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "4" {
			t.Fatalf("expected 4 pages in output PDF, got %s (err: %v)", string(out), err)
		}
	})

	t.Run("resume after 2 sheets printed (last sheet remaining)", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_dup_2.pdf")
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			6, 3, 1, contracts.DuplexLongEdge, 2,
		)
		if err != nil {
			t.Fatalf("GenerateContinuationPDF failed: %v", err)
		}
		if res.RemainingSheets != 1 {
			t.Fatalf("expected 1 remaining sheet, got %d", res.RemainingSheets)
		}
		if res.RemainingPages != 2 {
			t.Fatalf("expected 2 remaining pages, got %d", res.RemainingPages)
		}
	})
}

func TestGenerateContinuationPDF_MultiCopy(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 4-page duplex document (2 sheets per copy), 3 copies => total 6 sheets
	docPath := generateTestCanonicalPDF(t, tmpDir, 4)

	t.Run("interrupted during copy 1 on sheet 1 (1 sheet printed)", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_multi_1.pdf")
		// 1 sheet printed: remaining is sheet 2 of copy 1 + 2 full copies (sheets 1-2, 1-2)
		// Total remaining sheets: 6 - 1 = 5 sheets = 10 pages
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			4, 6, 3, contracts.DuplexLongEdge, 1,
		)
		if err != nil {
			t.Fatalf("GenerateContinuationPDF failed: %v", err)
		}
		if res.RemainingSheets != 5 {
			t.Fatalf("expected 5 remaining sheets, got %d", res.RemainingSheets)
		}
		if res.RemainingPages != 10 {
			t.Fatalf("expected 10 remaining pages, got %d", res.RemainingPages)
		}

		cmd := exec.Command("qpdf", "--show-npages", outPath)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "10" {
			t.Fatalf("expected 10 pages in output, got %s", string(out))
		}
	})

	t.Run("interrupted at copy boundary (2 sheets printed = 1 full copy complete)", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_multi_2.pdf")
		// 2 sheets printed: 2 full copies remain (4 sheets = 8 pages)
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			4, 6, 3, contracts.DuplexLongEdge, 2,
		)
		if err != nil {
			t.Fatalf("GenerateContinuationPDF failed: %v", err)
		}
		if res.RemainingSheets != 4 {
			t.Fatalf("expected 4 remaining sheets, got %d", res.RemainingSheets)
		}
		if res.RemainingPages != 8 {
			t.Fatalf("expected 8 remaining pages, got %d", res.RemainingPages)
		}
	})

	t.Run("interrupted during copy 2 on sheet 1 (3 sheets printed)", func(t *testing.T) {
		outPath := filepath.Join(tmpDir, "cont_multi_3.pdf")
		// 3 sheets printed: 1 sheet of copy 2 + 1 full copy of copy 3 = 3 sheets = 6 pages
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			4, 6, 3, contracts.DuplexLongEdge, 3,
		)
		if err != nil {
			t.Fatalf("GenerateContinuationPDF failed: %v", err)
		}
		if res.RemainingSheets != 3 {
			t.Fatalf("expected 3 remaining sheets, got %d", res.RemainingSheets)
		}
		if res.RemainingPages != 6 {
			t.Fatalf("expected 6 remaining pages, got %d", res.RemainingPages)
		}
	})
}

func TestGenerateContinuationPDF_NUpModes(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// In XeroxGo, 1-up, 2-up, 4-up, and 6-up canonical PDFs are already imposed
	// into standard A4 sheets before being saved to jobsDir.
	// For example:
	// - 2-up on a 6-page doc: 3 sides => duplex padded to 4 pages (2 sheets)
	// - 4-up on a 6-page doc: 2 sides => duplex 2 pages (1 sheet)
	// - 6-up on a 6-page doc: 1 side => duplex padded to 2 pages (1 sheet)
	//
	// We test continuation slicing on each canonical output.

	t.Run("2-up duplex 4-page canonical (2 sheets), 1 sheet printed", func(t *testing.T) {
		docPath := generateTestCanonicalPDF(t, tmpDir, 4)
		outPath := filepath.Join(tmpDir, "cont_2up.pdf")
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			3, 2, 1, contracts.DuplexLongEdge, 1,
		)
		if err != nil {
			t.Fatalf("failed 2-up duplex recovery: %v", err)
		}
		if res.RemainingSheets != 1 || res.RemainingPages != 2 {
			t.Fatalf("expected 1 sheet, 2 pages; got %d sheets, %d pages", res.RemainingSheets, res.RemainingPages)
		}
	})

	t.Run("4-up duplex 2-page canonical (1 sheet), 0 sheets printed (reprint)", func(t *testing.T) {
		docPath := generateTestCanonicalPDF(t, tmpDir, 2)
		outPath := filepath.Join(tmpDir, "cont_4up.pdf")
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			2, 1, 1, contracts.DuplexLongEdge, 0,
		)
		if err != nil {
			t.Fatalf("failed 4-up duplex recovery: %v", err)
		}
		if res.RemainingSheets != 1 || res.RemainingPages != 2 {
			t.Fatalf("expected 1 sheet, 2 pages; got %d sheets, %d pages", res.RemainingSheets, res.RemainingPages)
		}
	})

	t.Run("6-up simplex 3 copies of 1-page canonical (3 sheets total), 1 sheet printed", func(t *testing.T) {
		docPath := generateTestCanonicalPDF(t, tmpDir, 1)
		outPath := filepath.Join(tmpDir, "cont_6up.pdf")
		res, err := pdf.GenerateContinuationPDF(
			ctx, docPath, outPath,
			1, 3, 3, contracts.DuplexSimplex, 1,
		)
		if err != nil {
			t.Fatalf("failed 6-up simplex recovery: %v", err)
		}
		if res.RemainingSheets != 2 || res.RemainingPages != 2 {
			t.Fatalf("expected 2 sheets, 2 pages; got %d sheets, %d pages", res.RemainingSheets, res.RemainingPages)
		}
	})
}
