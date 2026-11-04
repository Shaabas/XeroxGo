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
	"strconv"
	"strings"
	"sync"

	"github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	xdraw "golang.org/x/image/draw"
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
	PrinterModel    string
	AdSlideIndex    int // 0: Quick & Easy, 1: High Quality Prints, 2: Skip The Queue
}

type Sink func(*image.RGBA) error

var (
	// Frontend portal.css aligned palette
	colorBgSlate        = color.RGBA{R: 248, G: 250, B: 252, A: 255} // #F8FAFC
	colorCardBg         = color.RGBA{R: 255, G: 255, B: 255, A: 255} // #FFFFFF
	colorBorder         = color.RGBA{R: 226, G: 232, B: 240, A: 255} // #E2E8F0
	colorBorderDark     = color.RGBA{R: 203, G: 213, B: 225, A: 255} // #CBD5E1
	colorPrimaryBlue    = color.RGBA{R: 37, G: 99, B: 235, A: 255}   // #2563EB
	colorAccentBlue     = colorPrimaryBlue                           // Alias for tests
	colorSuccessGreen   = color.RGBA{R: 16, G: 185, B: 129, A: 255}  // #10B981
	colorWarningAmber   = color.RGBA{R: 245, G: 158, B: 11, A: 255}  // #F59E0B
	colorErrorRed       = color.RGBA{R: 239, G: 68, B: 68, A: 255}   // #EF4444
	colorTextPrimary    = color.RGBA{R: 30, G: 41, B: 59, A: 255}    // #1E293B
	colorTextSecondary  = color.RGBA{R: 100, G: 116, B: 139, A: 255} // #64748B
	colorTextWhite      = color.RGBA{R: 255, G: 255, B: 255, A: 255} // #FFFFFF
	colorBarBg          = color.RGBA{R: 226, G: 232, B: 240, A: 255} // #E2E8F0
	colorPillBlueBg     = color.RGBA{R: 239, G: 246, B: 255, A: 255} // #EFF6FF
	colorPillBlueBorder = color.RGBA{R: 191, G: 219, B: 254, A: 255} // #BFDBFE
	colorPillGreenBg    = color.RGBA{R: 236, G: 253, B: 245, A: 255} // #ECFDF5
	colorPillGreenBorder= color.RGBA{R: 167, G: 243, B: 208, A: 255} // #A7F3D0
	colorPillRedBg      = color.RGBA{R: 254, G: 242, B: 242, A: 255} // #FEF2F2
	colorPillRedBorder  = color.RGBA{R: 254, G: 202, B: 202, A: 255} // #FECACA
)

var renderMu sync.Mutex

// Render renders a 1920x1080 RGBA dashboard image for the given state.
func Render(state ScreenState) (*image.RGBA, error) {
	renderMu.Lock()
	defer renderMu.Unlock()

	initUIAssets()

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

	// 1. Base clean Slate-50 background matching portal.css
	fillRect(canvas, canvas.Bounds(), colorBgSlate)

	// 2. Top white header bar (0 to 110)
	fillRect(canvas, image.Rect(0, 0, ScreenWidth, 110), colorCardBg)
	fillRect(canvas, image.Rect(0, 108, ScreenWidth, 110), colorBorder)

	// Embedded XeroxGo full logo
	drawLogo(canvas, 80, 31, 48)

	// Subtitle beside brand logo
	drawText(canvas, faceReg18, "Campus Print Kiosk • Touchless Mobile Printing", 480, 46, colorTextSecondary)

	// 3. Footer bar (1010 to 1080)
	fillRect(canvas, image.Rect(0, 1010, ScreenWidth, ScreenHeight), colorCardBg)
	fillRect(canvas, image.Rect(0, 1010, ScreenWidth, 1012), colorBorder)

	// Clean footer: in user-edited designs, footer text is only shown during active printing
	if state.Mode == ModePrinting {
		modelStr := "Brother HL-L5210DN"
		if state.PrinterModel != "" {
			modelStr = state.PrinterModel
		}
		drawText(canvas, faceReg18, "XeroxGo Kiosk Daemon • Touchless Mobile Printing • "+modelStr, 80, 1035, colorTextSecondary)
	}

	// 4. Mode-specific content
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
	var bgCol, borderCol, textCol color.RGBA
	var label string

	switch mode {
	case ModeIdle:
		bgCol = colorPillGreenBg
		borderCol = colorPillGreenBorder
		textCol = color.RGBA{R: 4, G: 120, B: 87, A: 255}
		label = "● READY TO PRINT"
	case ModeDownloading:
		bgCol = colorPillBlueBg
		borderCol = colorPillBlueBorder
		textCol = color.RGBA{R: 29, G: 78, B: 216, A: 255}
		label = "● DOWNLOADING"
	case ModePrinting:
		bgCol = colorPillBlueBg
		borderCol = colorPillBlueBorder
		textCol = color.RGBA{R: 29, G: 78, B: 216, A: 255}
		label = "● PRINTING"
	case ModeCompleted:
		bgCol = colorPillGreenBg
		borderCol = colorPillGreenBorder
		textCol = color.RGBA{R: 4, G: 120, B: 87, A: 255}
		label = "● COMPLETE"
	case ModeError:
		bgCol = colorPillRedBg
		borderCol = colorPillRedBorder
		textCol = color.RGBA{R: 185, G: 28, B: 28, A: 255}
		label = "● ATTENTION"
	}

	drawPill(dst, badgeRect, bgCol, borderCol)
	textW := measureTextWidth(faceBold18, label)
	startX := badgeRect.Min.X + (badgeRect.Dx()-textW)/2
	startY := badgeRect.Min.Y + (badgeRect.Dy()-20)/2
	drawText(dst, faceBold18, label, startX, startY, textCol)
}

