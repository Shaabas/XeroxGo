package ui_ipc

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	ScreenWidth  = 1920
	ScreenHeight = 1080
)

type ScreenMode string

const (
	ModeIdle        ScreenMode = "IDLE"
	ModeDownloading ScreenMode = "DOWNLOADING"
	ModePrinting    ScreenMode = "PRINTING"
	ModeCompleted   ScreenMode = "COMPLETED"
	ModeError       ScreenMode = "ERROR"
)

type ScreenState struct {
	Mode            ScreenMode
	QRContent       string
	Message         string
	Detail          string
	ProgressPercent int
	CurrentPage     int
	TotalPages      int
}

type Sink func(*image.RGBA) error

var (
	colorBgDark        = color.RGBA{R: 15, G: 23, B: 42, A: 255}    // #0f172a
	colorHeaderBg      = color.RGBA{R: 15, G: 23, B: 42, A: 255}    // #0f172a
	colorCardBg        = color.RGBA{R: 30, G: 41, B: 59, A: 255}    // #1e293b
	colorBorder        = color.RGBA{R: 51, G: 65, B: 85, A: 255}    // #334155
	colorAccentBlue    = color.RGBA{R: 56, G: 189, B: 248, A: 255}  // #38bdf8
	colorTextWhite     = color.RGBA{R: 248, G: 250, B: 252, A: 255} // #f8fafc
	colorTextMuted     = color.RGBA{R: 148, G: 163, B: 184, A: 255} // #94a3b8
	colorSuccessGreen  = color.RGBA{R: 34, G: 197, B: 94, A: 255}   // #22c55e
	colorErrorRed      = color.RGBA{R: 239, G: 68, B: 68, A: 255}   // #ef4444
	colorWarningOrange = color.RGBA{R: 249, G: 115, B: 22, A: 255}  // #f97316
	colorBarBg         = color.RGBA{R: 51, G: 65, B: 85, A: 255}    // #334155
)

// Render renders a 1920x1080 RGBA dashboard image for the given state.
func Render(state ScreenState) (*image.RGBA, error) {
	switch state.Mode {
	case ModeIdle:
		if strings.TrimSpace(state.QRContent) == "" {
			return nil, fmt.Errorf("idle mode: QRContent cannot be empty or whitespace-only")
		}
	case ModeDownloading:
		if state.ProgressPercent < 0 || state.ProgressPercent > 100 {
			return nil, fmt.Errorf("downloading mode: ProgressPercent must be between 0 and 100, got %d", state.ProgressPercent)
		}
	case ModePrinting:
		if state.TotalPages < 1 {
			return nil, fmt.Errorf("printing mode: TotalPages must be >= 1, got %d", state.TotalPages)
		}
		if state.CurrentPage < 0 || state.CurrentPage > state.TotalPages {
			return nil, fmt.Errorf("printing mode: CurrentPage must be between 0 and TotalPages (%d), got %d", state.TotalPages, state.CurrentPage)
		}
	case ModeCompleted:
		// Valid
	case ModeError:
		// Valid
	default:
		return nil, fmt.Errorf("unsupported screen mode: %q", state.Mode)
	}

	canvas := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))

	// 1. Base dark background
	fillRect(canvas, canvas.Bounds(), colorBgDark)

	// 2. Top header bar (0 to 110)
	fillRect(canvas, image.Rect(0, 0, ScreenWidth, 110), colorHeaderBg)
	fillRect(canvas, image.Rect(0, 108, ScreenWidth, 110), colorBorder)

	// Brand & subhead
	drawTextScaled(canvas, "XEROXGO", 80, 36, 4, colorAccentBlue)
	drawTextScaled(canvas, "CAMPUS PRINT KIOSK", 440, 48, 2, colorTextMuted)

	// Header status badge
	drawHeaderBadge(canvas, state.Mode)

	// 3. Main content card (80 to 1840, 140 to 980)
	cardRect := image.Rect(80, 140, 1840, 980)
	fillRect(canvas, cardRect, colorCardBg)
	strokeRect(canvas, cardRect, 2, colorBorder)

	// 4. Footer bar (1010 to 1080)
	fillRect(canvas, image.Rect(0, 1010, ScreenWidth, ScreenHeight), colorHeaderBg)
	fillRect(canvas, image.Rect(0, 1010, ScreenWidth, 1012), colorBorder)
	drawTextScaled(canvas, "XeroxGo Kiosk Daemon • Touchless Mobile Printing • Brother HL-L5210DN", 80, 1035, 2, colorTextMuted)

	// 5. Render mode-specific contents
	switch state.Mode {
	case ModeIdle:
		if err := renderIdle(canvas, state); err != nil {
			return nil, err
		}
	case ModeDownloading:
		renderDownloading(canvas, state)
	case ModePrinting:
		renderPrinting(canvas, state)
	case ModeCompleted:
		renderCompleted(canvas, state)
	case ModeError:
		renderError(canvas, state)
	}

	return canvas, nil
}

