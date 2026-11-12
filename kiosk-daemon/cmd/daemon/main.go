package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"xeroxgo/kiosk-daemon/internal/audio"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/spooler"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/kiosk-daemon/internal/viewer"
	"xeroxgo/pkg/contracts"
)

func sendSystemdNotify(state string) error {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return nil
	}
	addr := &net.UnixAddr{
		Name: socketPath,
		Net:  "unixgram",
	}
	if strings.HasPrefix(socketPath, "@") {
		addr.Name = "\x00" + socketPath[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

func startSystemdWatchdog(ctx context.Context, wg *sync.WaitGroup) {
	usecStr := os.Getenv("WATCHDOG_USEC")
	if usecStr == "" {
		return
	}
	usec, err := strconv.ParseInt(usecStr, 10, 64)
	if err != nil || usec <= 0 {
		return
	}

	interval := time.Duration(usec/2) * time.Microsecond
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = sendSystemdNotify("WATCHDOG=1")
			}
		}
	}()
}

type mockPrinterState struct {
	failureType      string
	failureSheet     int
	failureTriggered atomic.Bool
}

func startMockPrinter(ctx context.Context, downloadDir string, wg *sync.WaitGroup, mockState *mockPrinterState) (string, func(), error) {
	if mockState.failureSheet <= 0 && mockState.failureType != "" {
		mockState.failureTriggered.Store(true)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("failed to bind mock printer: %w", err)
	}

	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			_ = ln.Close()
		})
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleMockPrinterConn(conn, downloadDir, mockState)
		}
	}()

	return ln.Addr().String(), cleanup, nil
}


func handleMockPrinterConn(conn net.Conn, downloadDir string, mockState *mockPrinterState) {
	defer conn.Close()

	failTypeNorm := strings.ToLower(strings.TrimSpace(mockState.failureType))
	if mockState.failureSheet <= 0 && failTypeNorm != "" {
		mockState.failureTriggered.Store(true)
		log.Printf("[MOCK-PRINTER] Immediate failure (%s); aborting connection immediately", failTypeNorm)
		return
	}

	var buf bytes.Buffer
	const maxRead = 300 * 1024 * 1024 // 300 MiB safety bound
	limited := io.LimitReader(conn, maxRead)
	if _, err := io.Copy(&buf, limited); err != nil {
		return
	}

	streamBytes := buf.Bytes()
	headerMarker := []byte("@PJL ENTER LANGUAGE=PDF")
	headerIdx := bytes.Index(streamBytes, headerMarker)
	if headerIdx == -1 {
		return
	}

	pdfStart := headerIdx + len(headerMarker)
	if pdfStart < len(streamBytes) && streamBytes[pdfStart] == '\r' {
		pdfStart++
	}
	if pdfStart < len(streamBytes) && streamBytes[pdfStart] == '\n' {
		pdfStart++
	}

	trailerMarker := []byte("\x1b%-12345X@PJL EOJ")
	trailerIdx := bytes.Index(streamBytes[pdfStart:], trailerMarker)
	var pdfBytes []byte
	if trailerIdx != -1 {
		pdfBytes = streamBytes[pdfStart : pdfStart+trailerIdx]
	} else {
		pdfBytes = streamBytes[pdfStart:]
	}

	// 1. Authoritative sheet count from PJL header
	totalSheets := 0
	reSheets := regexp.MustCompile(`@PJL\s+COMMENT\s+XEROXGO\s+TOTAL_SHEETS=(\d+)`)
	if m := reSheets.FindSubmatch(streamBytes[:headerIdx]); len(m) > 1 {
		if s, serr := strconv.Atoi(string(m[1])); serr == nil && s > 0 {
			totalSheets = s
		}
	}

	// 2. Safe fallback if comment missing
	if totalSheets <= 0 {
		pages := 1
		rePages := regexp.MustCompile(`/Type\s*/Page[^s]`)
		matches := rePages.FindAll(pdfBytes, -1)
		if len(matches) > 0 {
			pages = len(matches)
		}

		copies := 1
		reCopies := regexp.MustCompile(`@PJL\s+SET\s+COPIES\s*=\s*(\d+)`)
		if m := reCopies.FindSubmatch(streamBytes[:headerIdx]); len(m) > 1 {
			if c, cerr := strconv.Atoi(string(m[1])); cerr == nil && c > 0 {
				copies = c
			}
		}

		isDuplex := bytes.Contains(streamBytes[:headerIdx], []byte("@PJL SET DUPLEX=ON"))
		if !isDuplex {
			totalSheets = pages * copies
		} else {
			totalSheets = ((pages + 1) / 2) * copies
		}
		if totalSheets < 1 {
			totalSheets = 1
		}
	}

	for sheet := 1; sheet <= totalSheets; sheet++ {
		// Check for mid-job failure injection: trigger failure when sheet exceeds failureSheet
		if mockState.failureSheet > 0 && sheet > mockState.failureSheet && failTypeNorm != "" {
			mockState.failureTriggered.Store(true)
			log.Printf("[MOCK-PRINTER] Injected failure (%s) triggered after sheet %d; aborting connection", failTypeNorm, mockState.failureSheet)
			return
		}

		time.Sleep(80 * time.Millisecond)
		msg := fmt.Sprintf("@PJL USTATUS PAGE\r\n%d\r\n", sheet)
		if _, werr := conn.Write([]byte(msg)); werr != nil {
			return
		}
	}

	time.Sleep(50 * time.Millisecond)
	_, _ = conn.Write([]byte("@PJL USTATUS JOB\r\nEND\r\n\f"))
}