// AdSlide models a single promotional slide from the mobile frontend carousel.
type AdSlide struct {
	Title   string
	Desc    string
	Tag     string
	Subline string
	C0      color.RGBA
	C1      color.RGBA
	C2      color.RGBA
}

var adSlides = [3]AdSlide{
	{
		Title:   "Quick & Easy",
		Desc:    "Direct UPI payment. No coins, no cash needed.",
		Tag:     "TOUCHLESS MOBILE PRINTING",
		Subline: "Scan QR with any phone  •  Upload PDF  •  Instant UPI  •  Collect print",
		C0:      color.RGBA{R: 30, G: 58, B: 138, A: 255},  // #1E3A8A
		C1:      color.RGBA{R: 37, G: 99, B: 235, A: 255},  // #2563EB
		C2:      color.RGBA{R: 79, G: 70, B: 229, A: 255},  // #4F46E5
	},
	{
		Title:   "High Quality Prints",
		Desc:    "Crisp laser documents ready in seconds.",
		Tag:     "CAMPUS PRINT PORTAL",
		Subline: "Single & Double-Sided (Duplex)  •  Full Color & Monochrome  •  600 DPI",
		C0:      color.RGBA{R: 6, G: 95, B: 70, A: 255},    // #065F46
		C1:      color.RGBA{R: 16, G: 185, B: 129, A: 255}, // #10B981
		C2:      color.RGBA{R: 5, G: 150, B: 105, A: 255},  // #059669
	},
	{
		Title:   "Skip The Queue",
		Desc:    "Pick up your print job with your 4-digit code.",
		Tag:     "EXPRESS CAMPUS PICKUP",
		Subline: "Upload from anywhere on campus  •  Release with your PIN in seconds",
		C0:      color.RGBA{R: 88, G: 28, B: 135, A: 255},  // #581C87
		C1:      color.RGBA{R: 124, G: 58, B: 237, A: 255}, // #7C3AED
		C2:      color.RGBA{R: 217, G: 119, B: 6, A: 255},  // #D97706
	},
}

