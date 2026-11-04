package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/skip2/go-qrcode"
)

const (
	width  = 1920
	height = 1080
)

type adSlideDef struct {
	title  string
	desc   string
	c0     string
	c1     string
	c2     string
	active int
}

var defaultSlides = []adSlideDef{
	{
		title:  "Quick &amp; Easy",
		desc:   "Direct UPI payment. No coins, no cash needed.",
		c0:     "#1E3A8A",
		c1:     "#2563EB",
		c2:     "#4F46E5",
		active: 0,
	},
	{
		title:  "High Quality Prints",
		desc:   "Crisp laser documents ready in seconds.",
		c0:     "#065F46",
		c1:     "#10B981",
		c2:     "#059669",
		active: 1,
	},
	{
		title:  "Skip The Queue",
		desc:   "Pick up your print job with your 4-digit code.",
		c0:     "#581C87",
		c1:     "#7C3AED",
		c2:     "#D97706",
		active: 2,
	},
}

func main() {
	outDir := "design/figma"
	if err := os.MkdirAll(outDir, 0755); err != nil {
		panic(err)
	}

	logoBytes, err := os.ReadFile("frontend/assets/xeroxgo-full-logo-transparent.png")
	if err != nil {
		logoBytes, _ = os.ReadFile("frontend/assets/xeroxgo-logo.png")
	}
	logoB64 := base64.StdEncoding.EncodeToString(logoBytes)

	// Generate QR code PNG and base64 encode
	qrPng, _ := qrcode.Encode("https://xeroxgo.vercel.app", qrcode.Medium, 400)
	qrB64 := base64.StdEncoding.EncodeToString(qrPng)

	screens := []struct {
		filename string
		title    string
		svg      string
	}{
		{"01_idle.svg", "Idle Screen (Slide 1 - Quick & Easy)", buildCurrentIdleSVG(logoB64, qrB64, defaultSlides[0], 1)},
		{"01_idle_slide2.svg", "Idle Screen (Slide 2 - High Quality Prints)", buildCurrentIdleSVG(logoB64, qrB64, defaultSlides[1], 2)},
		{"01_idle_slide3.svg", "Idle Screen (Slide 3 - Skip The Queue)", buildCurrentIdleSVG(logoB64, qrB64, defaultSlides[2], 3)},
		{"02_downloading.svg", "Downloading Screen", buildDownloadingSVG(logoB64)},
		{"03_printing.svg", "Printing Screen", buildPrintingSVG(logoB64)},
		{"04_completed.svg", "Print Completed Screen", buildCompletedSVG(logoB64)},
		{"05_error.svg", "Attention & Hardware Alert Screen", buildErrorSVG(logoB64)},
	}

	for _, s := range screens {
		path := filepath.Join(outDir, s.filename)
		if err := os.WriteFile(path, []byte(s.svg), 0644); err != nil {
			panic(err)
		}
		fmt.Printf("Exported %s\n", path)
	}

	// Generate unified multi-frame artboard for Figma
	masterArtboard := buildMasterArtboardSVG(screens)
	artboardPath := filepath.Join(outDir, "all_screens_artboard.svg")
	if err := os.WriteFile(artboardPath, []byte(masterArtboard), 0644); err != nil {
		panic(err)
	}
	fmt.Printf("Exported %s\n", artboardPath)
}