func drawHeaderBadge(dst *image.RGBA, mode ScreenMode) {
	badgeRect := image.Rect(1540, 30, 1840, 80)
	var bgCol color.RGBA
	var textCol color.RGBA
	var label string

	switch mode {
	case ModeIdle:
		bgCol = color.RGBA{R: 30, G: 41, B: 59, A: 255}
		textCol = colorSuccessGreen
		label = "ONLINE READY"
	case ModeDownloading:
		bgCol = color.RGBA{R: 3, G: 105, B: 161, A: 255}
		textCol = colorTextWhite
		label = "DOWNLOADING"
	case ModePrinting:
		bgCol = color.RGBA{R: 3, G: 105, B: 161, A: 255}
		textCol = colorTextWhite
		label = "PRINTING"
	case ModeCompleted:
		bgCol = color.RGBA{R: 21, G: 128, B: 61, A: 255}
		textCol = colorTextWhite
		label = "COMPLETE"
	case ModeError:
		bgCol = color.RGBA{R: 185, G: 28, B: 28, A: 255}
		textCol = colorTextWhite
		label = "ATTENTION"
	}

	fillRect(dst, badgeRect, bgCol)
	strokeRect(dst, badgeRect, 1, colorBorder)
	textWidth := len(label) * 7 * 2
	startX := badgeRect.Min.X + (badgeRect.Dx()-textWidth)/2
	startY := badgeRect.Min.Y + (badgeRect.Dy()-13*2)/2
	drawTextScaled(dst, label, startX, startY, 2, textCol)
}