func renderIdle(dst *image.RGBA, state ScreenState) error {
	// 1. Left Card: Ad Card (occupies full [37, 140, 1240, 980] bounds, no redundant white card)
	adCard := image.Rect(37, 140, 1240, 980)

	// Pick active slide (0, 1, or 2)
	slideIndex := state.AdSlideIndex % 3
	if slideIndex < 0 {
		slideIndex = (slideIndex + 3) % 3
	}
	slide := adSlides[slideIndex]

	// 135-degree true CSS linear gradient filling the full left card with rx=24
	fillRoundedGradient(dst, adCard, 24, slide.C0, slide.C1, slide.C2)

	// Top row: Tag Pill (No close button!)
	tagW := measureTextWidth(faceBold18, slide.Tag)
	tagRect := image.Rect(100, 210, 100+tagW+40, 256)
	fillRoundedRect(dst, tagRect, 10, color.RGBA{R: 255, G: 255, B: 255, A: 45})
	strokeRoundedRect(dst, tagRect, 10, 1, color.RGBA{R: 255, G: 255, B: 255, A: 85})
	drawText(dst, faceBold18, slide.Tag, tagRect.Min.X+20, tagRect.Min.Y+7, colorTextWhite)

	// Main Hero Slide Title & Description
	drawText(dst, faceBold56, slide.Title, 100, 330, colorTextWhite)
	drawText(dst, faceReg28, slide.Desc, 100, 420, color.RGBA{R: 241, G: 245, B: 249, A: 245})

	// Frosted feature card
	featureRect := image.Rect(100, 520, 1177, 630)
	fillRoundedRect(dst, featureRect, 16, color.RGBA{R: 255, G: 255, B: 255, A: 32})
	strokeRoundedRect(dst, featureRect, 16, 1, color.RGBA{R: 255, G: 255, B: 255, A: 65})
	drawText(dst, faceReg22, slide.Subline, 135, 558, color.RGBA{R: 255, G: 255, B: 255, A: 245})

	// Prompt subtext
	drawText(dst, faceReg20, "Scan the QR code on the right with your phone camera to begin printing", 100, 730, color.RGBA{R: 226, G: 232, B: 240, A: 225})

	// Bottom Carousel Indicators matching mobile frontend (.banner-indicators)
	// Centered at x=638: active pill (w=44) + 2 inactive dots (w=10)
	curX := 592
	for k := 0; k < 3; k++ {
		if k == slideIndex {
			pill := image.Rect(curX, 885, curX+44, 895)
			fillRoundedRect(dst, pill, 5, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			curX += 44 + 14
		} else {
			drawCircle(dst, curX+5, 890, 5, color.RGBA{R: 255, G: 255, B: 255, A: 115})
			curX += 10 + 14
		}
	}

	// 2. Right Card: QR Code + Onboarding Steps
	rightCard := image.Rect(1268, 140, 1891, 980)
	fillRoundedRect(dst, rightCard, 24, colorCardBg)
	strokeRoundedRect(dst, rightCard, 24, 2, colorBorder)

	// Headline above QR code
	qrTitle := "Scan with any phone to open your personal print portal"
	qrTitleW := measureTextWidth(faceReg18, qrTitle)
	drawText(dst, faceReg18, qrTitle, rightCard.Min.X+(rightCard.Dx()-qrTitleW)/2, 172, colorTextPrimary)

	// QR Code Generation
	qrBytes, err := qrcode.Encode(state.QRContent, qrcode.Medium, 400)
	if err != nil {
		return fmt.Errorf("failed to encode QR code: %w", err)
	}
	qrImg, err := png.Decode(bytes.NewReader(qrBytes))
	if err != nil {
		return fmt.Errorf("failed to decode QR PNG: %w", err)
	}

	// QR code is placed directly on the card without extra border box
	qrBounds := image.Rect(1380, 210, 1780, 610)
	draw.Draw(dst, qrBounds, qrImg, image.Point{}, draw.Src)

	// Action pill under QR
	pillRect := image.Rect(1350, 597, 1808, 657)
	drawPill(dst, pillRect, colorPillBlueBg, colorPillBlueBorder)
	pillText := "Instant Web App • No Install Needed"
	pillW := measureTextWidth(faceBold20, pillText)
	drawText(dst, faceBold20, pillText, pillRect.Min.X+(pillRect.Dx()-pillW)/2, 617, colorPrimaryBlue)

	// 4 Numbered Steps
	steps := []struct {
		num    string
		title  string
		isLast bool
	}{
		{"1", "Point camera at the QR code", false},
		{"2", "Upload document & choose options", false},
		{"3", "Pay securely with instant UPI", false},
		{"4", "Collect prints from the output tray", true},
	}
	ySteps := []int{692, 750, 808, 867}
	for i, s := range steps {
		y := ySteps[i]
		circleCol := colorPrimaryBlue
		if s.isLast {
			circleCol = colorSuccessGreen
		}
		drawCircle(dst, 1404, y+14, 15, circleCol)
		numW := measureTextWidth(faceBold18, s.num)
		drawText(dst, faceBold18, s.num, 1404-numW/2, y+4, colorTextWhite)

		drawText(dst, faceBold20, s.title, 1440, y+4, colorTextPrimary)
	}

	return nil
}

func renderDownloading(dst *image.RGBA, state ScreenState) {
	card := image.Rect(80, 140, 1840, 980)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	drawText(dst, faceBold44, "DOWNLOADING DOCUMENT", 140, 260, colorPrimaryBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Receiving print job securely from XeroxGo cloud..."
	}
	drawText(dst, faceReg24, msg, 140, 325, colorTextPrimary)

	if strings.TrimSpace(state.Detail) != "" {
		detailPill := image.Rect(140, 380, 760, 426)
		drawPill(dst, detailPill, colorPillBlueBg, colorPillBlueBorder)
		drawText(dst, faceReg18, state.Detail, 160, 393, colorPrimaryBlue)
	}

	// Progress Bar matching edited SVG bounds: (140, 500, 1787, 550)
	drawProgressBar(dst, 140, 500, 1647, 50, state.ProgressPercent, 100, colorPrimaryBlue)

	pctText := fmt.Sprintf("Download Progress: %d%%", state.ProgressPercent)
	drawText(dst, faceBold24, pctText, 140, 580, colorTextPrimary)

	drawText(dst, faceReg20, "Secure encrypted download in progress. Printing will begin automatically.", 140, 625, colorTextSecondary)
}

func renderPrinting(dst *image.RGBA, state ScreenState) {
	card := image.Rect(80, 140, 1840, 980)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	drawText(dst, faceBold44, "PRINTING DOCUMENT", 140, 260, colorPrimaryBlue)

	counterText := fmt.Sprintf("PAGE %d OF %d", state.CurrentPage, state.TotalPages)
	counterPill := image.Rect(140, 325, 460, 385)
	drawPill(dst, counterPill, colorPillBlueBg, colorPillBlueBorder)
	cW := measureTextWidth(faceBold28, counterText)
	drawText(dst, faceBold28, counterText, counterPill.Min.X+(counterPill.Dx()-cW)/2, 340, colorPrimaryBlue)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Please wait while your document prints..."
	}
	drawText(dst, faceReg24, msg, 140, 415, colorTextPrimary)

	if strings.TrimSpace(state.Detail) != "" {
		drawText(dst, faceReg18, state.Detail, 140, 465, colorTextSecondary)
	}

	// Progress Bar matching gate test bounds: (140, 520, 1700, 570)
	drawProgressBar(dst, 140, 520, 1560, 50, state.CurrentPage, state.TotalPages, colorPrimaryBlue)

	subProgress := fmt.Sprintf("Progress: %d of %d pages streamed to printer hardware", state.CurrentPage, state.TotalPages)
	drawText(dst, faceBold22, subProgress, 140, 600, colorTextPrimary)

	modelStr := "Brother HL-L5210DN"
	if state.PrinterModel != "" {
		modelStr = state.PrinterModel
	}
	activePill := image.Rect(140, 680, 1100, 745)
	drawPill(dst, activePill, colorBgSlate, colorBorder)
	activeText := fmt.Sprintf("Job is active on %s. Pages will exit into output tray.", modelStr)
	drawText(dst, faceReg20, activeText, 170, 702, colorTextSecondary)
}

