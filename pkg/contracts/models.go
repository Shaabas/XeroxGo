package contracts

import "time"

// JobStatus represents the strict state machine of a print order.
type JobStatus string

const (
	JobStatusUploaded          JobStatus = "UPLOADED"            // Raw file received, awaiting PDF sanitization
	JobStatusNormalized        JobStatus = "NORMALIZED"          // Validated, converted to Mono A4, priced
	JobStatusPendingPayment    JobStatus = "PENDING_PAYMENT"     // Waiting for payment gateway webhook
	JobStatusPaidWaiting       JobStatus = "PAID_WAITING"        // Paid! Waiting for student release / kiosk ready
	JobStatusDispatched        JobStatus = "DISPATCHED"          // Sent to Raspberry Pi over WebSocket
	JobStatusSpooling          JobStatus = "SPOOLING"            // Pi is downloading the sanitized PDF
	JobStatusPrinting          JobStatus = "PRINTING"            // Streaming to Brother HL-L5210DN via PJL/IPP
	JobStatusCompleted         JobStatus = "COMPLETED"           // All sheets confirmed exited via PJL USTATUS
	JobStatusPausedError       JobStatus = "PAUSED_ERROR"        // Out of paper / Jam mid-job (tracks pages_printed)
	JobStatusFailedRefundable  JobStatus = "FAILED_REFUNDABLE"   // Unrecoverable hardware error; triggers refund
)

// DuplexMode specifies single-sided or two-sided printing.
type DuplexMode string

const (
	DuplexSimplex   DuplexMode = "SIMPLEX"           // 1-sided
	DuplexLongEdge  DuplexMode = "DUPLEX_LONG_EDGE"  // Standard portrait 2-sided
	DuplexShortEdge DuplexMode = "DUPLEX_SHORT_EDGE" // Landscape / calendar 2-sided
)

// PrinterHealth represents real-time telemetry from the Brother HL-L5210DN.
type PrinterHealth struct {
	Online            bool      `json:"online"`
	State             string    `json:"state"`               // "IDLE", "PRINTING", "WARMING_UP", "ERROR"
	PaperTrayStatus   string    `json:"paper_tray_status"`   // "OK", "LOW", "EMPTY"
	EstimatedSheets   int       `json:"estimated_sheets"`    // Tracked remaining sheets in 250-sheet tray
	TonerLevelPercent int       `json:"toner_level_percent"` // 0 - 100 via SNMP
	DrumLifePercent   int       `json:"drum_life_percent"`   // 0 - 100 via SNMP
	CoverOpen         bool      `json:"cover_open"`
	PaperJam          bool      `json:"paper_jam"`
	OutputBinFull     bool      `json:"output_bin_full"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// PrintJobCommand is sent from Cloud -> Raspberry Pi to execute an authorized job.
type PrintJobCommand struct {
	JobID          string     `json:"job_id"`
	KioskID        string     `json:"kiosk_id"`
	DisplayCode    string     `json:"display_code"`    // Short 4-char ID shown on HDMI monitor (e.g., "A74X")
	DownloadURL    string     `json:"download_url"`    // Short-lived signed URL for sanitized PDF
	SHA256Checksum string     `json:"sha256_checksum"` // Verified by Pi before sending to printer
	TotalPages     int        `json:"total_pages"`     // Logical PDF pages after normalization
	TotalSheets    int        `json:"total_sheets"`    // Physical A4 sheets required
	Copies         int        `json:"copies"`
	Duplex         DuplexMode `json:"duplex"`
	StartFromPage  int        `json:"start_from_page"` // Usually 1; >1 when resuming after power/paper recovery
}

// JobProgressEvent is streamed from Raspberry Pi -> Cloud as each page prints.
type JobProgressEvent struct {
	JobID          string    `json:"job_id"`
	KioskID        string    `json:"kiosk_id"`
	Status         JobStatus `json:"status"`
	PagesPrinted   int       `json:"pages_printed"`
	SheetsPrinted  int       `json:"sheets_printed"`
	ErrorReason    string    `json:"error_reason,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
}