func renderIdle(dst *image.RGBA, state ScreenState) error {
	// Left pane: Instructions
	drawTextScaled(dst, "SCAN TO PRINT", 140, 220, 5, colorTextWhite)
	drawTextScaled(dst, "Use your phone camera or XeroxGo app to begin", 140, 300, 2, colorTextMuted)

	drawTextScaled(dst, "1. Open camera or QR scanner on your phone", 140, 380, 2, colorTextWhite)
	drawTextScaled(dst, "2. Scan the QR code shown on the right", 140, 440, 2, colorTextWhite)
	drawTextScaled(dst, "3. Upload your PDF and select print options", 140, 500, 2, colorTextWhite)
	drawTextScaled(dst, "4. Pay securely and collect your documents", 140, 560, 2, colorTextWhite)

	// Readiness indicator box
	statusBox := image.Rect(140, 680, 800, 750)
	fillRect(dst, statusBox, colorBgDark)
	strokeRect(dst, statusBox, 1, colorSuccessGreen)
	drawTextScaled(dst, "PRINTER READY -- HIGH SPEED DUPLEX A4", 170, 705, 2, colorSuccessGreen)

	// Right pane: QR Code
	qrBytes, err := qrcode.Encode(state.QRContent, qrcode.Medium, 400)
	if err != nil {
		return fmt.Errorf("failed to encode QR code: %w", err)
	}
	qrImg, err := png.Decode(bytes.NewReader(qrBytes))
	if err != nil {
		return fmt.Errorf("failed to decode QR PNG: %w", err)
	}

	// White background backing for scannability
	qrBacking := image.Rect(1216, 266, 1664, 714)
	fillRect(dst, qrBacking, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	strokeRect(dst, qrBacking, 2, color.RGBA{R: 203, G: 213, B: 225, A: 255})

	// Draw decoded QR image at (1240, 290)
	draw.Draw(dst, image.Rect(1240, 290, 1640, 690), qrImg, image.Point{}, draw.Src)

	// Label under QR
	label := "POINT PHONE CAMERA AT CODE"
	labelWidth := len(label) * 7 * 2
	labelX := qrBacking.Min.X + (qrBacking.Dx()-labelWidth)/2
	drawTextScaled(dst, label, labelX, 740, 2, colorTextWhite)

	return nil
}

func renderDownloading(dst *image.RGBA, state ScreenState) {
	drawTextScaled(dst, "DOWNLOADING DOCUMENT", 140, 240, 5, colorAccentBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Receiving print job from cloud server..."
	}
	drawTextScaled(dst, msg, 140, 340, 3, colorTextWhite)

	if strings.TrimSpace(state.Detail) != "" {
		drawTextScaled(dst, state.Detail, 140, 410, 2, colorTextMuted)
	}

	// Progress Bar
	barX := 140
	barY := 500
	barW := 1560
	barH := 50
	drawProgressBar(dst, barX, barY, barW, barH, state.ProgressPercent, 100, colorAccentBlue)

	pctText := fmt.Sprintf("Download Progress: %d%%", state.ProgressPercent)
	drawTextScaled(dst, pctText, 140, 580, 3, colorTextWhite)

	drawTextScaled(dst, "Secure encrypted download in progress. Printing will begin automatically.", 140, 680, 2, colorTextMuted)
}

func renderPrinting(dst *image.RGBA, state ScreenState) {
	drawTextScaled(dst, "PRINTING DOCUMENT", 140, 240, 5, colorAccentBlue)

	counterText := fmt.Sprintf("PAGE %d OF %d", state.CurrentPage, state.TotalPages)
	drawTextScaled(dst, counterText, 140, 330, 4, colorTextWhite)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Please wait while your document prints..."
	}
	drawTextScaled(dst, msg, 140, 410, 3, colorTextWhite)

	if strings.TrimSpace(state.Detail) != "" {
		drawTextScaled(dst, state.Detail, 140, 465, 2, colorTextMuted)
	}

	// Progress Bar
	barX := 140
	barY := 520
	barW := 1560
	barH := 50
	drawProgressBar(dst, barX, barY, barW, barH, state.CurrentPage, state.TotalPages, colorAccentBlue)

	subProgress := fmt.Sprintf("Progress: %d of %d pages streamed to printer hardware", state.CurrentPage, state.TotalPages)
	drawTextScaled(dst, subProgress, 140, 595, 2, colorTextWhite)

	drawTextScaled(dst, "Job is active on Brother HL-L5210DN. Pages will exit into top tray.", 140, 680, 2, colorTextMuted)
}

func renderCompleted(dst *image.RGBA, state ScreenState) {
	drawTextScaled(dst, "PRINT COMPLETE", 140, 240, 5, colorSuccessGreen)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Your print job has finished successfully."
	}
	drawTextScaled(dst, msg, 140, 340, 3, colorTextWhite)

	drawTextScaled(dst, "PLEASE COLLECT ALL PRINTED PAGES FROM THE OUTPUT TRAY", 140, 420, 3, colorSuccessGreen)

	if strings.TrimSpace(state.Detail) != "" {
		drawTextScaled(dst, state.Detail, 140, 485, 2, colorTextMuted)
	}

	drawTextScaled(dst, "1. Inspect all printed pages before leaving the kiosk area.", 140, 560, 2, colorTextWhite)
	drawTextScaled(dst, "2. Kiosk will reset to the idle screen for the next user.", 140, 610, 2, colorTextWhite)
	drawTextScaled(dst, "Thank you for printing with XeroxGo!", 140, 690, 2, colorTextMuted)
}