func renderCompleted(dst *image.RGBA, state ScreenState) {
	card := image.Rect(80, 140, 1840, 980)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	drawText(dst, faceBold48, "PRINT COMPLETE", 140, 260, colorSuccessGreen)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Your print job has finished successfully."
	}
	drawText(dst, faceReg24, msg, 140, 335, colorTextPrimary)

	calloutBox := image.Rect(140, 390, 1700, 470)
	drawPill(dst, calloutBox, colorPillGreenBg, colorPillGreenBorder)
	calloutText := "PLEASE COLLECT ALL PRINTED PAGES FROM THE OUTPUT TRAY"
	cW := measureTextWidth(faceBold24, calloutText)
	drawText(dst, faceBold24, calloutText, calloutBox.Min.X+(calloutBox.Dx()-cW)/2, 417, color.RGBA{R: 4, G: 120, B: 87, A: 255})

	if strings.TrimSpace(state.Detail) != "" {
		drawText(dst, faceReg18, state.Detail, 140, 505, colorTextSecondary)
	}

	drawText(dst, faceReg22, "1. Inspect all printed pages before leaving the kiosk area.", 140, 555, colorTextPrimary)
	drawText(dst, faceReg22, "2. Kiosk will reset to the idle screen for the next user.", 140, 605, colorTextPrimary)
	drawText(dst, faceReg20, "Thank you for printing with XeroxGo!", 140, 685, colorTextSecondary)
}

