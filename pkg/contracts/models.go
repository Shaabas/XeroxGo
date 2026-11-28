package contracts

import (
	"fmt"
	"strings"
	"time"
)

type JobStatus string

const (
	JobStatusUploaded         JobStatus = "UPLOADED"
	JobStatusNormalized       JobStatus = "NORMALIZED"
	JobStatusPendingPayment   JobStatus = "PENDING_PAYMENT"
	JobStatusPaidWaiting      JobStatus = "PAID_WAITING"
	JobStatusDispatched       JobStatus = "DISPATCHED"
	JobStatusSpooling         JobStatus = "SPOOLING"
	JobStatusPrinting         JobStatus = "PRINTING"
	JobStatusCompleted        JobStatus = "COMPLETED"
	JobStatusPausedError      JobStatus = "PAUSED_ERROR"
	JobStatusFailedRefundable JobStatus = "FAILED_REFUNDABLE"
)

type DuplexMode string

const (
	DuplexSimplex   DuplexMode = "SIMPLEX"
	DuplexLongEdge  DuplexMode = "DUPLEX_LONG_EDGE"
	DuplexShortEdge DuplexMode = "DUPLEX_SHORT_EDGE"
)

// EvaluatedHealthStatus represents canonical evaluated readiness for print dispatch.
type EvaluatedHealthStatus string

const (
	EvaluatedHealthHealthy    EvaluatedHealthStatus = "HEALTHY"
	EvaluatedHealthFault      EvaluatedHealthStatus = "FAULT"
	EvaluatedHealthUnverified EvaluatedHealthStatus = "UNVERIFIED"
)

// AcceptingJobsStatus represents native IPP printer-is-accepting-jobs admission state.
type AcceptingJobsStatus string

const (
	AcceptingJobsUnknown      AcceptingJobsStatus = "UNKNOWN"
	AcceptingJobsAccepting    AcceptingJobsStatus = "ACCEPTING"
	AcceptingJobsNotAccepting AcceptingJobsStatus = "NOT_ACCEPTING"
)

// HealthReason represents a structured blocking fault or non-blocking warning.
type HealthReason struct {
	Code    string `json:"code"`
	Source  string `json:"source,omitempty"`
	Message string `json:"message,omitempty"`
}

