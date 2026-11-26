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

func TestValidateHealthEvidenceFreshness_AndStaggeredExpiry(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Second)
	nearlyExpiredCUPS := now.Add(-5500 * time.Millisecond)

	base := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		EvaluatedHealth:   contracts.EvaluatedHealthHealthy,
		PaperTrayStatus:   "OK",
		ExecutionSafe:     true,
		EvidenceFreshness: contracts.BuildUniformEvidenceFreshness(fresh, contracts.DefaultTelemetryMaxAge),
		UpdatedAt:         fresh,
	}

	if ok, oldest, reason := contracts.ValidateHealthEvidenceFreshness(base, now, contracts.DefaultTelemetryMaxAge); !ok || !oldest.Equal(fresh) {
		t.Fatalf("expected uniform fresh health valid, got ok=%v oldest=%v reason=%s", ok, oldest, reason)
	}
	if ok, reason := contracts.ValidateReadyHealth(base, now, contracts.DefaultTelemetryMaxAge); !ok {
		t.Fatalf("expected ValidateReadyHealth true, got false (%s)", reason)
	}

	// Staggered sources: CUPS is 5.5s old while IPP/SNMP are 1s old.
	staggered := base
	staggered.EvidenceFreshness = &contracts.HealthEvidenceFreshness{
		CUPS:      contracts.SourceFreshness{Available: true, ObservedAt: &nearlyExpiredCUPS, Fresh: true},
		NativeIPP: contracts.SourceFreshness{Available: true, ObservedAt: &fresh, Fresh: true},
		SNMP:      contracts.SourceFreshness{Available: true, ObservedAt: &fresh, Fresh: true},
		MaxAgeSec: 6,
	}
	staggered.UpdatedAt = nearlyExpiredCUPS
	if ok, oldest, reason := contracts.ValidateHealthEvidenceFreshness(staggered, now, contracts.DefaultTelemetryMaxAge); !ok || !oldest.Equal(nearlyExpiredCUPS) {
		t.Fatalf("expected staggered 5.5s sample valid at now, got ok=%v reason=%s", ok, reason)
	}
	if ok, _, _ := contracts.ValidateHealthEvidenceFreshness(staggered, now.Add(600*time.Millisecond), contracts.DefaultTelemetryMaxAge); ok {
		t.Fatal("expected staggered 5.5s CUPS source to expire at now+600ms")
	}

	// UpdatedAt renewed to `fresh` while CUPS is 5.5s old must be rejected.
	inflated := staggered
	inflated.UpdatedAt = fresh
	if ok, _, _ := contracts.ValidateHealthEvidenceFreshness(inflated, now, contracts.DefaultTelemetryMaxAge); ok {
		t.Fatal("expected UpdatedAt newer than oldest required source to be rejected")
	}

	// Missing NativeIPP evidence_freshness cannot establish HEALTHY readiness.
	missingIPP := base
	missingIPP.EvidenceFreshness = &contracts.HealthEvidenceFreshness{
		CUPS:      contracts.SourceFreshness{Available: true, ObservedAt: &fresh, Fresh: true},
		NativeIPP: contracts.SourceFreshness{Available: false, ObservedAt: &fresh, Fresh: false, Error: "unreachable"},
		SNMP:      contracts.SourceFreshness{Available: true, ObservedAt: &fresh, Fresh: true},
		MaxAgeSec: 6,
	}
	if ok, _ := contracts.ValidateReadyHealth(missingIPP, now, contracts.DefaultTelemetryMaxAge); ok {
		t.Fatal("expected ValidateReadyHealth to reject HEALTHY missing NativeIPP evidence")
	}
}