func renderError(dst *image.RGBA, state ScreenState) {
	card := image.Rect(80, 140, 1840, 980)
	fillRoundedRect(dst, card, 24, colorCardBg)
	strokeRoundedRect(dst, card, 24, 2, colorBorder)

	drawText(dst, faceBold48, "PRINTER ATTENTION REQUIRED", 140, 230, colorErrorRed)

	msg := state.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Printer paper tray 1 is empty or disconnected."
	}
	drawText(dst, faceBold24, msg, 140, 305, colorTextPrimary)

	if strings.TrimSpace(state.Detail) != "" {
		drawText(dst, faceReg20, state.Detail, 140, 350, colorTextSecondary)
	}

	// Actionable recovery box
	boxRect := image.Rect(140, 410, 1775, 905)
	fillRoundedRect(dst, boxRect, 20, color.RGBA{R: 255, G: 251, B: 235, A: 255})
	strokeRoundedRect(dst, boxRect, 20, 2, color.RGBA{R: 253, G: 230, B: 138, A: 255})

	drawText(dst, faceBold22, "ACTIONABLE HARDWARE RECOVERY INSTRUCTIONS:", 182, 445, color.RGBA{R: 180, G: 83, B: 9, A: 255})

	recoverySteps := []string{
		"1. Check paper tray 1 and refill standard A4 paper if depleted.",
		"2. If paper is jammed, follow printer front panel steps to clear and re-seat covers.",
		"3. Verify that the output area and paper exit tray are completely clear.",
		"4. If error persists, please notify the campus attendant or support.",
	}
	yR := 509
	for _, step := range recoverySteps {
		drawText(dst, faceReg20, step, 182, yR, colorTextPrimary)
		yR += 70
	}
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
	if r.Empty() || col.A == 0 {
		return
	}
	if col.A == 255 {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			rowStart := y*dst.Stride + r.Min.X*4
			rowEnd := y*dst.Stride + r.Max.X*4
			for p := rowStart; p < rowEnd; p += 4 {
				dst.Pix[p+0] = col.R
				dst.Pix[p+1] = col.G
				dst.Pix[p+2] = col.B
				dst.Pix[p+3] = 255
			}
		}
		return
	}

	alpha := float64(col.A) / 255.0
	invAlpha := 1.0 - alpha
	rF := float64(col.R) * alpha
	gF := float64(col.G) * alpha
	bF := float64(col.B) * alpha

	for y := r.Min.Y; y < r.Max.Y; y++ {
		rowStart := y*dst.Stride + r.Min.X*4
		rowEnd := y*dst.Stride + r.Max.X*4
		for p := rowStart; p < rowEnd; p += 4 {
			dst.Pix[p+0] = uint8(rF + float64(dst.Pix[p+0])*invAlpha)
			dst.Pix[p+1] = uint8(gF + float64(dst.Pix[p+1])*invAlpha)
			dst.Pix[p+2] = uint8(bF + float64(dst.Pix[p+2])*invAlpha)
			dst.Pix[p+3] = 255
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
	initUIAssets()
	var face font.Face
	switch scale {
	case 1:
		face = faceReg16
	case 2:
		face = faceReg18
	case 3:
		face = faceReg24
	case 4:
		face = faceBold36
	default:
		face = faceBold48
	}
	drawText(dst, face, text, startX, startY, col)
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
			return fmt.Errorf("failed to sync temp file: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			return fmt.Errorf("failed to close temp file: %w", err)
		}

		if err := os.Rename(tmpName, pngPath); err != nil {
			return fmt.Errorf("failed to atomically rename preview file: %w", err)
		}

		cleanup = false
		return nil
	}
}

// FramebufferGeometry holds the detected physical display dimensions and pixel layout.
type FramebufferGeometry struct {
	Width        int
	Height       int
	BitsPerPixel int
	Stride       int
}

// FBGeometry is an alias for FramebufferGeometry for test compatibility.
type FBGeometry = FramebufferGeometry

