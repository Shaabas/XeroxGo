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

func main() {
	outDir := "design/figma"
	if err := os.MkdirAll(outDir, 0755); err != nil {
		panic(err)
	}

	logoBytes, err := os.ReadFile("frontend/assets/xeroxgo-full-logo-transparent.png")
	if err != nil {
		logoBytes, _ = os.ReadFile("kiosk-daemon/internal/ui_ipc/assets/logo.png")
	}
	logoB64 := base64.StdEncoding.EncodeToString(logoBytes)

	// Generate QR code SVG or data uri
	qrPng, _ := qrcode.Encode("https://xeroxgo.vercel.app", qrcode.Medium, 400)
	qrB64 := base64.StdEncoding.EncodeToString(qrPng)

	screens := []struct {
		filename string
		title    string
		svg      string
	}{
		{"01_idle.svg", "Idle - Ready to Scan", buildIdleSVG(logoB64, qrB64)},
		{"02_downloading.svg", "Downloading Job", buildDownloadingSVG(logoB64)},
		{"03_printing.svg", "Printing Hardware Progress", buildPrintingSVG(logoB64)},
		{"04_completed.svg", "Print Completed", buildCompletedSVG(logoB64)},
		{"05_error.svg", "Attention & Hardware Alert", buildErrorSVG(logoB64)},
	}

	for _, s := range screens {
		path := filepath.Join(outDir, s.filename)
		if err := os.WriteFile(path, []byte(s.svg), 0644); err != nil {
			panic(err)
		}
		fmt.Printf("Exported %s\n", path)
	}

	// Also generate unified multi-frame artboard
	masterArtboard := buildMasterArtboardSVG(screens)
	artboardPath := filepath.Join(outDir, "all_screens_artboard.svg")
	if err := os.WriteFile(artboardPath, []byte(masterArtboard), 0644); err != nil {
		panic(err)
	}
	fmt.Printf("Exported %s\n", artboardPath)
}