func svgHeader(title string, extraDefs string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg width="1920" height="1080" viewBox="0 0 1920 1080" fill="none" xmlns="http://www.w3.org/2000/svg">
  <title>%s</title>
  <defs>
    <style>
      @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&amp;display=swap');
      text { font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
    </style>
%s  </defs>
  <!-- Background Slate-50 -->
  <rect width="1920" height="1080" fill="#F8FAFC"/>
`, title, extraDefs)
}

func buildCommonHeaderAndFooter(logoB64, badgeText, badgeBg, badgeBorder, badgeTextColor, model string) string {
	return fmt.Sprintf(`
  <!-- HEADER -->
  <g id="Header">
    <rect width="1920" height="110" fill="#FFFFFF"/>
    <line x1="0" y1="108" x2="1920" y2="108" stroke="#E2E8F0" stroke-width="2"/>
    <image x="80" y="31" width="360" height="48" href="data:image/png;base64,%s" preserveAspectRatio="xMidYMid meet"/>
    <text x="480" y="62" fill="#64748B" font-size="18" font-weight="500">Campus Print Kiosk • Touchless Mobile Printing</text>
    
    <!-- Status Badge -->
    <g id="StatusBadge" transform="translate(1540, 30)">
      <rect width="300" height="50" rx="25" fill="%s" stroke="%s" stroke-width="2"/>
      <text x="150" y="31" fill="%s" font-size="18" font-weight="700" text-anchor="middle">%s</text>
    </g>
  </g>

  <!-- FOOTER -->
  <g id="Footer">
    <rect y="1010" width="1920" height="70" fill="#FFFFFF"/>
    <line x1="0" y1="1010" x2="1920" y2="1010" stroke="#E2E8F0" stroke-width="2"/>
    <text x="80" y="1052" fill="#64748B" font-size="18" font-weight="500">XeroxGo Kiosk Daemon • Touchless Mobile Printing • %s</text>
  </g>
`, logoB64, badgeBg, badgeBorder, badgeTextColor, badgeText, model)
}

func buildCurrentIdleSVG(logoB64, qrB64 string, slide adSlideDef, slideNum int) string {
	var buf bytes.Buffer
	gradID := fmt.Sprintf("adGrad%d", slideNum)
	extraDefs := fmt.Sprintf(`    <linearGradient id="%s" x1="0%%" y1="0%%" x2="100%%" y2="100%%">
      <stop offset="0%%" stop-color="%s"/>
      <stop offset="50%%" stop-color="%s"/>
      <stop offset="100%%" stop-color="%s"/>
    </linearGradient>
`, gradID, slide.c0, slide.c1, slide.c2)

	buf.WriteString(svgHeader(fmt.Sprintf("XeroxGo Kiosk - Idle Screen (Slide %d)", slideNum), extraDefs))
	buf.WriteString(buildCommonHeaderAndFooter(logoB64, "● READY TO PRINT", "#ECFDF5", "#A7F3D0", "#047857", "Brother DCP-T310"))

	// Indicators: Slide 1, 2, 3
	dot1W, dot2W, dot3W := 10, 10, 10
	dot1A, dot2A, dot3A := "0.45", "0.45", "0.45"
	switch slide.active {
	case 0:
		dot1W = 44
		dot1A = "1.0"
	case 1:
		dot2W = 44
		dot2A = "1.0"
	case 2:
		dot3W = 44
		dot3A = "1.0"
	}

	buf.WriteString(fmt.Sprintf(`
  <!-- MAIN CONTENT: DUAL CARDS -->
  <g id="IdleCards">
    <!-- LEFT CARD: PROMOTIONAL AD BANNER CAROUSEL -->
    <g id="LeftCard_AdSlide">
      <rect x="37" y="140" width="1203" height="840" rx="24" fill="url(#%s)"/>
      
      <!-- Slide Content -->
      <g id="SlideText" transform="translate(117, 450)">
        <text x="0" y="0" fill="#FFFFFF" font-size="56" font-weight="800" letter-spacing="-0.02em">%s</text>
        <text x="0" y="70" fill="#E2E8F0" font-size="28" font-weight="400">%s</text>
      </g>

      <!-- Carousel Indicator Dots -->
      <g id="CarouselIndicators" transform="translate(590, 885)">
        <rect x="0" y="0" width="%d" height="10" rx="5" fill="#FFFFFF" opacity="%s"/>
        <rect x="%d" y="0" width="%d" height="10" rx="5" fill="#FFFFFF" opacity="%s"/>
        <rect x="%d" y="0" width="%d" height="10" rx="5" fill="#FFFFFF" opacity="%s"/>
      </g>
    </g>

    <!-- RIGHT CARD: QR CODE & MOBILE ONBOARDING -->
    <g id="RightCard_QROnboarding">
      <rect x="1268" y="140" width="612" height="840" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>
      
      <!-- QR Title -->
      <text x="1574" y="185" fill="#1E293B" font-size="18" font-weight="600" text-anchor="middle">Scan with any phone to open your personal print portal</text>

      <!-- QR Code Image -->
      <g id="QRCode" transform="translate(1374, 210)">
        <image width="400" height="400" href="data:image/png;base64,%s" preserveAspectRatio="xMidYMid meet"/>
      </g>

      <!-- Action Pill with Website Link -->
      <g id="ActionPill" transform="translate(1344, 615)">
        <rect width="460" height="56" rx="28" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="2"/>
        <text x="230" y="36" fill="#2563EB" font-size="22" font-weight="700" text-anchor="middle">xeroxgo.vercel.app</text>
      </g>

      <!-- Numbered Steps -->
      <g id="Steps" transform="translate(1344, 700)">
        <!-- Step 1 -->
        <g id="Step1" transform="translate(0, 0)">
          <circle cx="16" cy="16" r="16" fill="#2563EB"/>
          <text x="16" y="22" fill="#FFFFFF" font-size="16" font-weight="700" text-anchor="middle">1</text>
          <text x="46" y="23" fill="#1E293B" font-size="20" font-weight="600">Point camera at the QR code</text>
        </g>

        <!-- Step 2 -->
        <g id="Step2" transform="translate(0, 58)">
          <circle cx="16" cy="16" r="16" fill="#2563EB"/>
          <text x="16" y="22" fill="#FFFFFF" font-size="16" font-weight="700" text-anchor="middle">2</text>
          <text x="46" y="23" fill="#1E293B" font-size="20" font-weight="600">Upload document &amp; choose options</text>
        </g>

        <!-- Step 3 -->
        <g id="Step3" transform="translate(0, 116)">
          <circle cx="16" cy="16" r="16" fill="#2563EB"/>
          <text x="16" y="22" fill="#FFFFFF" font-size="16" font-weight="700" text-anchor="middle">3</text>
          <text x="46" y="23" fill="#1E293B" font-size="20" font-weight="600">Pay securely with instant UPI</text>
        </g>

        <!-- Step 4 -->
        <g id="Step4" transform="translate(0, 174)">
          <circle cx="16" cy="16" r="16" fill="#10B981"/>
          <text x="16" y="22" fill="#FFFFFF" font-size="16" font-weight="700" text-anchor="middle">4</text>
          <text x="46" y="23" fill="#1E293B" font-size="20" font-weight="600">Collect prints from the output tray</text>
        </g>
      </g>

      <text x="1574" y="945" fill="#94A3B8" font-size="14" font-weight="500" text-anchor="middle">End-to-End Encrypted Session • Files Deleted Immediately</text>
    </g>
  </g>
</svg>`,
		gradID,
		slide.title,
		slide.desc,
		dot1W, dot1A,
		dot1W+14, dot2W, dot2A,
		dot1W+14+dot2W+14, dot3W, dot3A,
		qrB64,
	))

	return buf.String()
}

func buildDownloadingSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Downloading Screen", ""))
	buf.WriteString(buildCommonHeaderAndFooter(logoB64, "● DOWNLOADING", "#EFF6FF", "#BFDBFE", "#1D4ED8", "Brother DCP-T310"))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="DownloadingCard">
    <rect x="80" y="140" width="1760" height="840" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <rect x="140" y="195" width="200" height="40" rx="20" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="1.5"/>
    <text x="240" y="221" fill="#2563EB" font-size="18" font-weight="700" text-anchor="middle">CLOUD TRANSFER</text>

    <text x="140" y="295" fill="#2563EB" font-size="44" font-weight="800">DOWNLOADING DOCUMENT</text>
    <text x="140" y="345" fill="#1E293B" font-size="24" font-weight="400">Receiving print job securely from XeroxGo cloud...</text>

    <!-- Detail Pill -->
    <rect x="140" y="380" width="620" height="46" rx="23" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="1.5"/>
    <text x="160" y="410" fill="#2563EB" font-size="18" font-weight="500">Job ID: xg-order-883921 • Document: Lecture_Notes_CS101.pdf</text>

    <!-- Progress Bar -->
    <g id="ProgressBar" transform="translate(140, 500)">
      <rect width="1560" height="50" rx="10" fill="#E2E8F0" stroke="#CBD5E1" stroke-width="2"/>
      <rect x="2" y="2" width="1058" height="46" rx="8" fill="#2563EB"/>
    </g>

    <text x="140" y="605" fill="#1E293B" font-size="24" font-weight="700">Download Progress: 68%</text>
    <text x="140" y="700" fill="#64748B" font-size="20" font-weight="400">Secure encrypted download in progress. Printing will begin automatically.</text>
  </g>
</svg>`)
	return buf.String()
}

func buildPrintingSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Printing Screen", ""))
	buf.WriteString(buildCommonHeaderAndFooter(logoB64, "● PRINTING", "#EFF6FF", "#BFDBFE", "#1D4ED8", "Brother DCP-T310"))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="PrintingCard">
    <rect x="80" y="140" width="1760" height="840" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <rect x="140" y="195" width="200" height="40" rx="20" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="1.5"/>
    <text x="240" y="221" fill="#2563EB" font-size="18" font-weight="700" text-anchor="middle">HARDWARE PRINT</text>

    <text x="140" y="295" fill="#2563EB" font-size="44" font-weight="800">PRINTING DOCUMENT</text>

    <!-- Page Counter Pill -->
    <rect x="140" y="325" width="320" height="60" rx="30" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="2"/>
    <text x="300" y="363" fill="#2563EB" font-size="28" font-weight="700" text-anchor="middle">PAGE 2 OF 5</text>

    <text x="140" y="435" fill="#1E293B" font-size="24" font-weight="400">Please wait while your document prints...</text>
    <text x="140" y="480" fill="#64748B" font-size="18" font-weight="400">Job ID: xg-order-883921 • Duplex Simplex • 1 Copy</text>

    <!-- Progress Bar -->
    <g id="ProgressBar" transform="translate(140, 520)">
      <rect width="1560" height="50" rx="10" fill="#E2E8F0" stroke="#CBD5E1" stroke-width="2"/>
      <rect x="2" y="2" width="624" height="46" rx="8" fill="#2563EB"/>
    </g>

    <text x="140" y="620" fill="#1E293B" font-size="22" font-weight="700">Progress: 2 of 5 pages streamed to printer hardware</text>

    <!-- Hardware Notice Pill -->
    <rect x="140" y="680" width="960" height="65" rx="32" fill="#F8FAFC" stroke="#E2E8F0" stroke-width="2"/>
    <text x="170" y="721" fill="#64748B" font-size="20" font-weight="400">Job is active on Brother DCP-T310. Pages will exit into output tray.</text>
  </g>