// SourceFreshness represents the availability and observation age of one telemetry source.
type SourceFreshness struct {
	Available  bool       `json:"available"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	AgeSeconds float64    `json:"age_seconds,omitempty"`
	Fresh      bool       `json:"fresh"`
	Error      string     `json:"error,omitempty"`
}

// HealthEvidenceFreshness captures source-level freshness for canonical evaluation.
type HealthEvidenceFreshness struct {
	CUPS      SourceFreshness `json:"cups"`
	NativeIPP SourceFreshness `json:"native_ipp"`
	SNMP      SourceFreshness `json:"snmp"`
	MaxAgeSec float64         `json:"max_age_sec"`
}

type PrinterHealth struct {
	Online                 bool                     `json:"online"`
	State                  string                   `json:"state"`                     // Operational state: "IDLE", "PRINTING", "WARMING_UP", "ERROR", "OFFLINE", "UNKNOWN"
	EvaluatedHealth        EvaluatedHealthStatus    `json:"evaluated_health"`          // Canonical readiness: "HEALTHY", "FAULT", "UNVERIFIED"
	NativeIPPAdmission   AcceptingJobsStatus      `json:"native_ipp_admission,omitempty"` // Native IPP printer-is-accepting-jobs state ("ACCEPTING", "NOT_ACCEPTING", "UNKNOWN")
	PaperTrayStatus        string                   `json:"paper_tray_status"`         // "OK", "LOW", "EMPTY", "UNKNOWN"
	EstimatedSheets        int                      `json:"estimated_sheets"`          // Tracked remaining sheets in tray
	ConfiguredTrayCapacity int                      `json:"configured_tray_capacity,omitempty"` // Configured physical tray capacity on the kiosk
	InventoryCalibrated    bool                     `json:"inventory_calibrated"`      // Existing SQLite inventory calibration validity
	InventorySeq           uint64                   `json:"inventory_seq,omitempty"`   // Monotonic calibration sequence from kiosk
	ExecutionSafe          bool                     `json:"execution_safe"`            // Local durable execution-safety readiness
	ExecutionSafetyReason  string                   `json:"execution_safety_reason,omitempty"`
	TonerLevelPercent      int                      `json:"toner_level_percent"` // 0 - 100 when TonerLevelKnown is true; -1 when unknown
	TonerLevelKnown        bool                     `json:"toner_level_known"`
	DrumLifePercent        int                      `json:"drum_life_percent"` // 0 - 100 when DrumLifeKnown is true; -1 when unknown
	DrumLifeKnown          bool                     `json:"drum_life_known"`
	InputTrayLevel         int                      `json:"input_tray_level"` // Measured tray level when InputTrayLevelKnown is true; -1 when unknown
	InputTrayLevelKnown    bool                     `json:"input_tray_level_known"`
	InputTrayCapacity      int                      `json:"input_tray_capacity"` // Tray capacity when InputTrayCapacityKnown is true; -1 when unknown
	InputTrayCapacityKnown bool                     `json:"input_tray_capacity_known"`
	CoverOpen              bool                     `json:"cover_open"`
	PaperJam               bool                     `json:"paper_jam"`
	OutputBinFull          bool                     `json:"output_bin_full"`
	BlockingReasons        []HealthReason           `json:"blocking_reasons,omitempty"`
	Warnings               []HealthReason           `json:"warnings,omitempty"`
	EvidenceFreshness      *HealthEvidenceFreshness `json:"evidence_freshness,omitempty"`
	ErrorMessage           string                   `json:"error_message,omitempty"`
	UpdatedAt              time.Time                `json:"updated_at"`
}

// EffectiveNativeIPPAdmission returns the normalized native IPP admission state,
// defaulting missing or unrecognized values to AcceptingJobsUnknown.
func (h PrinterHealth) EffectiveNativeIPPAdmission() AcceptingJobsStatus {
	switch h.NativeIPPAdmission {
	case AcceptingJobsAccepting, AcceptingJobsNotAccepting:
		return h.NativeIPPAdmission
	default:
		return AcceptingJobsUnknown
	}
}

type PrintJobCommand struct {
	JobID            string     `json:"job_id"`
	OrderID          string     `json:"order_id,omitempty"`           // Parent order ID for multi-document correlation
	DocumentIndex    int        `json:"document_index"`               // 0-indexed position within parent order
	TotalDocuments   int        `json:"total_documents"`              // Total document jobs belonging to parent order
	OriginalFilename string     `json:"original_filename,omitempty"`  // Document display name
	ColorMode        string     `json:"color_mode,omitempty"`         // "BW" or "COLOR"
	KioskID          string     `json:"kiosk_id"`
	DisplayCode      string     `json:"display_code"`                 // 4-char code shown on HDMI screen (e.g. "A74X")
	DownloadURL      string     `json:"download_url"`                 // Signed URL to download normalized PDF
	SHA256Checksum   string     `json:"sha256_checksum"`              // Verified by Pi before printing
	TotalPages       int        `json:"total_pages"`                  // Logical PDF pages per copy after normalization
	TotalSheets      int        `json:"total_sheets"`                 // Physical A4 sheets required (sheetsPerCopy * copies)
	Copies           int        `json:"copies"`
	Duplex           DuplexMode `json:"duplex"`
	StartFromPage    int        `json:"start_from_page"`              // 1 normally; >1 when resuming after crash/paper out
}

type JobProgressEvent struct {
	JobID         string    `json:"job_id"`
	OrderID       string    `json:"order_id,omitempty"`
	KioskID       string    `json:"kiosk_id"`
	Status        JobStatus `json:"status"`
	PagesPrinted  int       `json:"pages_printed"`
	SheetsPrinted int       `json:"sheets_printed"`
	ErrorReason   string    `json:"error_reason,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// ProductCapabilityPolicy defines the configured product policy for supported print options.
type ProductCapabilityPolicy struct {
	AllowColor     bool   `json:"allow_color"`
	AllowDuplex    bool   `json:"allow_duplex"`
	RequireA4      bool   `json:"require_a4"`
	PaperSize      string `json:"paper_size"`
	AllowMultiCopy bool   `json:"allow_multi_copy"`
}

// DefaultDemoProductPolicy returns the canonical product capability policy for the HP LaserJet Tank 1020w demo kiosk:
// A4 paper only, BW (monochrome) only, Simplex (single-sided) only, multi-copy allowed.
func DefaultDemoProductPolicy() ProductCapabilityPolicy {
	return ProductCapabilityPolicy{
		AllowColor:     false,
		AllowDuplex:    false,
		RequireA4:      true,
		PaperSize:      "A4",
		AllowMultiCopy: true,
	}
}

// EffectiveTrayCapacity returns the conservative upper bound on tray capacity using
// the configured capacity and any valid smaller known physical tray capacity.
// Unknown physical capacity does not restrict configured capacity.
func EffectiveTrayCapacity(configuredCapacity, physicalCapacity int, physicalCapacityKnown bool) int {
	if configuredCapacity <= 0 {
		return 0
	}
	limit := configuredCapacity
	if physicalCapacityKnown && physicalCapacity > 0 && physicalCapacity < limit {
		limit = physicalCapacity
	}
	return limit
}

// EffectiveInventorySheets computes the conservative effective sheet inventory from
// calibrated stock, configured capacity, and any valid smaller known physical capacity.
func EffectiveInventorySheets(calibratedSheets, configuredCapacity, physicalCapacity int, physicalCapacityKnown bool) int {
	if calibratedSheets <= 0 {
		return 0
	}
	limit := EffectiveTrayCapacity(configuredCapacity, physicalCapacity, physicalCapacityKnown)
	if limit <= 0 {
		return 0
	}
	if calibratedSheets > limit {
		return limit
	}
	return calibratedSheets
}

// ValidatePrintModeAgainstPolicy validates duplex and color mode against the configured product policy.
// Fails closed on invalid or unsupported print modes.
func ValidatePrintModeAgainstPolicy(duplex DuplexMode, colorMode string, policy ProductCapabilityPolicy) error {
	switch duplex {
	case DuplexSimplex:
	case DuplexLongEdge, DuplexShortEdge:
		if !policy.AllowDuplex {
			return fmt.Errorf("duplex printing (%s) is not supported by this kiosk", duplex)
		}
	default:
		return fmt.Errorf("invalid duplex mode %q", duplex)
	}

	cm := strings.ToUpper(strings.TrimSpace(colorMode))
	switch cm {
	case "", "BW":
	case "COLOR":
		if !policy.AllowColor {
			return fmt.Errorf("color printing is not supported by this kiosk")
		}
	default:
		return fmt.Errorf("invalid color mode %q", colorMode)
	}

	return nil
}