func renderError(dst *image.RGBA, state ScreenState) {
	drawTextScaled(dst, "PRINTER ATTENTION REQUIRED", 140, 210, 5, colorErrorRed)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "An error occurred during print processing."
	}
	msgLines := wrapText(msg, 60)
	y := 295
	for i, line := range msgLines {
		if i >= 2 {
			break
		}
		drawTextScaled(dst, line, 140, y, 3, colorTextWhite)
		y += 40
	}

	if strings.TrimSpace(state.Detail) != "" {
		detailLines := wrapText(state.Detail, 80)
		for i, line := range detailLines {
			if i >= 2 {
				break
			}
			drawTextScaled(dst, line, 140, y, 2, colorTextMuted)
			y += 28
		}
	}

	// Hardware recovery instructions panel
	boxRect := image.Rect(140, 480, 1700, 850)
	fillRect(dst, boxRect, colorBgDark)
	strokeRect(dst, boxRect, 2, colorWarningOrange)

	drawTextScaled(dst, "ACTIONABLE HARDWARE RECOVERY INSTRUCTIONS:", 180, 510, 2, colorWarningOrange)
	drawTextScaled(dst, "1. Check paper tray 1 and refill standard A4 paper if depleted.", 180, 565, 2, colorTextWhite)
	drawTextScaled(dst, "2. If paper is jammed, follow printer front panel steps to clear and re-seat covers.", 180, 620, 2, colorTextWhite)
	drawTextScaled(dst, "3. Verify that the output area and paper exit tray are completely clear.", 180, 675, 2, colorTextWhite)
	drawTextScaled(dst, "4. If error persists, please notify the campus attendant or support.", 180, 730, 2, colorTextWhite)
}

func progressBarFillWidth(innerBarWidth, current, total int) int {
	if innerBarWidth <= 0 || total <= 0 || current <= 0 {
		return 0
	}
	if current >= total {
		return innerBarWidth
	}
	return (innerBarWidth * current) / total
}

func drawProgressBar(dst *image.RGBA, x, y, width, height, current, total int, fillColor color.RGBA) {
	outerRect := image.Rect(x, y, x+width, y+height)
	fillRect(dst, outerRect, colorBarBg)
	strokeRect(dst, outerRect, 2, colorBorder)

	innerW := width - 4
	innerH := height - 4
	fillW := progressBarFillWidth(innerW, current, total)
	if fillW > 0 {
		fillRect(dst, image.Rect(x+2, y+2, x+2+fillW, y+2+innerH), fillColor)
	}
}

func fillRect(dst *image.RGBA, r image.Rectangle, col color.RGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		rowStart := y*dst.Stride + r.Min.X*4
		rowEnd := y*dst.Stride + r.Max.X*4
		for p := rowStart; p < rowEnd; p += 4 {
			dst.Pix[p+0] = col.R
			dst.Pix[p+1] = col.G
			dst.Pix[p+2] = col.B
			dst.Pix[p+3] = col.A
		}
	}
}

func strokeRect(dst *image.RGBA, r image.Rectangle, thickness int, col color.RGBA) {
	if thickness <= 0 {
		return
	}
	fillRect(dst, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+thickness), col)
	fillRect(dst, image.Rect(r.Min.X, r.Max.Y-thickness, r.Max.X, r.Max.Y), col)
	fillRect(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+thickness, r.Max.Y), col)
	fillRect(dst, image.Rect(r.Max.X-thickness, r.Min.Y, r.Max.X, r.Max.Y), col)
}

func drawTextScaled(dst *image.RGBA, text string, startX, startY, scale int, col color.RGBA) {
	if scale <= 0 {
		scale = 1
	}
	d := &font.Drawer{
		Face: basicfont.Face7x13,
	}
	textWidth := d.MeasureString(text).Ceil()
	if textWidth <= 0 {
		return
	}
	textHeight := 13

	mask := image.NewAlpha(image.Rect(0, 0, textWidth, textHeight))
	d.Dst = mask
	d.Src = image.Opaque
	d.Dot = fixed.Point26_6{X: 0, Y: fixed.I(11)} // 11 is baseline ascent
	d.DrawString(text)

	for my := 0; my < textHeight; my++ {
		dyStart := startY + my*scale
		for mx := 0; mx < textWidth; mx++ {
			if mask.AlphaAt(mx, my).A == 0 {
				continue
			}
			dxStart := startX + mx*scale
			fillRect(dst, image.Rect(dxStart, dyStart, dxStart+scale, dyStart+scale), col)
		}
	}
}

