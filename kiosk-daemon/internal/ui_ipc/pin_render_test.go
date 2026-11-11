package ui_ipc

import (
	"image"
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

func TestRenderOrderSummary(t *testing.T) {
	testCases := []struct {
		name  string
		state ScreenState
	}{
		{
			name: "simplex_mono_1_copy",
			state: ScreenState{
				Mode: ModeOrderSummary,
				OrderSummary: &OrderSummary{
					OrderID:         "ord_sum_001",
					TotalPricePaise: 1500,
					TotalPages:      5,
					TotalSheets:     5,
					Copies:          1,
					Duplex:          "simplex",
					ColorMode:       "mono",
				},
				TimeoutSeconds: 60,
			},
		},
		{
			name: "duplex_color_3_copies",
			state: ScreenState{
				Mode: ModeOrderSummary,
				OrderSummary: &OrderSummary{
					OrderID:         "ord_sum_002",
					TotalPricePaise: 7200,
					TotalPages:      12,
					TotalSheets:     6,
					Copies:          3,
					Duplex:          "long_edge",
					ColorMode:       "color",
				},
				TimeoutSeconds: 45,
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
	state := ScreenState{
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
	}

	img, err := Render(state)
	if err != nil {
		t.Fatalf("Render(PaymentPending) failed: %v", err)
	}
	if img == nil {
		t.Fatalf("Render(PaymentPending) returned nil image")
	}
	if img.Bounds().Dx() != ScreenWidth || img.Bounds().Dy() != ScreenHeight {
		t.Errorf("expected 1920x1080, got %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
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
		Mode:         ModeOrderSummary,
		OrderSummary: nil,
	}.Validate()
	if err == nil {
		t.Errorf("expected validation error for nil OrderSummary")
	}

	// 3. OrderSummary with zero pages
	err = ScreenState{
		Mode: ModeOrderSummary,
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

	// 4. OrderSummary with zero sheets
	err = ScreenState{
		Mode: ModeOrderSummary,
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

	// 5. OrderSummary with zero copies
	err = ScreenState{
		Mode: ModeOrderSummary,
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

	// Order summary 720p
	err = RenderTo(buf, ScreenState{
		Mode: ModeOrderSummary,
		OrderSummary: &OrderSummary{
			OrderID:         "ord_720p",
			TotalPricePaise: 2000,
			TotalPages:      4,
			TotalSheets:     2,
			Copies:          1,
			Duplex:          "long_edge",
			ColorMode:       "mono",
		},
		TimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatalf("RenderTo 720p Order summary failed: %v", err)
	}
}

