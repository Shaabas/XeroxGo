package main

import (
	"fmt"
	"runtime"
	"time"
	"xeroxgo/pkg/contracts"
)

func main() {
	health := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   250,
		TonerLevelPercent: 100,
		UpdatedAt:         time.Now(),
	}

	fmt.Printf("XeroxGo Kiosk Daemon initialized on %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Brother HL-L5210DN Status: %+v\n", health)
}
