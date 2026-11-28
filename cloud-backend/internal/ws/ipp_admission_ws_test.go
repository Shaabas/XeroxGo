package ws

import (
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestValidateIncomingHealthPayload_NativeIPPAdmission(t *testing.T) {
	now := time.Now().UTC()
	base := func(adm contracts.AcceptingJobsStatus) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			Online:                 true,
			State:                  "IDLE",
			EvaluatedHealth:        contracts.EvaluatedHealthHealthy,
			NativeIPPAdmission:     adm,
			PaperTrayStatus:        "OK",
			EstimatedSheets:        100,
			ConfiguredTrayCapacity: 250,
			InventoryCalibrated:    true,
			ExecutionSafe:          true,
			TonerLevelPercent:      90,
			TonerLevelKnown:        true,
			DrumLifePercent:        90,
			DrumLifeKnown:          true,
			EvidenceFreshness:      contracts.BuildUniformEvidenceFreshness(now, contracts.DefaultTelemetryMaxAge),
			UpdatedAt:              now,
		}
	}

	for _, valid := range []contracts.AcceptingJobsStatus{"", contracts.AcceptingJobsUnknown, contracts.AcceptingJobsAccepting, contracts.AcceptingJobsNotAccepting} {
		h := base(valid)
		if err := validateHealthPayload(&h, now); err != nil {
			t.Fatalf("expected valid NativeIPPAdmission %q to pass, got %v", valid, err)
		}
	}

	invalid := base(contracts.AcceptingJobsStatus("INVALID_STATE"))
	if err := validateHealthPayload(&invalid, now); err == nil {
		t.Fatal("expected invalid NativeIPPAdmission to be rejected")
	}

	printingNotAcc := base(contracts.AcceptingJobsNotAccepting)
	printingNotAcc.State = "PRINTING"
	entry := &sessionHealthEntry{health: printingNotAcc, receivedAt: now}
	if ok, reason := isHealthEntryReady(entry, now); !ok {
		t.Fatalf("expected isHealthEntryReady true for PRINTING+NOT_ACCEPTING, got %q", reason)
	}
}
