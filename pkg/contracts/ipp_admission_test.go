package contracts

import (
	"testing"
	"time"
)

func baseAvailableHealth(now time.Time) PrinterHealth {
	return PrinterHealth{
		Online:                 true,
		State:                  "IDLE",
		EvaluatedHealth:        EvaluatedHealthHealthy,
		NativeIPPAdmission:     AcceptingJobsAccepting,
		PaperTrayStatus:        "OK",
		EstimatedSheets:        100,
		ConfiguredTrayCapacity: 250,
		InventoryCalibrated:    true,
		ExecutionSafe:          true,
		TonerLevelPercent:      85,
		TonerLevelKnown:        true,
		DrumLifePercent:        90,
		DrumLifeKnown:          true,
		InputTrayLevel:         100,
		InputTrayLevelKnown:    true,
		InputTrayCapacity:      250,
		InputTrayCapacityKnown: true,
		EvidenceFreshness:      BuildUniformEvidenceFreshness(now, DefaultTelemetryMaxAge),
		UpdatedAt:              now,
	}
}

func TestNativeIPPAdmission_ValidateReadyAndIsHealthAvailable(t *testing.T) {
	now := time.Now().UTC()

	t.Run("IDLE_ACCEPTING_is_ready_and_available", func(t *testing.T) {
		h := baseAvailableHealth(now)
		if ok, reason := ValidateReadyHealth(h, now, DefaultTelemetryMaxAge); !ok {
			t.Fatalf("expected ValidateReadyHealth true, got %q", reason)
		}
		if ok, reason := IsHealthAvailable(h, now, DefaultTelemetryMaxAge); !ok {
			t.Fatalf("expected IsHealthAvailable true, got %q", reason)
		}
	})

	t.Run("IDLE_NOT_ACCEPTING_is_ready_for_dispatch_sync_but_unavailable_for_customers", func(t *testing.T) {
		h := baseAvailableHealth(now)
		h.State = "IDLE"
		h.NativeIPPAdmission = AcceptingJobsNotAccepting
		if ok, reason := ValidateReadyHealth(h, now, DefaultTelemetryMaxAge); !ok {
			t.Fatalf("expected ValidateReadyHealth true for healthy IDLE+NOT_ACCEPTING, got %q", reason)
		}
		if ok, _ := IsHealthAvailable(h, now, DefaultTelemetryMaxAge); ok {
			t.Fatal("expected IsHealthAvailable false for IDLE+NOT_ACCEPTING")
		}
	})

	t.Run("PRINTING_NOT_ACCEPTING_is_ready_and_available", func(t *testing.T) {
		h := baseAvailableHealth(now)
		h.State = "PRINTING"
		h.NativeIPPAdmission = AcceptingJobsNotAccepting
		if ok, reason := ValidateReadyHealth(h, now, DefaultTelemetryMaxAge); !ok {
			t.Fatalf("expected ValidateReadyHealth true for healthy PRINTING+NOT_ACCEPTING, got %q", reason)
		}
		if ok, reason := IsHealthAvailable(h, now, DefaultTelemetryMaxAge); !ok {
			t.Fatalf("expected IsHealthAvailable true for healthy PRINTING+NOT_ACCEPTING, got %q", reason)
		}
	})

	t.Run("UNKNOWN_or_missing_or_invalid_admission_is_never_available", func(t *testing.T) {
		for _, val := range []AcceptingJobsStatus{"", AcceptingJobsUnknown, AcceptingJobsStatus("BOGUS")} {
			h := baseAvailableHealth(now)
			h.NativeIPPAdmission = val
			if got := h.EffectiveNativeIPPAdmission(); got != AcceptingJobsUnknown {
				t.Fatalf("EffectiveNativeIPPAdmission(%q) = %q, want UNKNOWN", val, got)
			}
			if ok, _ := IsHealthAvailable(h, now, DefaultTelemetryMaxAge); ok {
				t.Fatalf("expected IsHealthAvailable false for NativeIPPAdmission=%q", val)
			}
		}
	})
}