</svg>`)
	return buf.String()
}

func buildCompletedSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Completed Screen", ""))
	buf.WriteString(buildCommonHeaderAndFooter(logoB64, "● COMPLETE", "#ECFDF5", "#A7F3D0", "#047857", "Brother DCP-T310"))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="CompletedCard">
    <rect x="80" y="140" width="1760" height="840" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <rect x="140" y="195" width="180" height="40" rx="20" fill="#ECFDF5" stroke="#A7F3D0" stroke-width="1.5"/>
    <text x="230" y="221" fill="#047857" font-size="18" font-weight="700" text-anchor="middle">JOB FINISHED</text>

    <text x="140" y="295" fill="#10B981" font-size="48" font-weight="800">PRINT COMPLETE</text>
    <text x="140" y="355" fill="#1E293B" font-size="24" font-weight="400">All 5 pages have been streamed and printed successfully.</text>

    <!-- Collection Callout Banner -->
    <rect x="140" y="390" width="1560" height="80" rx="40" fill="#ECFDF5" stroke="#A7F3D0" stroke-width="2"/>
    <text x="920" y="440" fill="#047857" font-size="24" font-weight="700" text-anchor="middle">PLEASE COLLECT ALL PRINTED PAGES FROM THE OUTPUT TRAY</text>

    <text x="140" y="520" fill="#64748B" font-size="18" font-weight="400">Job ID: xg-order-883921 • Total Sheets: 5</text>
    
    <g id="Instructions" transform="translate(140, 570)">
      <text x="0" y="0" fill="#1E293B" font-size="22" font-weight="500">1. Inspect all printed pages before leaving the kiosk area.</text>
      <text x="0" y="50" fill="#1E293B" font-size="22" font-weight="500">2. Kiosk will reset to the idle screen for the next user.</text>
      <text x="0" y="130" fill="#64748B" font-size="20" font-weight="400">Thank you for printing with XeroxGo!</text>
    </g>
  </g>
</svg>`)
	return buf.String()
}

func buildErrorSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Error Screen", ""))
	buf.WriteString(buildCommonHeaderAndFooter(logoB64, "● ATTENTION", "#FEF2F2", "#FECACA", "#B91C1C", "Brother DCP-T310"))

	buf.WriteString(`
  <!-- MAIN CARD -->
  <g id="ErrorCard">
    <rect x="80" y="140" width="1760" height="840" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>

    <rect x="140" y="175" width="200" height="40" rx="20" fill="#FEF2F2" stroke="#FECACA" stroke-width="1.5"/>
    <text x="240" y="201" fill="#B91C1C" font-size="18" font-weight="700" text-anchor="middle">DEVICE STATUS</text>

    <text x="140" y="260" fill="#EF4444" font-size="48" font-weight="800">PRINTER ATTENTION REQUIRED</text>
    <text x="140" y="325" fill="#1E293B" font-size="24" font-weight="700">Printer paper tray 1 is empty or disconnected.</text>
    <text x="140" y="365" fill="#64748B" font-size="20" font-weight="400">Error Code: CUPS_MEDIA_EMPTY • Job ID: xg-order-883921</text>

    <!-- Recovery Box -->
    <g id="RecoveryInstructions" transform="translate(140, 410)">
      <rect width="1560" height="460" rx="20" fill="#FFFBEB" stroke="#FDE68A" stroke-width="2"/>
      <text x="40" y="48" fill="#B45309" font-size="22" font-weight="700">ACTIONABLE HARDWARE RECOVERY INSTRUCTIONS:</text>

      <g transform="translate(40, 110)">
        <text x="0" y="0" fill="#1E293B" font-size="20" font-weight="500">1. Check paper tray 1 and refill standard A4 paper if depleted.</text>
        <text x="0" y="65" fill="#1E293B" font-size="20" font-weight="500">2. If paper is jammed, follow printer front panel steps to clear and re-seat covers.</text>
        <text x="0" y="130" fill="#1E293B" font-size="20" font-weight="500">3. Verify that the output area and paper exit tray are completely clear.</text>
        <text x="0" y="195" fill="#1E293B" font-size="20" font-weight="500">4. If error persists, please notify the campus attendant or support.</text>
      </g>
    </g>
  </g>
</svg>`)
	return buf.String()
}

func buildMasterArtboardSVG(screens []struct {
	filename string
	title    string
	svg      string
}) string {
	const (
		pad = 100
		sw  = 1920
		sh  = 1080
	)
	cols := 3
	rows := (len(screens) + cols - 1) / cols
	totalW := cols*sw + (cols+1)*pad
	totalH := rows*sh + (rows+1)*pad

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg width="%d" height="%d" viewBox="0 0 %d %d" fill="none" xmlns="http://www.w3.org/2000/svg">
  <title>XeroxGo Kiosk - Master UI Design Board</title>
  <defs>
    <style>
      @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&amp;display=swap');
      text { font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
    </style>
  </defs>
  <rect width="%d" height="%d" fill="#0F172A"/>
`, totalW, totalH, totalW, totalH, totalW, totalH))

	for i, s := range screens {
		r := i / cols
		c := i % cols
		x := pad + c*(sw+pad)
		y := pad + r*(sh+pad)

		buf.WriteString(fmt.Sprintf(`
  <g id="ScreenFrame_%d" transform="translate(%d, %d)">
    <text x="0" y="-30" fill="#94A3B8" font-size="28" font-weight="700">%s (%s)</text>
    <rect width="%d" height="%d" rx="28" fill="#F8FAFC" filter="drop-shadow(0 20px 40px rgba(0,0,0,0.5))"/>
    <g transform="translate(0, 0)">
`, i+1, x, y, s.title, s.filename, sw, sh))

		// Extract inner svg content
		content := s.svg
		startIdx := bytes.Index([]byte(content), []byte("<!-- Background Slate-50 -->"))
		endIdx := bytes.LastIndex([]byte(content), []byte("</svg>"))
		if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
			buf.WriteString(content[startIdx:endIdx])
		}

		buf.WriteString(`
    </g>
  </g>
`)
	}

	buf.WriteString("</svg>\n")
	return buf.String()
}
