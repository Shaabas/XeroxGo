package contracts

import "time"

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
	PaperTrayStatus        string                   `json:"paper_tray_status"`         // "OK", "LOW", "EMPTY", "UNKNOWN"
	EstimatedSheets        int                      `json:"estimated_sheets"`          // Tracked remaining sheets in tray
	InventoryCalibrated    bool                     `json:"inventory_calibrated"`      // Existing SQLite inventory calibration validity
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
