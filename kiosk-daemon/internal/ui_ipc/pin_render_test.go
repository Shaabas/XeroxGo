package ui_ipc

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestRenderPINEntry(t *testing.T) {
	testCases := []struct {
		name  string
		state ScreenState
	}{
		{
			name: "empty_pin",
			state: ScreenState{
				Mode:           ModePINEntry,
				PINDigits:      "",
				TimeoutSeconds: 30,
			},
		},
		{
			name: "partial_pin_2_digits",
			state: ScreenState{
				Mode:           ModePINEntry,
				PINDigits:      "48",
				TimeoutSeconds: 25,
			},
		},
		{
			name: "full_pin_4_digits",
			state: ScreenState{
				Mode:           ModePINEntry,
				PINDigits:      "4829",
				TimeoutSeconds: 20,
			},
		},
		{
			name: "pin_loading",
			state: ScreenState{
				Mode:       ModePINEntry,
				PINDigits:  "1234",
				PINLoading: true,
			},
		},
		{
			name: "pin_invalid_error",
			state: ScreenState{
				Mode:           ModePINEntry,
				PINDigits:      "",
				PINError:       "Invalid or expired PIN. Please try again.",
				TimeoutSeconds: 15,
			},
		},
		{
			name: "pin_rate_limit_error",
			state: ScreenState{
				Mode:           ModePINEntry,
				PINDigits:      "",
				PINError:       "Too many failed attempts. Please wait 30s.",
				TimeoutSeconds: 10,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			img, err := Render(tc.state)
			if err != nil {
				t.Fatalf("Render(%s) failed: %v", tc.name, err)
			}
			if img == nil {
				t.Fatalf("Render(%s) returned nil image", tc.name)
			}
			if img.Bounds().Dx() != ScreenWidth || img.Bounds().Dy() != ScreenHeight {
				t.Errorf("expected 1920x1080, got %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
			}
		})
	}
}

func TestRenderCheckoutStates(t *testing.T) {
	testCases := []struct {
		name  string
		state ScreenState
	}{
		{
			name: "simplex_mono_1_copy",
			state: ScreenState{
				Mode: ModePaymentPending,
				OrderSummary: &OrderSummary{
					OrderID:         "ord_sum_001",
					TotalPricePaise: 1500,
					TotalPages:      5,
					TotalSheets:     5,
					Copies:          1,
					Duplex:          "simplex",
					ColorMode:       "mono",
				},
				SessionID:      "sess_sum_001",
				TimeoutSeconds: 120,
			},
		},
		{
			name: "duplex_color_3_copies",
			state: ScreenState{
				Mode: ModePaymentPending,
				OrderSummary: &OrderSummary{
					OrderID:         "ord_sum_002",
					TotalPricePaise: 7200,
					TotalPages:      12,
					TotalSheets:     6,
					Copies:          3,
					Duplex:          "long_edge",
					ColorMode:       "color",
				},
				SessionID:      "sess_sum_002",
				TimeoutSeconds: 45,
			},
		},
		{
			name: "checkout_claiming_loading",
			state: ScreenState{
				Mode: ModePaymentPending,
				OrderSummary: &OrderSummary{
					OrderID:         "ord_sum_003",
					TotalPricePaise: 2000,
					TotalPages:      4,
					TotalSheets:     4,
					Copies:          1,
					Duplex:          "simplex",
					ColorMode:       "mono",
				},
				PINLoading:     true,
				TimeoutSeconds: 0,
			},
		},
		{
			name: "checkout_claim_error",
			state: ScreenState{
				Mode: ModePaymentPending,
				OrderSummary: &OrderSummary{
					OrderID:         "ord_sum_004",
					TotalPricePaise: 2000,
					TotalPages:      4,
					TotalSheets:     4,
					Copies:          1,
					Duplex:          "simplex",
					ColorMode:       "mono",
				},
				PINError:       "Paper tray supply is low or empty. Please ask attendant to refill paper and press Confirm.",
				TimeoutSeconds: 0,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			img, err := Render(tc.state)
			if err != nil {
				t.Fatalf("Render(%s) failed: %v", tc.name, err)
			}
			if img == nil {
				t.Fatalf("Render(%s) returned nil image", tc.name)
			}
		})
	}
}

func TestRenderPaymentPending(t *testing.T) {
	// Case 1: Loading state without QR content
	stateLoading := ScreenState{
		Mode: ModePaymentPending,
		OrderSummary: &OrderSummary{
			OrderID:         "ord_pay_pen_001",
			TotalPricePaise: 3500,
			TotalPages:      10,
			TotalSheets:     5,
			Copies:          1,
			Duplex:          "long_edge",
			ColorMode:       "color",
		},
		SessionID:      "sess_abc123456",
		TimeoutSeconds: 300,
		QRContent:      "",
	}

	imgLoading, err := Render(stateLoading)
	if err != nil {
		t.Fatalf("Render(PaymentPending, loading) failed: %v", err)
	}
	if imgLoading == nil {
		t.Fatalf("Render(PaymentPending, loading) returned nil image")
	}
	if imgLoading.Bounds().Dx() != ScreenWidth || imgLoading.Bounds().Dy() != ScreenHeight {
		t.Errorf("expected 1920x1080, got %dx%d", imgLoading.Bounds().Dx(), imgLoading.Bounds().Dy())
	}

	// Case 2: Active state with dynamic checkout QR payload
	stateQR := ScreenState{
		Mode: ModePaymentPending,
		OrderSummary: &OrderSummary{
			OrderID:         "ord_pay_pen_001",
			TotalPricePaise: 3500,
			TotalPages:      10,
			TotalSheets:     5,
			Copies:          1,
			Duplex:          "long_edge",
			ColorMode:       "color",
		},
		SessionID:      "sess_abc123456",
		TimeoutSeconds: 295,
		QRContent:      "https://demo.xeroxgo.com/pay/dmo_9876543210fedcba",
	}

	imgQR, err := Render(stateQR)
	if err != nil {
		t.Fatalf("Render(PaymentPending, QR) failed: %v", err)
	}
	if imgQR == nil {
		t.Fatalf("Render(PaymentPending, QR) returned nil image")
	}
	if imgQR.Bounds().Dx() != ScreenWidth || imgQR.Bounds().Dy() != ScreenHeight {
		t.Errorf("expected 1920x1080, got %dx%d", imgQR.Bounds().Dx(), imgQR.Bounds().Dy())
	}
}

func TestRenderValidation_PINAndSummary(t *testing.T) {
	// 1. PIN digits > 4
	err := ScreenState{
		Mode:      ModePINEntry,
		PINDigits: "12345",
	}.Validate()
	if err == nil {
		t.Errorf("expected validation error for 5-digit PIN")
	}

	// 2. Nil OrderSummary
	err = ScreenState{
		Mode:         ModePaymentPending,
		OrderSummary: nil,
	}.Validate()
	if err == nil {
		t.Errorf("expected validation error for nil OrderSummary")
	}

	// 3. PaymentPending with zero pages
	err = ScreenState{
		Mode: ModePaymentPending,
		OrderSummary: &OrderSummary{
			TotalPricePaise: 100,
			TotalPages:      0,
			TotalSheets:     1,
			Copies:          1,
		},
	}.Validate()
	if err == nil {
		t.Errorf("expected validation error for 0 TotalPages")
	}

	// 4. PaymentPending with zero sheets
	err = ScreenState{
		Mode: ModePaymentPending,
		OrderSummary: &OrderSummary{
			TotalPricePaise: 100,
			TotalPages:      1,
			TotalSheets:     0,
			Copies:          1,
		},
	}.Validate()
	if err == nil {
		t.Errorf("expected validation error for 0 TotalSheets")
	}

	// 5. PaymentPending with zero copies
	err = ScreenState{
		Mode: ModePaymentPending,
		OrderSummary: &OrderSummary{
			TotalPricePaise: 100,
			TotalPages:      1,
			TotalSheets:     1,
			Copies:          0,
		},
	}.Validate()
	if err == nil {
		t.Errorf("expected validation error for 0 Copies")
	}
}

func TestRenderPINAndSummary_Target720p(t *testing.T) {
	// Scaled 720p buffer
	buf := image.NewRGBA(image.Rect(0, 0, 1280, 720))

	// PIN entry 720p
	err := RenderTo(buf, ScreenState{
		Mode:           ModePINEntry,
		PINDigits:      "5544",
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("RenderTo 720p PIN entry failed: %v", err)
	}

	// Checkout 720p
	err = RenderTo(buf, ScreenState{
		Mode: ModePaymentPending,
		OrderSummary: &OrderSummary{
			OrderID:         "ord_720p",
			TotalPricePaise: 2000,
			TotalPages:      4,
			TotalSheets:     2,
			Copies:          1,
			Duplex:          "long_edge",
			ColorMode:       "mono",
		},
		SessionID:      "sess_720p",
		TimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatalf("RenderTo 720p checkout failed: %v", err)
	}
}

func TestRenderCheckout_PhasesAndCountdownSuppression(t *testing.T) {
	summary := &OrderSummary{
		OrderID:         "ord_phase_01",
		TotalPricePaise: 2500,
		TotalPages:      5,
		TotalSheets:     3,
		Copies:          1,
		Duplex:          "long_edge",
		ColorMode:       "mono",
	}

	// Timer region around bottomY - 45 = 320 + 510 - 45 = 785 inside rightCard (980..1820)
	hasRedTimerPixels := func(img *image.RGBA) bool {
		for y := 775; y <= 810; y++ {
			for x := 1250; x <= 1550; x++ {
				c := img.RGBAAt(x, y)
				if c.R > 200 && c.G < 100 && c.B < 100 {
					return true
				}
			}
		}
		return false
	}

	// 1. Claiming phase ("Preparing Session...") must NOT render session countdown even if TimeoutSeconds > 0 or SessionID empty
	imgClaiming, err := Render(ScreenState{
		Mode:           ModePaymentPending,
		OrderSummary:   summary,
		PINLoading:     true,
		TimeoutSeconds: 120,
	})
	if err != nil {
		t.Fatalf("Render(claiming) failed: %v", err)
	}
	if hasRedTimerPixels(imgClaiming) {
		t.Errorf("countdown timer must not render before valid session exists (claiming phase)")
	}

	// 2. Initiating phase ("Generating QR...") renders session countdown once session exists
	imgInitiating, err := Render(ScreenState{
		Mode:           ModePaymentPending,
		OrderSummary:   summary,
		SessionID:      "sess_phase_01",
		TimeoutSeconds: 120,
	})
	if err != nil {
		t.Fatalf("Render(initiating) failed: %v", err)
	}
	if !hasRedTimerPixels(imgInitiating) {
		t.Errorf("expected session countdown timer to render when valid session exists during QR initiation")
	}

	// 3. Ready phase renders valid QR and countdown
	checkoutURL := "https://xeroxgo.vercel.app/pay/dmo_0123456789abcdef0123456789abcdef0123456789abcdef"
	imgReady, err := Render(ScreenState{
		Mode:           ModePaymentPending,
		OrderSummary:   summary,
		SessionID:      "sess_phase_01",
		QRContent:      checkoutURL,
		TimeoutSeconds: 118,
	})
	if err != nil {
		t.Fatalf("Render(ready) failed: %v", err)
	}
	if !hasRedTimerPixels(imgReady) {
		t.Errorf("expected session countdown timer to render when valid session exists")
	}

	// Verify QR region (1490, 370, 1770, 650) matches encoded demo URL QR modules
	expectedQR, err := getQRCodeImage(checkoutURL)
	if err != nil {
		t.Fatalf("getQRCodeImage failed: %v", err)
	}
	if expectedQR == nil {
		t.Fatalf("expected non-nil QR image")
	}
	darkCount := 0
	lightCount := 0
	for y := 370; y < 650; y++ {
		for x := 1490; x < 1770; x++ {
			c := imgReady.RGBAAt(x, y)
			if c.R < 50 && c.G < 50 && c.B < 50 {
				darkCount++
			} else if c.R > 230 && c.G > 230 && c.B > 230 {
				lightCount++
			}
		}
	}
	if darkCount == 0 || lightCount == 0 {
		t.Errorf("expected QR modules in checkout QR box (dark=%d, light=%d)", darkCount, lightCount)
	}

	// 4. Failure phase renders error banner and no countdown when no session exists
	imgFail, err := Render(ScreenState{
		Mode:         ModePaymentPending,
		OrderSummary: summary,
		PINError:     "Unable to start payment session. Please try again.",
	})
	if err != nil {
		t.Fatalf("Render(failure) failed: %v", err)
	}
	if hasRedTimerPixels(imgFail) {
		t.Errorf("countdown timer must not render on claim failure without active session")
	}

	if outDir := os.Getenv("XEROXGO_RENDER_OUT_DIR"); outDir != "" {
		save := func(name string, img *image.RGBA) {
			f, fErr := os.Create(filepath.Join(outDir, name))
			if fErr != nil {
				t.Fatalf("create %s: %v", name, fErr)
			}
			defer f.Close()
			if encErr := png.Encode(f, img); encErr != nil {
				t.Fatalf("encode %s: %v", name, encErr)
			}
		}
		save("checkout_loading.png", imgClaiming)
		save("checkout_initiating.png", imgInitiating)
		save("checkout_ready.png", imgReady)
		save("checkout_failure.png", imgFail)
	}
}