func wrapText(text string, maxCharsPerLine int) []string {
	if len(text) == 0 {
		return nil
	}
	if maxCharsPerLine <= 0 {
		maxCharsPerLine = 40
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var lines []string
	var cur strings.Builder

	for _, word := range words {
		for len(word) > maxCharsPerLine {
			if cur.Len() > 0 {
				lines = append(lines, cur.String())
				cur.Reset()
			}
			lines = append(lines, word[:maxCharsPerLine])
			word = word[maxCharsPerLine:]
		}
		if word == "" {
			continue
		}
		if cur.Len() == 0 {
			cur.WriteString(word)
		} else if cur.Len()+1+len(word) <= maxCharsPerLine {
			cur.WriteByte(' ')
			cur.WriteString(word)
		} else {
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(word)
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

// FilePreviewSink atomically writes a 1920x1080 RGBA image to pngPath via a temporary file.
func FilePreviewSink(pngPath string) Sink {
	return func(img *image.RGBA) error {
		if strings.TrimSpace(pngPath) == "" {
			return fmt.Errorf("pngPath cannot be empty")
		}
		if img == nil {
			return fmt.Errorf("image cannot be nil")
		}

		dir := filepath.Dir(pngPath)
		dirStat, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("destination directory does not exist: %w", err)
		}
		if !dirStat.IsDir() {
			return fmt.Errorf("destination parent path is not a directory")
		}

		tmpFile, err := os.CreateTemp(dir, "preview_*.png.tmp")
		if err != nil {
			return fmt.Errorf("failed to create temporary preview file: %w", err)
		}
		tmpName := tmpFile.Name()

		cleanup := true
		defer func() {
			if cleanup {
				_ = tmpFile.Close()
				_ = os.Remove(tmpName)
			}
		}()

		if err := png.Encode(tmpFile, img); err != nil {
			return fmt.Errorf("failed to encode PNG: %w", err)
		}

		if err := tmpFile.Sync(); err != nil {
			return fmt.Errorf("failed to sync temporary preview file: %w", err)
		}

		if err := tmpFile.Close(); err != nil {
			return fmt.Errorf("failed to close temporary preview file: %w", err)
		}

		if err := os.Rename(tmpName, pngPath); err != nil {
			return fmt.Errorf("failed to atomically rename preview file: %w", err)
		}

		cleanup = false
		return nil
	}
}

// FramebufferSink streams a 1920x1080 RGBA image as 32-bit BGRA directly to an existing fb device/file.
func FramebufferSink(fbPath string) Sink {
	return func(img *image.RGBA) (returnErr error) {
		if strings.TrimSpace(fbPath) == "" {
			return fmt.Errorf("fbPath cannot be empty")
		}
		if img == nil {
			return fmt.Errorf("image cannot be nil")
		}
		bounds := img.Bounds()
		if bounds.Dx() != ScreenWidth || bounds.Dy() != ScreenHeight {
			return fmt.Errorf("invalid image dimensions: expected %dx%d, got %dx%d", ScreenWidth, ScreenHeight, bounds.Dx(), bounds.Dy())
		}

		f, err := os.OpenFile(fbPath, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("failed to open framebuffer: %w", err)
		}
		defer func() {
			closeErr := f.Close()
			if returnErr == nil && closeErr != nil {
				returnErr = fmt.Errorf("failed to close framebuffer: %w", closeErr)
			}
		}()

		rowBuf := make([]byte, ScreenWidth*4)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			rowPix := img.Pix[(y-bounds.Min.Y)*img.Stride+bounds.Min.X*4 : (y-bounds.Min.Y)*img.Stride+bounds.Min.X*4+ScreenWidth*4]
			for x := 0; x < ScreenWidth; x++ {
				inIdx := x * 4
				outIdx := x * 4

				rowBuf[outIdx+0] = rowPix[inIdx+2] // B
				rowBuf[outIdx+1] = rowPix[inIdx+1] // G
				rowBuf[outIdx+2] = rowPix[inIdx+0] // R
				rowBuf[outIdx+3] = rowPix[inIdx+3] // A
			}

			written := 0
			for written < len(rowBuf) {
				n, err := f.Write(rowBuf[written:])
				if err != nil {
					return fmt.Errorf("failed to write row %d to framebuffer: %w", y, err)
				}
				if n == 0 {
					return fmt.Errorf("short write on row %d to framebuffer", y)
				}
				written += n
			}
		}

		return nil
	}
}