// DetectFramebufferGeometry reads geometry parameters from sysfs.
func DetectFramebufferGeometry(fbPath string) FramebufferGeometry {
	geom := FramebufferGeometry{
		Width:        1920,
		Height:       1080,
		BitsPerPixel: 32,
		Stride:       1920 * 4,
	}

	fbBase := filepath.Base(fbPath)
	sysfsDir := filepath.Join("/sys/class/graphics", fbBase)

	if sizeBytes, err := os.ReadFile(filepath.Join(sysfsDir, "virtual_size")); err == nil {
		parts := strings.Split(strings.TrimSpace(string(sizeBytes)), ",")
		if len(parts) == 2 {
			if w, werr := strconv.Atoi(parts[0]); werr == nil && w > 0 {
				geom.Width = w
			}
			if h, herr := strconv.Atoi(parts[1]); herr == nil && h > 0 {
				geom.Height = h
			}
		}
	}

	if bppBytes, err := os.ReadFile(filepath.Join(sysfsDir, "bits_per_pixel")); err == nil {
		if bpp, perr := strconv.Atoi(strings.TrimSpace(string(bppBytes))); perr == nil && bpp > 0 {
			geom.BitsPerPixel = bpp
		}
	}

	strideRead := false
	for _, name := range []string{"stride", "line_length"} {
		if strideBytes, err := os.ReadFile(filepath.Join(sysfsDir, name)); err == nil {
			if s, serr := strconv.Atoi(strings.TrimSpace(string(strideBytes))); serr == nil && s > 0 {
				geom.Stride = s
				strideRead = true
				break
			}
		}
	}
	if !strideRead {
		bytesPerPixel := geom.BitsPerPixel / 8
		if bytesPerPixel < 1 {
			bytesPerPixel = 4
		}
		geom.Stride = geom.Width * bytesPerPixel
	}

	return geom
}

// FramebufferSink returns a Sink that maps canvas to /dev/fb0 using auto-detected geometry.
func FramebufferSink(fbPath string) Sink {
	return FramebufferSinkWithGeometry(fbPath, DetectFramebufferGeometry(fbPath))
}

// FramebufferSinkWithGeometry writes canvas pixels to /dev/fb0, scaling and encoding as required.
func FramebufferSinkWithGeometry(fbPath string, geom FramebufferGeometry) Sink {
	return func(img *image.RGBA) error {
		if strings.TrimSpace(fbPath) == "" {
			return fmt.Errorf("framebuffer path cannot be empty")
		}
		if img == nil {
			return fmt.Errorf("image cannot be nil")
		}

		f, err := os.OpenFile(fbPath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("failed to open framebuffer %s: %w", fbPath, err)
		}
		defer f.Close()

		canvasBounds := img.Bounds()
		canvasW := canvasBounds.Dx()
		canvasH := canvasBounds.Dy()

		var targetImg *image.RGBA
		if geom.Width == canvasW && geom.Height == canvasH {
			targetImg = img
		} else {
			targetImg = image.NewRGBA(image.Rect(0, 0, geom.Width, geom.Height))
			xdraw.BiLinear.Scale(targetImg, targetImg.Bounds(), img, canvasBounds, xdraw.Over, nil)
		}

		targetW := geom.Width
		targetH := geom.Height

		switch geom.BitsPerPixel {
		case 16:
			buf := make([]byte, geom.Stride*targetH)
			for y := 0; y < targetH; y++ {
				rowOffset := y * geom.Stride
				srcRowOffset := y * targetImg.Stride
				for x := 0; x < targetW; x++ {
					p := srcRowOffset + x*4
					r := uint16(targetImg.Pix[p+0]) >> 3
					g := uint16(targetImg.Pix[p+1]) >> 2
					b := uint16(targetImg.Pix[p+2]) >> 3
					rgb565 := (r << 11) | (g << 5) | b
					destIdx := rowOffset + x*2
					buf[destIdx+0] = byte(rgb565 & 0xFF)
					buf[destIdx+1] = byte((rgb565 >> 8) & 0xFF)
				}
			}
			if _, err := f.WriteAt(buf, 0); err != nil {
				return fmt.Errorf("write 16-bit framebuffer: %w", err)
			}
			return nil

		default:
			buf := make([]byte, geom.Stride*targetH)
			for y := 0; y < targetH; y++ {
				rowOffset := y * geom.Stride
				srcRowOffset := y * targetImg.Stride
				for x := 0; x < targetW; x++ {
					p := srcRowOffset + x*4
					destIdx := rowOffset + x*4
					buf[destIdx+0] = targetImg.Pix[p+2] // B
					buf[destIdx+1] = targetImg.Pix[p+1] // G
					buf[destIdx+2] = targetImg.Pix[p+0] // R
					buf[destIdx+3] = targetImg.Pix[p+3] // A
				}
			}
			if _, err := f.WriteAt(buf, 0); err != nil {
				return fmt.Errorf("write 32-bit framebuffer: %w", err)
			}
			return nil
		}
	}
}
