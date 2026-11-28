package contracts

import (
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultTelemetryMaxAge is the canonical 6-second maximum evidence freshness window.
	DefaultTelemetryMaxAge = 6 * time.Second
	// FutureHealthTimestampTolerance bounds allowed forward clock skew on health timestamps.
	FutureHealthTimestampTolerance = 5 * time.Second
)

// ValidateHealthEvidenceFreshness enforces that all required production sources (CUPS, NativeIPP,
// SNMP) carry valid, available, fresh, non-zero, non-future observation timestamps within maxAge,
// and ensures PrinterHealth.UpdatedAt does not outlive the oldest required observation.
func ValidateHealthEvidenceFreshness(
	h PrinterHealth,
	now time.Time,
	maxAge time.Duration,
) (ok bool, oldestObserved time.Time, reason string) {
	if now.IsZero() {
		return false, time.Time{}, "evaluation time is zero"
	}
	if maxAge <= 0 {
		maxAge = DefaultTelemetryMaxAge
	}
	if h.UpdatedAt.IsZero() {
		return false, time.Time{}, "updated_at must be non-zero"
	}
	if h.UpdatedAt.After(now.Add(FutureHealthTimestampTolerance)) {
		return false, time.Time{}, fmt.Sprintf("updated_at %s is in the future", h.UpdatedAt.Format(time.RFC3339))
	}
	if now.Sub(h.UpdatedAt) > maxAge {
		return false, time.Time{}, fmt.Sprintf("telemetry observation expired (age %s exceeds %s)", now.Sub(h.UpdatedAt).Round(time.Millisecond), maxAge)
	}
	if h.EvidenceFreshness == nil {
		return false, time.Time{}, "missing required evidence_freshness metadata"
	}

	required := []struct {
		name string
		src  SourceFreshness
	}{
		{"CUPS", h.EvidenceFreshness.CUPS},
		{"NativeIPP", h.EvidenceFreshness.NativeIPP},
		{"SNMP", h.EvidenceFreshness.SNMP},
	}

	for _, item := range required {
		if !item.src.Available {
			return false, time.Time{}, firstNonEmptyFreshness(item.src.Error, fmt.Sprintf("%s evidence unavailable", item.name))
		}
		if item.src.ObservedAt == nil || item.src.ObservedAt.IsZero() {
			return false, time.Time{}, fmt.Sprintf("%s observation timestamp is zero", item.name)
		}
		obs := *item.src.ObservedAt
		if obs.After(now.Add(FutureHealthTimestampTolerance)) {
			return false, time.Time{}, fmt.Sprintf("%s observation timestamp is in the future", item.name)
		}
		if now.Sub(obs) > maxAge {
			return false, obs, fmt.Sprintf("%s evidence expired (age %s exceeds %s)", item.name, now.Sub(obs).Round(time.Millisecond), maxAge)
		}
		if !item.src.Fresh {
			return false, obs, firstNonEmptyFreshness(item.src.Error, fmt.Sprintf("%s evidence marked stale", item.name))
		}
		if oldestObserved.IsZero() || obs.Before(oldestObserved) {
			oldestObserved = obs
		}
	}

	// Reconciliation or publication must never assign an UpdatedAt newer than the oldest required observation.
	if !oldestObserved.IsZero() && h.UpdatedAt.After(oldestObserved) {
		return false, oldestObserved, fmt.Sprintf("updated_at %s exceeds oldest required observation %s", h.UpdatedAt.Format(time.RFC3339Nano), oldestObserved.Format(time.RFC3339Nano))
	}
	return true, oldestObserved, ""
}

// ValidateReadyHealth validates both canonical evidence freshness and absence of blocking states/faults.
func ValidateReadyHealth(h PrinterHealth, now time.Time, maxAge time.Duration) (bool, string) {
	if ok, _, reason := ValidateHealthEvidenceFreshness(h, now, maxAge); !ok {
		return false, reason
	}
	if h.EvaluatedHealth != EvaluatedHealthHealthy {
		return false, firstNonEmptyFreshness(h.ErrorMessage, fmt.Sprintf("evaluated health is %s", h.EvaluatedHealth))
	}
	if !h.ExecutionSafe {
		return false, firstNonEmptyFreshness(h.ExecutionSafetyReason, "local execution safety barrier active")
	}
	if !h.Online || h.State == "ERROR" || h.State == "OFFLINE" || h.State == "UNKNOWN" || h.State == "" ||
		h.PaperJam || h.CoverOpen || h.OutputBinFull || h.PaperTrayStatus == "EMPTY" || len(h.BlockingReasons) > 0 {
		return false, firstNonEmptyFreshness(h.ErrorMessage, "printer reported blocking condition")
	}
	return true, ""
}

// IsHealthAvailable evaluates the canonical service availability decision across hardware readiness,
// execution safety, configured capacity bounds, and calibrated effective paper inventory.
func IsHealthAvailable(h PrinterHealth, now time.Time, maxAge time.Duration) (bool, string) {
	if ok, reason := ValidateReadyHealth(h, now, maxAge); !ok {
		return false, reason
	}
	if h.ConfiguredTrayCapacity <= 0 {
		return false, "kiosk reported invalid or non-positive tray capacity"
	}
	if h.EstimatedSheets < 0 || h.EstimatedSheets > h.ConfiguredTrayCapacity {
		return false, fmt.Sprintf("kiosk estimated sheets %d out of bounds [0, %d]", h.EstimatedSheets, h.ConfiguredTrayCapacity)
	}
	tray := strings.ToUpper(strings.TrimSpace(h.PaperTrayStatus))
	if tray == "UNKNOWN" || !h.InventoryCalibrated {
		return false, "printer paper supply is unknown; tray calibration required"
	}
	if tray == "EMPTY" || tray == "CHECK_PRINTER" || h.EstimatedSheets <= 0 {
		return false, "printer paper tray is empty or low; attendant refill required"
	}
	if EffectiveInventorySheets(h.EstimatedSheets, h.ConfiguredTrayCapacity, h.InputTrayCapacity, h.InputTrayCapacityKnown) <= 0 {
		return false, "effective verified tray inventory is zero"
	}
	adm := h.EffectiveNativeIPPAdmission()
	if adm == AcceptingJobsUnknown {
		return false, "native IPP job admission is unknown"
	}
	if adm == AcceptingJobsNotAccepting && strings.ToUpper(strings.TrimSpace(h.State)) != "PRINTING" {
		return false, "printer is not accepting jobs while idle"
	}
	return true, ""
}

// BuildUniformEvidenceFreshness constructs a consistent HealthEvidenceFreshness block when all three
// required sources share a verified observation timestamp (e.g. test/emulator simulations).
func BuildUniformEvidenceFreshness(ts time.Time, maxAge time.Duration) *HealthEvidenceFreshness {
	if maxAge <= 0 {
		maxAge = DefaultTelemetryMaxAge
	}
	obs := ts
	return &HealthEvidenceFreshness{
		CUPS:      SourceFreshness{Available: true, ObservedAt: &obs, Fresh: true},
		NativeIPP: SourceFreshness{Available: true, ObservedAt: &obs, Fresh: true},
		SNMP:      SourceFreshness{Available: true, ObservedAt: &obs, Fresh: true},
		MaxAgeSec: maxAge.Seconds(),
	}
}

func firstNonEmptyFreshness(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
