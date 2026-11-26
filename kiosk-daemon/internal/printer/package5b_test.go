package printer

import (
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestPackage5B_SNMPTrayCapacityContradiction(t *testing.T) {
	now := time.Now().UTC()
	res := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
		CUPS: contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			UpdatedAt:       now,
		},
		CUPSQueried: true,
		IPP: IPPPrinterStateResult{
			Source:            CapabilitySourceNativeIPP,
			EndpointReachable: true,
			State:             IPPPrinterStateIdle,
			AcceptingJobs:     AcceptingJobsAccepting,
			ObservedAt:        now,
		},
		SNMP: SNMPObservation{
			Available:  true,
			ObservedAt: now,
			Health: contracts.PrinterHealth{
				Online:                 true,
				State:                  "IDLE",
				PaperTrayStatus:        "OK",
				InputTrayCapacity:      250,
				InputTrayCapacityKnown: true,
				UpdatedAt:              now,
			},
		},
		SNMPQueried:            true,
		RequireCUPS:            true,
		RequireIPP:             true,
		RequireSNMP:            true,
		RequireNativeIPP:       true,
		EvalTime:               now,
		ConfiguredTrayCapacity: 150,
	})

	if len(res.Contradictions) == 0 {
		t.Fatalf("expected contradiction when SNMP InputTrayCapacity (250) differs from ConfiguredTrayCapacity (150)")
	}
	contract := res.ToContract(100, true, true, "")
	if contract.InputTrayCapacity != 250 {
		t.Fatalf("expected InputTrayCapacity to remain 250, got %d", contract.InputTrayCapacity)
	}
	if len(contract.Warnings) == 0 {
		t.Fatalf("expected warning surfaced for SNMP tray capacity contradiction")
	}
}

func TestPackage5B_CapabilityPolicyAndHardwareValidation(t *testing.T) {
	policy := DefaultDemoCapabilityPolicy()
	hwA4Driverless := &PrinterCapabilities{
		Source:            CapabilitySourceCUPSQueue,
		A4Support:         CapabilitySupported,
		SimplexSupport:    CapabilitySupported,
		MonochromeSupport: CapabilitySupported,
		PDFSupport:        CapabilityUnsupported, // CUPS driverless queue converts PDF
	}

	validCmd := contracts.PrintJobCommand{
		JobID:     "job-5b-ok",
		ColorMode: "BW",
		Duplex:    contracts.DuplexSimplex,
		Copies:    3,
	}
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, hwA4Driverless); err != nil {
		t.Fatalf("expected valid BW/Simplex/multi-copy job on A4 driverless printer to pass, got: %v", err)
	}

	// Reject COLOR without silent conversion
	colorCmd := validCmd
	colorCmd.ColorMode = "COLOR"
	if err := ValidateJobAgainstPolicyAndHardware(colorCmd, policy, hwA4Driverless); err == nil {
		t.Fatalf("expected COLOR job to be rejected under demo policy")
	}

	// Reject DUPLEX without silent conversion
	duplexCmd := validCmd
	duplexCmd.Duplex = contracts.DuplexLongEdge
	if err := ValidateJobAgainstPolicyAndHardware(duplexCmd, policy, hwA4Driverless); err == nil {
		t.Fatalf("expected DUPLEX_LONG_EDGE job to be rejected under demo policy")
	}

	// Reject unknown A4 hardware capability
	hwUnknownA4 := *hwA4Driverless
	hwUnknownA4.A4Support = CapabilityUnknown
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, &hwUnknownA4); err == nil {
		t.Fatalf("expected unknown A4 capability to be rejected")
	}

	// Reject unsupported A4 hardware capability
	hwNoA4 := *hwA4Driverless
	hwNoA4.A4Support = CapabilityUnsupported
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, &hwNoA4); err == nil {
		t.Fatalf("expected unsupported A4 capability to be rejected")
	}

	// 1. Missing discovery / nil capabilities
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, nil); err == nil {
		t.Fatalf("expected nil capabilities to be rejected")
	}

	// 2. Empty capability evidence / absent discovery source
	hwEmpty := &PrinterCapabilities{}
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, hwEmpty); err == nil {
		t.Fatalf("expected empty capabilities to be rejected")
	}

	// 3. Unknown monochrome capability
	hwUnknownMono := *hwA4Driverless
	hwUnknownMono.MonochromeSupport = CapabilityUnknown
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, &hwUnknownMono); err == nil {
		t.Fatalf("expected unknown monochrome capability to be rejected")
	}

	// 4. Unknown simplex capability
	hwUnknownSimplex := *hwA4Driverless
	hwUnknownSimplex.SimplexSupport = CapabilityUnknown
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, &hwUnknownSimplex); err == nil {
		t.Fatalf("expected unknown simplex capability to be rejected")
	}

	// 5. Contradictory evidence: both color and mono unsupported
	hwContradictoryColors := *hwA4Driverless
	hwContradictoryColors.MonochromeSupport = CapabilityUnsupported
	hwContradictoryColors.ColorSupport = CapabilityUnsupported
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, &hwContradictoryColors); err == nil {
		t.Fatalf("expected contradictory color/mono evidence to be rejected")
	}

	// 6. Contradictory evidence: all sides unsupported
	hwContradictorySides := *hwA4Driverless
	hwContradictorySides.SimplexSupport = CapabilityUnsupported
	hwContradictorySides.DuplexLongEdgeSupport = CapabilityUnsupported
	hwContradictorySides.DuplexShortEdgeSupport = CapabilityUnsupported
	if err := ValidateJobAgainstPolicyAndHardware(validCmd, policy, &hwContradictorySides); err == nil {
		t.Fatalf("expected contradictory sides evidence to be rejected")
	}
}

func TestPackage5B_ConservativeCapacityEnforcement(t *testing.T) {
	now := time.Now().UTC()
	res := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
		CUPS: contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			UpdatedAt:       now,
		},
		CUPSQueried: true,
		IPP: IPPPrinterStateResult{
			Source:            CapabilitySourceNativeIPP,
			EndpointReachable: true,
			State:             IPPPrinterStateIdle,
			AcceptingJobs:     AcceptingJobsAccepting,
			ObservedAt:        now,
		},
		SNMP: SNMPObservation{
			Available:  true,
			ObservedAt: now,
			Health: contracts.PrinterHealth{
				Online:                 true,
				State:                  "IDLE",
				PaperTrayStatus:        "OK",
				InputTrayCapacity:      100, // Physical limit is 100
				InputTrayCapacityKnown: true,
				UpdatedAt:              now,
			},
		},
		SNMPQueried:            true,
		RequireCUPS:            true,
		RequireIPP:             true,
		RequireSNMP:            true,
		RequireNativeIPP:       true,
		EvalTime:               now,
		ConfiguredTrayCapacity: 150, // Configured assumes 150
	})

	// Estimated stock is 140, but verified physical limit from SNMP is 100
	contract := res.ToContract(140, true, true, "")
	if contract.EstimatedSheets != 100 {
		t.Fatalf("expected EstimatedSheets to be conservatively capped to physical capacity 100, got %d", contract.EstimatedSheets)
	}
}
