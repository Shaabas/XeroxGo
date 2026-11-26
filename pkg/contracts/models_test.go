package contracts_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestJSONRoundTrip(t *testing.T) {
	timestamp := time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		input     interface{}
		newOutput func() interface{}
	}{
		{
			name: "printer_health",
			input: contracts.PrinterHealth{
				Online:                true,
				State:                 "IDLE",
				EvaluatedHealth:       contracts.EvaluatedHealthHealthy,
				PaperTrayStatus:       "OK",
				EstimatedSheets:       250,
				InventoryCalibrated:   true,
				ExecutionSafe:         true,
				ExecutionSafetyReason: "",
				TonerLevelPercent:     90,
				TonerLevelKnown:       true,
				DrumLifePercent:       -1,
				DrumLifeKnown:         false,
				InputTrayLevel:        -1,
				InputTrayLevelKnown:   false,
				InputTrayCapacity:     150,
				InputTrayCapacityKnown: true,
				CoverOpen:             false,
				PaperJam:              false,
				OutputBinFull:         false,
				Warnings: []contracts.HealthReason{
					{Code: "toner_low", Source: "snmp", Message: "low toner"},
				},
				EvidenceFreshness: &contracts.HealthEvidenceFreshness{
					CUPS:      contracts.SourceFreshness{Available: true, ObservedAt: &timestamp, AgeSeconds: 0.4, Fresh: true},
					NativeIPP: contracts.SourceFreshness{Available: true, ObservedAt: &timestamp, AgeSeconds: 0.4, Fresh: true},
					SNMP:      contracts.SourceFreshness{Available: true, ObservedAt: &timestamp, AgeSeconds: 0.5, Fresh: true},
					MaxAgeSec: 6,
				},
				UpdatedAt: timestamp,
			},
			newOutput: func() interface{} { return &contracts.PrinterHealth{} },
		},
		{
			name: "print_job_command",
			input: contracts.PrintJobCommand{
				JobID:          "job-001",
				OrderID:        "ord-001",
				DocumentIndex:  1,
				TotalDocuments: 3,
				OriginalFilename: "Chapter1.pdf",
				ColorMode:      "COLOR",
				KioskID:        "kiosk-001",
				DisplayCode:    "A74X",
				DownloadURL:    "https://example.invalid/document.pdf",
				SHA256Checksum: "0123456789abcdef",
				TotalPages:     4,
				TotalSheets:    2,
				Copies:         1,
				Duplex:         contracts.DuplexLongEdge,
				StartFromPage:  1,
			},
			newOutput: func() interface{} { return &contracts.PrintJobCommand{} },
		},
		{
			name: "job_progress_event",
			input: contracts.JobProgressEvent{
				JobID:         "job-001",
				OrderID:       "ord-001",
				KioskID:       "kiosk-001",
				Status:        contracts.JobStatusPrinting,
				PagesPrinted:  2,
				SheetsPrinted: 1,
				Timestamp:     timestamp,
			},
			newOutput: func() interface{} { return &contracts.JobProgressEvent{} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(tt.input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			got := tt.newOutput()
			if err := json.Unmarshal(encoded, got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			decoded := reflect.ValueOf(got).Elem().Interface()
			if !reflect.DeepEqual(tt.input, decoded) {
				t.Fatalf(
					"round-trip mismatch:\noriginal: %#v\ndecoded:  %#v\nJSON:     %s",
					tt.input,
					decoded,
					encoded,
				)
			}
		})
	}
}
