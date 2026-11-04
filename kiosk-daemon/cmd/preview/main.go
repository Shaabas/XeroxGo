package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"

	"xeroxgo/kiosk-daemon/internal/ui_ipc"
)

func main() {
	outDir := "previews"
	if err := os.MkdirAll(outDir, 0755); err != nil {
		panic(err)
	}

	states := []struct {
		name  string
		state ui_ipc.ScreenState
	}{
		{
			name: "01_idle.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeIdle,
				QRContent:    "https://frontend-chi-nine-50.vercel.app/k/kiosk-demo",
				PrinterModel: "Brother DCP-T310",
				AdSlideIndex: 0,
			},
		},
		{
			name: "01_idle_slide2.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeIdle,
				QRContent:    "https://frontend-chi-nine-50.vercel.app/k/kiosk-demo",
				PrinterModel: "Brother DCP-T310",
				AdSlideIndex: 1,
			},
		},
		{
			name: "01_idle_slide3.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeIdle,
				QRContent:    "https://frontend-chi-nine-50.vercel.app/k/kiosk-demo",
				PrinterModel: "Brother DCP-T310",
				AdSlideIndex: 2,
			},
		},
		{
			name: "01_idle_transition.png",
			state: ui_ipc.ScreenState{
				Mode:           ui_ipc.ModeIdle,
				QRContent:      "https://frontend-chi-nine-50.vercel.app/k/kiosk-demo",
				PrinterModel:   "Brother DCP-T310",
				AdSlideIndex:   1,
				TransitionFrom: 0,
				TransitionProg: 0.5,
			},
		},
		{
			name: "02_downloading.png",
			state: ui_ipc.ScreenState{
				Mode:            ui_ipc.ModeDownloading,
				ProgressPercent: 68,
				Message:         "Receiving print job securely from cloud server...",
				Detail:          "Job ID: xg-order-883921 • Document: Lecture_Notes_CS101.pdf",
				PrinterModel:    "Brother DCP-T310",
			},
		},
		{
			name: "03_printing.png",
			state: ui_ipc.ScreenState{
				Mode:            ui_ipc.ModePrinting,
				CurrentPage:     2,
				TotalPages:      5,
				Message:         "Please wait while your document prints...",
				Detail:          "Job ID: xg-order-883921 • Duplex Simplex • 1 Copy",
				PrinterModel:    "Brother DCP-T310",
			},
		},
		{
			name: "04_completed.png",
			state: ui_ipc.ScreenState{
				Mode:            ui_ipc.ModeCompleted,
				CurrentPage:     5,
				TotalPages:      5,
				ProgressPercent: 100,
				Message:         "All 5 pages have been streamed and printed successfully.",
				Detail:          "Job ID: xg-order-883921 • Total Sheets: 5",
				PrinterModel:    "Brother DCP-T310",
			},
		},
		{
			name: "05_error.png",
			state: ui_ipc.ScreenState{
				Mode:         ui_ipc.ModeError,
				Message:      "Printer paper tray 1 is empty or disconnected.",
				Detail:       "Error Code: CUPS_MEDIA_EMPTY • Job ID: xg-order-883921",
				PrinterModel: "Brother DCP-T310",
			},
		},
	}

	for _, s := range states {
		img, err := ui_ipc.Render(s.state)
		if err != nil {
			panic(fmt.Sprintf("render %s failed: %v", s.name, err))
		}
		path := filepath.Join(outDir, s.name)
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, img); err != nil {
			panic(err)
		}
		_ = f.Close()
		fmt.Printf("Generated %s\n", path)
	}
}