func svgHeader(title string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg width="1920" height="1080" viewBox="0 0 1920 1080" fill="none" xmlns="http://www.w3.org/2000/svg">
  <title>%s</title>
  <defs>
    <style>
      @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&amp;display=swap');
      text { font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
    </style>
  </defs>
  <!-- Background Slate-50 -->
  <rect width="1920" height="1080" fill="#F8FAFC"/>
`, title)
}

func buildCommonHeaderAndFooter(logoB64, badgeText, badgeBg, badgeBorder, badgeTextColor, model string) string {
	return fmt.Sprintf(`
  <!-- HEADER -->
  <g id="Header">
    <rect width="1920" height="110" fill="#FFFFFF"/>
    <line x1="0" y1="110" x2="1920" y2="110" stroke="#E2E8F0" stroke-width="2"/>
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

func buildIdleSVG(logoB64, qrB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Idle Screen"))
	buf.WriteString(buildCommonHeaderAndFooter(logoB64, "● READY TO PRINT", "#ECFDF5", "#A7F3D0", "#047857", "Brother DCP-T310"))

	buf.WriteString(`
  <!-- MAIN CONTENT: TWO COLUMN CARDS -->
  <g id="Cards">
    <!-- Left Card: Guide -->
    <g id="LeftCard">
      <rect x="80" y="140" width="1040" height="840" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>
      
      <!-- Tag Pill -->
      <rect x="140" y="185" width="190" height="36" rx="18" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="1.5"/>
      <text x="235" y="209" fill="#2563EB" font-size="16" font-weight="700" text-anchor="middle">TOUCHLESS GUIDE</text>

      <text x="140" y="280" fill="#1E293B" font-size="44" font-weight="800">Scan &amp; Print in Seconds</text>
      <text x="140" y="325" fill="#64748B" font-size="20" font-weight="400">No apps or accounts required. Point your phone camera to begin.</text>

      <!-- Steps -->
      <g id="Steps" transform="translate(140, 375)">
        <!-- Step 1 -->
        <g id="Step1" transform="translate(0, 0)">
          <circle cx="22" cy="22" r="22" fill="#2563EB"/>
          <text x="22" y="29" fill="#FFFFFF" font-size="20" font-weight="700" text-anchor="middle">1</text>
          <text x="65" y="20" fill="#1E293B" font-size="22" font-weight="700">Point camera at the QR code</text>
          <text x="65" y="48" fill="#64748B" font-size="18" font-weight="400">Open the standard Camera app on your iPhone or Android device.</text>
        </g>

        <!-- Step 2 -->
        <g id="Step2" transform="translate(0, 95)">
          <circle cx="22" cy="22" r="22" fill="#2563EB"/>
          <text x="22" y="29" fill="#FFFFFF" font-size="20" font-weight="700" text-anchor="middle">2</text>
          <text x="65" y="20" fill="#1E293B" font-size="22" font-weight="700">Upload document &amp; choose options</text>
          <text x="65" y="48" fill="#64748B" font-size="18" font-weight="400">Select copies, pages, orientation, and color or monochrome.</text>
        </g>

        <!-- Step 3 -->
        <g id="Step3" transform="translate(0, 190)">
          <circle cx="22" cy="22" r="22" fill="#2563EB"/>
          <text x="22" y="29" fill="#FFFFFF" font-size="20" font-weight="700" text-anchor="middle">3</text>
          <text x="65" y="20" fill="#1E293B" font-size="22" font-weight="700">Pay securely with instant UPI</text>
          <text x="65" y="48" fill="#64748B" font-size="18" font-weight="400">Google Pay, PhonePe, Paytm, or BHIM. Zero card details needed.</text>
        </g>

        <!-- Step 4 -->
        <g id="Step4" transform="translate(0, 285)">
          <circle cx="22" cy="22" r="22" fill="#10B981"/>
          <text x="22" y="29" fill="#FFFFFF" font-size="20" font-weight="700" text-anchor="middle">4</text>
          <text x="65" y="20" fill="#1E293B" font-size="22" font-weight="700">Collect prints from the output tray</text>
          <text x="65" y="48" fill="#64748B" font-size="18" font-weight="400">High quality output prints automatically into the front tray.</text>
        </g>
      </g>

      <!-- Readiness banner -->
      <rect x="140" y="835" width="920" height="70" rx="35" fill="#ECFDF5" stroke="#A7F3D0" stroke-width="2"/>
      <text x="600" y="878" fill="#047857" font-size="18" font-weight="700" text-anchor="middle">PRINTER READY • HIGH SPEED A4 • TOUCHLESS MOBILE PRINTING</text>
    </g>

    <!-- Right Card: QR Code -->
    <g id="RightCard">
      <rect x="1160" y="140" width="680" height="840" rx="24" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>
      <text x="1500" y="210" fill="#1E293B" font-size="24" font-weight="700" text-anchor="middle">POINT PHONE CAMERA</text>
      <text x="1500" y="245" fill="#64748B" font-size="18" font-weight="400" text-anchor="middle">Scan with any phone to open your personal print portal</text>

      <!-- QR Card Box -->
      <rect x="1216" y="266" width="448" height="448" rx="16" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="2"/>
      <image x="1240" y="290" width="400" height="400" href="data:image/png;base64,` + qrB64 + `" preserveAspectRatio="xMidYMid meet"/>

      <!-- Action Pill -->
      <rect x="1250" y="745" width="500" height="60" rx="30" fill="#EFF6FF" stroke="#BFDBFE" stroke-width="2"/>
      <text x="1500" y="782" fill="#2563EB" font-size="20" font-weight="700" text-anchor="middle">xeroxgo.vercel.app</text>

      <text x="1500" y="865" fill="#64748B" font-size="16" font-weight="500" text-anchor="middle">End-to-End Encrypted Session • Files Deleted Immediately</text>
    </g>
  </g>
</svg>`)
	return buf.String()
}

func buildDownloadingSVG(logoB64 string) string {
	var buf bytes.Buffer
	buf.WriteString(svgHeader("XeroxGo Kiosk - Downloading Screen"))
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
	buf.WriteString(svgHeader("XeroxGo Kiosk - Printing Screen"))
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
	buf.WriteString(svgHeader("XeroxGo Kiosk - Completed Screen"))
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
	buf.WriteString(svgHeader("XeroxGo Kiosk - Error Screen"))
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
	// 5 screens in a 3-column / 2-row layout with 100px padding
	// Canvas: width = 3 * 1920 + 4 * 100 = 6160
	// Height = 2 * 1080 + 3 * 100 = 2460
	const (
		pad = 100
		sw  = 1920
		sh  = 1080
	)
	cols := 3
	rows := 2
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

		// Title tag
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