func suppressConsoleCursor() {
	// 1. Disable cursor blinking in kernel fbcon sysfs if available
	_ = os.WriteFile("/sys/class/graphics/fbcon/cursor_blink", []byte("0\n"), 0644)

	// 2. Hide cursor on virtual terminals via VT100 escape sequence
	for _, dev := range []string{"/dev/tty1", "/dev/tty0", "/dev/console"} {
		if f, err := os.OpenFile(dev, os.O_WRONLY, 0); err == nil {
			_, _ = f.Write([]byte("\x1b[?25l"))
			_ = f.Close()
		}
	}
}

func restoreConsoleCursor() {
	for _, dev := range []string{"/dev/tty1", "/dev/tty0", "/dev/console"} {
		if f, err := os.OpenFile(dev, os.O_WRONLY, 0); err == nil {
			_, _ = f.Write([]byte("\x1b[?25h"))
			_ = f.Close()
		}
	}
	_ = os.WriteFile("/sys/class/graphics/fbcon/cursor_blink", []byte("1\n"), 0644)
}

func main() {
	mockPrinter := flag.Bool("mock-printer", false, "Run in mock printer mode on local loopback without physical device")
	previewPNG := flag.String("preview-png", "", "Path to write preview PNG instead of writing to framebuffer")
	cloudURL := flag.String("cloud-url", "ws://127.0.0.1:8080/ws", "Cloud WebSocket hub endpoint")
	kioskID := flag.String("kiosk-id", "kiosk-001", "Kiosk identifier")
	kioskSecret := flag.String("kiosk-secret", os.Getenv("XEROXGO_KIOSK_SECRET"), "Secret key for authenticating with cloud WebSocket hub")
	stateDB := flag.String("state-db", "/var/kiosk/state.db", "SQLite state database path")
	downloadDir := flag.String("download-dir", "/var/kiosk/jobs", "Directory for downloaded PDF jobs")
	printerAddr := flag.String("printer-addr", "192.168.99.2:9100", "Physical printer TCP address")
	snmpTarget := flag.String("snmp-target", "", "SNMP target address (defaults to host from -printer-addr)")
	snmpCommunity := flag.String("snmp-community", "public", "SNMP community string for printer monitoring (read-only)")
	snmpPort := flag.Int("snmp-port", 161, "SNMP port (default 161)")
	cupsPrinter := flag.String("cups-printer", "", "CUPS queue name (e.g. DCPT310). When set, jobs are sent via CUPS instead of raw TCP port 9100")
	cupsQueue := flag.String("cups-queue", "", "Alias for -cups-printer")
	printerModel := flag.String("printer-model", "", "Printer model name displayed on screen (default: Brother DCP-T310 for CUPS, Brother HL-L5210DN for PJL)")
	completedTimeout := flag.Duration("completed-timeout", 7*time.Second, "Duration to display completion screen before returning to idle QR screen")
	framebuffer := flag.String("framebuffer", "/dev/fb0", "Linux framebuffer device path")
	screenRes := flag.String("screen-res", "", "Override screen resolution WIDTHxHEIGHT (e.g. 1280x720). Defaults to auto-detect")
	screenBPP := flag.Int("screen-bpp", 0, "Override screen bits per pixel (e.g. 16 or 32). Defaults to auto-detect")
	idleQRContent := flag.String("idle-qr-content", "", "URL or content rendered in idle screen QR code")
	idleQR := flag.String("idle-qr", "", "Alias for -idle-qr-content")
	paymentURLBase := flag.String("payment-url-base", "https://xeroxgo.vercel.app/payment", "Base URL for payment QR codes (e.g. https://xeroxgo.vercel.app/payment). If non-empty, wraps payment tokens in this URL")
	paymentURL := flag.String("payment-url", "", "Alias for -payment-url-base")
	adsCachePath := flag.String("ads-cache-path", "/var/kiosk/ads_cache.json", "Local cache file path for promotional ads")
	enableCloudRecovery := flag.Bool("enable-cloud-recovery", true, "Enable cloud-owned print recovery and continuation artifacts (disables Pi-side slicer)")
	enableKeypad := flag.Bool("enable-keypad", true, "Enable keyboard/keypad input for PIN entry (default true)")
	keypadDevice := flag.String("keypad-device", "", "Path to keyboard TTY device (e.g. /dev/tty1). If specified, reads from device; otherwise reads from stdin")
	pinInactivityTimeout := flag.Duration("pin-timeout", 30*time.Second, "Inactivity timeout for PIN entry screen before returning to idle")
	orderSummaryTimeout := flag.Duration("summary-timeout", 60*time.Second, "Inactivity timeout for order summary screen before returning to idle")
	paymentPendingTimeout := flag.Duration("payment-timeout", 300*time.Second, "Inactivity timeout for payment pending screen before returning to idle")
	refill := flag.Int("refill", -1, "Reset and calibrate estimated paper tray stock to N sheets (0-250) and exit")
	simulatedStock := flag.Int("simulated-stock", -1, "Initialize and calibrate estimated paper tray stock to N sheets (0-250) for testing/demo")
	viewerAddr := flag.String("viewer-addr", "", "HTTP address to serve the live preview browser viewer (e.g. :8081 or 0.0.0.0:8081). If empty, viewer HTTP server is disabled.")
	enableSound := flag.Bool("enable-sound", true, "Enable sound playback on job completion (degrades gracefully if hardware unavailable)")
	simulatedFailure := flag.String("simulated-failure", "", "Simulated failure condition: none, paper-empty, jam, offline")
	simulatedFailureSheet := flag.Int("simulated-failure-sheet", 0, "Sheet number after which to inject simulated failure (0 = fail at start/health)")
	flag.Parse()

	if *cupsPrinter == "" && *cupsQueue != "" {
		*cupsPrinter = *cupsQueue
	}
	if *idleQRContent == "" && *idleQR != "" {
		*idleQRContent = *idleQR
	}
	if *paymentURL != "" {
		*paymentURLBase = *paymentURL
	}

	if strings.TrimSpace(*kioskID) == "" {
		log.Fatalf("Fatal: --kiosk-id cannot be empty")
	}

	if *idleQRContent == "" {
		u, err := url.Parse(*cloudURL)
		if err == nil && u.Host != "" {
			scheme := "http"
			if u.Scheme == "wss" {
				scheme = "https"
			}
			*idleQRContent = fmt.Sprintf("%s://%s/k/%s", scheme, u.Host, url.PathEscape(*kioskID))
		} else {
			*idleQRContent = "https://xeroxgo.vercel.app"
		}
	}

	if err := os.MkdirAll(filepath.Dir(*stateDB), 0755); err != nil {
		log.Fatalf("Fatal: failed to create state database directory: %v", err)
	}
	if err := os.MkdirAll(*downloadDir, 0700); err != nil {
		log.Fatalf("Fatal: failed to create download directory: %v", err)
	}

	stateStore, err := state.Open(*stateDB)
	if err != nil {
		log.Fatalf("Fatal: failed to open state database at %s: %v", *stateDB, err)
	}
	defer stateStore.Close()

	if *refill >= 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stateStore.ResetPaperTray(ctx, *refill); err != nil {
			log.Fatalf("Fatal: failed to refill paper tray to %d sheets: %v", *refill, err)
		}
		log.Printf("Successfully refilled and calibrated paper tray to %d sheets", *refill)
		return
	}

	if *simulatedStock >= 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stateStore.ResetPaperTray(ctx, *simulatedStock); err != nil {
			log.Fatalf("Fatal: failed to set simulated paper stock: %v", err)
		}
		log.Printf("Initialized simulated paper tray to %d sheets", *simulatedStock)
	} else if *mockPrinter {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, calib, err := stateStore.GetPaperSupply(ctx)
		if err == nil && !calib {
			_ = stateStore.ResetPaperTray(ctx, 250)
			log.Printf("Mock printer mode: initialized calibrated paper stock to 250 sheets")
		}
	}

	var screenSink ui_ipc.Sink
	if *previewPNG != "" {
		if *screenRes != "" {
			parts := strings.Split(*screenRes, "x")
			if len(parts) == 2 {
				if w, err := strconv.Atoi(parts[0]); err == nil && w > 0 {
					if h, err := strconv.Atoi(parts[1]); err == nil && h > 0 {
						ui_ipc.ConfigureTarget(ui_ipc.FramebufferGeometry{Width: w, Height: h, BitsPerPixel: 32, Stride: w * 4})
					}
				}
			}
		}
		if err := os.MkdirAll(filepath.Dir(*previewPNG), 0755); err != nil {
			log.Fatalf("Fatal: failed to create preview png directory: %v", err)
		}
		screenSink = ui_ipc.FilePreviewSink(*previewPNG)
	} else {
		geom := ui_ipc.DetectFramebufferGeometry(*framebuffer)
		if *screenRes != "" {
			parts := strings.Split(*screenRes, "x")
			if len(parts) == 2 {
				if w, err := strconv.Atoi(parts[0]); err == nil && w > 0 {
					geom.Width = w
				}
				if h, err := strconv.Atoi(parts[1]); err == nil && h > 0 {
					geom.Height = h
				}
				bytesPerPixel := geom.BitsPerPixel / 8
				if bytesPerPixel < 1 {
					bytesPerPixel = 2
				}
				geom.Stride = geom.Width * bytesPerPixel
			}
		}
		if *screenBPP > 0 {
			geom.BitsPerPixel = *screenBPP
			bytesPerPixel := geom.BitsPerPixel / 8
			if bytesPerPixel < 1 {
				bytesPerPixel = 2
			}
			geom.Stride = geom.Width * bytesPerPixel
		}
		log.Printf("XeroxGo display: output=%s, resolution=%dx%d, bpp=%d, stride=%d",
			*framebuffer, geom.Width, geom.Height, geom.BitsPerPixel, geom.Stride)
		suppressConsoleCursor()
		ui_ipc.ConfigureTarget(geom)
		screenSink = ui_ipc.FramebufferSinkWithGeometry(*framebuffer, geom)
	}

	var rootWg sync.WaitGroup
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	actualPrinterAddr := *printerAddr
	var mockHealthPoller func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	var cleanupMock func()

	soundPlayer := audio.NewPlayer(*enableSound)

	var mockState *mockPrinterState
	if *mockPrinter {
		mockState = &mockPrinterState{
			failureType:  *simulatedFailure,
			failureSheet: *simulatedFailureSheet,
		}
		addr, cleanup, err := startMockPrinter(ctx, *downloadDir, &rootWg, mockState)
		if err != nil {
			log.Fatalf("Fatal: failed to start mock printer: %v", err)
		}
		cleanupMock = cleanup
		actualPrinterAddr = addr
		log.Printf("XeroxGo kiosk daemon: mock printer active on %s (simulated failure: %q, sheet: %d)", actualPrinterAddr, *simulatedFailure, *simulatedFailureSheet)

		mockHealthPoller = func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			h := contracts.PrinterHealth{
				Online:            true,
				State:             "IDLE",
				PaperTrayStatus:   "OK",
				EstimatedSheets:   estimatedSheets,
				TonerLevelPercent: 95,
				DrumLifePercent:   98,
				UpdatedAt:         time.Now().UTC(),
			}

			if estimatedSheets <= 0 {
				h.PaperTrayStatus = "EMPTY"
			} else if estimatedSheets <= 10 {
				h.PaperTrayStatus = "LOW"
			}

			if mockState != nil && mockState.failureTriggered.Load() {
				switch strings.ToLower(strings.TrimSpace(mockState.failureType)) {
				case "paper-empty":
					h.PaperTrayStatus = "EMPTY"
					h.EstimatedSheets = 0
					h.ErrorMessage = "printer paper tray empty"
				case "jam":
					h.State = "ERROR"
					h.PaperJam = true
					h.ErrorMessage = "printer has a paper jam"
				case "offline":
					h.Online = false
					h.State = "OFFLINE"
					h.ErrorMessage = "printer is offline or disconnected"
				}
			}

			return h
		}
	}

	var viewerServer *viewer.Server
	if strings.TrimSpace(*viewerAddr) != "" {
		vServer, vErr := viewer.NewServer(viewer.Config{
			Addr:        *viewerAddr,
			PreviewPNG:  *previewPNG,
			SoundPlayer: soundPlayer,
			KioskID:     *kioskID,
		})
		if vErr != nil {
			log.Printf("[WARN] Failed to initialize live monitor viewer on %s: %v", *viewerAddr, vErr)
		} else {
			viewerServer = vServer
			rootWg.Add(1)
			go func() {
				defer rootWg.Done()
				vServer.Start(ctx)
			}()
		}
	}

	effectivePrinterModel := strings.TrimSpace(*printerModel)
	if effectivePrinterModel == "" {
		if strings.TrimSpace(*cupsPrinter) != "" {
			effectivePrinterModel = "Brother DCP-T310"
		} else {
			effectivePrinterModel = "Brother HL-L5210DN"
		}
	}

	var inputSource input.Source
	if *enableKeypad || strings.TrimSpace(*keypadDevice) != "" {
		devPath := strings.TrimSpace(*keypadDevice)
		if devPath != "" {
			ttySrc, err := input.OpenTTYSource(devPath)
			if err == nil {
				inputSource = ttySrc
				defer inputSource.Close()
				log.Printf("XeroxGo kiosk daemon: keyboard input enabled on %s (raw mode, echo suppressed)", devPath)
			} else {
				log.Printf("XeroxGo kiosk daemon: warning: could not open %s (%v), falling back to stdin", devPath, err)
			}
		}

		if inputSource == nil {
			if oldTerm, err := input.MakeRaw(int(os.Stdin.Fd())); err == nil {
				defer input.Restore(int(os.Stdin.Fd()), oldTerm)
				log.Printf("XeroxGo kiosk daemon: raw terminal mode active on stdin")
			}
			inputSource = input.NewReaderSource(os.Stdin)
			defer inputSource.Close()
			log.Printf("XeroxGo kiosk daemon: stdin keypad reader enabled")
		}
	}

	clientCfg := spooler.ClientConfig{
		CloudURL:                 *cloudURL,
		KioskID:                  *kioskID,
		KioskSecret:              strings.TrimSpace(*kioskSecret),
		StateStore:               stateStore,
		PrinterAddr:              actualPrinterAddr,
		SNMPTarget:               *snmpTarget,
		SNMPCommunity:            *snmpCommunity,
		SNMPPort:                 *snmpPort,
		CUPSQueue:                *cupsPrinter,
		PrinterModel:             effectivePrinterModel,
		DownloadDir:              *downloadDir,
		IdleQRContent:            *idleQRContent,
		AdsCachePath:             *adsCachePath,
		CompletedDisplayDuration: *completedTimeout,
		ScreenSink:               screenSink,
		HTTPClient:               nil,
		MockHealth:               mockHealthPoller,
		EnableCloudRecovery:      *enableCloudRecovery,
		InputSource:              inputSource,
		InactivityTimeout:        *pinInactivityTimeout,
		SummaryTimeout:           *orderSummaryTimeout,
		PaymentTimeout:           *paymentPendingTimeout,
		PaymentURLBase:           *paymentURLBase,
		SoundPlayer:              soundPlayer,
		OnJobCompleted: func(jobID string) {
			if viewerServer != nil {
				viewerServer.NotifyJobCompleted(jobID)
			}
		},
	}

	client, err := spooler.NewClient(clientCfg)
	if err != nil {
		log.Fatalf("Fatal: failed to create spooler client: %v", err)
	}

	startSystemdWatchdog(ctx, &rootWg)
	_ = sendSystemdNotify("READY=1")

	targetDesc := actualPrinterAddr
	if *cupsPrinter != "" {
		targetDesc = "cups:" + *cupsPrinter
	}
	log.Printf("XeroxGo kiosk daemon started for kiosk %s (cloud=%s, target=%s, model=%s)", *kioskID, *cloudURL, targetDesc, effectivePrinterModel)

	if err := client.Run(ctx); err != nil {
		log.Printf("spooler client exited with error: %v", err)
	}

	cancel()
	if cleanupMock != nil {
		cleanupMock()
	}
	rootWg.Wait()
	restoreConsoleCursor()
	log.Printf("XeroxGo kiosk daemon shut down cleanly.")
}
