package printer

import (
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestReconciler_NativeIPPAdmissionSemantics(t *testing.T) {
	now := time.Now().UTC()
	cupsValid := true

	mkSNMP := func(deviceStatus, status int, jam bool) SNMPObservation {
		return EvaluateSNMPProbe(&SNMPProbeResult{
			TargetHost: "10.42.0.210",
			Printers: []SNMPPrinterDevice{
				{Index: "1", DeviceStatus: deviceStatus, Status: status, ErrorFlags: PrinterErrorFlags{Jammed: jam}},
			},
			Supplies: []SNMPSupply{
				{Index: "1.1", Description: "Black Cartridge", Class: 3, Type: 3, Unit: 19, MaxCapacity: 100, Level: 85},
			},
		}, 120, now)
	}

	baseSNMP := mkSNMP(2, 3, false) // running + idle

	t.Run("PRINTING_with_NOT_ACCEPTING_is_HEALTHY_and_reports_NOT_ACCEPTING", func(t *testing.T) {
		snmp := mkSNMP(2, 4, false) // running + printing
		res := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
			CUPS:             contracts.PrinterHealth{Online: true, State: "PRINTING", PaperTrayStatus: "OK", UpdatedAt: now},
			CUPSValid:        &cupsValid,
			CUPSQueried:      true,
			RequireCUPS:      true,
			IPP:              IPPPrinterStateResult{Source: CapabilitySourceNativeIPP, Queue: "HPTank", TargetURI: "ipp://10.42.0.210:631/ipp/print", State: IPPPrinterStateProcessing, AcceptingJobs: AcceptingJobsNotAccepting, EndpointReachable: true, QueueAccessible: true, ObservedAt: now},
			RequireIPP:       true,
			RequireNativeIPP: true,
			SNMP:             snmp,
			SNMPQueried:      true,
			RequireSNMP:      true,
			EvalTime:         now,
		})
		if res.EvaluatedHealth != contracts.EvaluatedHealthHealthy || !res.Online {
			t.Fatalf("expected HEALTHY+Online, got %s online=%v faults=%v contradictions=%v", res.EvaluatedHealth, res.Online, res.BlockingFaults, res.Contradictions)
		}
		if res.NativeIPPAdmission != contracts.AcceptingJobsNotAccepting {
			t.Fatalf("expected NOT_ACCEPTING, got %s", res.NativeIPPAdmission)
		}
		if len(res.Contradictions) != 0 {
			t.Fatalf("expected zero contradictions, got %v", res.Contradictions)
		}
		c := res.ToContract(120, true, true, "")
		if c.NativeIPPAdmission != contracts.AcceptingJobsNotAccepting {
			t.Fatalf("ToContract NativeIPPAdmission = %s, want NOT_ACCEPTING", c.NativeIPPAdmission)
		}
	})

	t.Run("IDLE_with_NOT_ACCEPTING_is_HEALTHY_and_reports_NOT_ACCEPTING", func(t *testing.T) {
		res := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
			CUPS:             contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: now},
			CUPSValid:        &cupsValid,
			CUPSQueried:      true,
			RequireCUPS:      true,
			IPP:              IPPPrinterStateResult{Source: CapabilitySourceNativeIPP, Queue: "HPTank", TargetURI: "ipp://10.42.0.210:631/ipp/print", State: IPPPrinterStateIdle, AcceptingJobs: AcceptingJobsNotAccepting, EndpointReachable: true, QueueAccessible: true, ObservedAt: now},
			RequireIPP:       true,
			RequireNativeIPP: true,
			SNMP:             baseSNMP,
			SNMPQueried:      true,
			RequireSNMP:      true,
			EvalTime:         now,
		})
		if res.EvaluatedHealth != contracts.EvaluatedHealthHealthy || !res.Online {
			t.Fatalf("expected HEALTHY+Online, got %s faults=%v", res.EvaluatedHealth, res.BlockingFaults)
		}
		if res.NativeIPPAdmission != contracts.AcceptingJobsNotAccepting {
			t.Fatalf("expected NOT_ACCEPTING, got %s", res.NativeIPPAdmission)
		}
	})

	t.Run("Expired_or_non_native_IPP_defaults_NativeIPPAdmission_to_UNKNOWN", func(t *testing.T) {
		expired := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
			CUPS:             contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: now},
			CUPSValid:        &cupsValid,
			CUPSQueried:      true,
			RequireCUPS:      true,
			IPP:              IPPPrinterStateResult{Source: CapabilitySourceNativeIPP, Queue: "HPTank", TargetURI: "ipp://10.42.0.210:631/ipp/print", State: IPPPrinterStateIdle, AcceptingJobs: AcceptingJobsAccepting, EndpointReachable: true, QueueAccessible: true, ObservedAt: now.Add(-15 * time.Second)},
			RequireIPP:       true,
			RequireNativeIPP: true,
			SNMP:             baseSNMP,
			SNMPQueried:      true,
			RequireSNMP:      true,
			EvalTime:         now,
		})
		if expired.NativeIPPAdmission != contracts.AcceptingJobsUnknown {
			t.Fatalf("expected UNKNOWN for expired native IPP, got %s", expired.NativeIPPAdmission)
		}

		nonNative := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
			CUPS:        contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: now},
			CUPSValid:   &cupsValid,
			CUPSQueried: true,
			RequireCUPS: true,
			IPP:         IPPPrinterStateResult{Source: CapabilitySourceCUPSQueue, Queue: "HPTank", State: IPPPrinterStateIdle, AcceptingJobs: AcceptingJobsAccepting, EndpointReachable: true, QueueAccessible: true, ObservedAt: now},
			RequireIPP:  true,
			SNMP:        baseSNMP,
			SNMPQueried: true,
			RequireSNMP: true,
			EvalTime:    now,
		})
		if nonNative.NativeIPPAdmission != contracts.AcceptingJobsUnknown {
			t.Fatalf("expected UNKNOWN for non-native IPP source, got %s", nonNative.NativeIPPAdmission)
		}
	})

	t.Run("IPPPrinterStateStopped_still_produces_FAULT_even_when_accepting", func(t *testing.T) {
		res := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
			CUPS:             contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: now},
			CUPSValid:        &cupsValid,
			CUPSQueried:      true,
			RequireCUPS:      true,
			IPP:              IPPPrinterStateResult{Source: CapabilitySourceNativeIPP, Queue: "HPTank", TargetURI: "ipp://10.42.0.210:631/ipp/print", State: IPPPrinterStateStopped, AcceptingJobs: AcceptingJobsAccepting, EndpointReachable: true, QueueAccessible: true, ObservedAt: now},
			RequireIPP:       true,
			RequireNativeIPP: true,
			SNMP:             baseSNMP,
			SNMPQueried:      true,
			RequireSNMP:      true,
			EvalTime:         now,
		})
		if res.EvaluatedHealth != contracts.EvaluatedHealthFault {
			t.Fatalf("expected FAULT for IPPPrinterStateStopped, got %s", res.EvaluatedHealth)
		}
	})

	t.Run("Physical_fault_during_PRINTING_and_NOT_ACCEPTING_still_produces_FAULT", func(t *testing.T) {
		snmpJam := mkSNMP(5, 1, true) // down + jammed
		res := EvaluateCanonicalPrinterHealth(CanonicalHealthInput{
			CUPS:             contracts.PrinterHealth{Online: true, State: "PRINTING", PaperTrayStatus: "OK", UpdatedAt: now},
			CUPSValid:        &cupsValid,
			CUPSQueried:      true,
			RequireCUPS:      true,
			IPP:              IPPPrinterStateResult{Source: CapabilitySourceNativeIPP, Queue: "HPTank", TargetURI: "ipp://10.42.0.210:631/ipp/print", State: IPPPrinterStateProcessing, AcceptingJobs: AcceptingJobsNotAccepting, ParsedReasons: []IPPPrinterReason{{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError}}, PaperJam: true, EndpointReachable: true, QueueAccessible: true, ObservedAt: now},
			RequireIPP:       true,
			RequireNativeIPP: true,
			SNMP:             snmpJam,
			SNMPQueried:      true,
			RequireSNMP:      true,
			EvalTime:         now,
		})
		if res.EvaluatedHealth != contracts.EvaluatedHealthFault || !res.PaperJam {
			t.Fatalf("expected FAULT with PaperJam, got %s jam=%v", res.EvaluatedHealth, res.PaperJam)
		}
	})
}
