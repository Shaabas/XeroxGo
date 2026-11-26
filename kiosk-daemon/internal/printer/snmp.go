package printer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"

	"xeroxgo/pkg/contracts"
)

const (
	defaultTarget    = "192.168.99.2"
	defaultPort      = 161
	defaultCommunity = "public"
	defaultTimeout   = 2 * time.Second
	defaultRetries   = 1

	// RFC 1213 / RFC 3418 MIB-II
	oidSysDescr = "1.3.6.1.2.1.1.1.0"

	// RFC 2790 Host Resources MIB
	oidHrPrinterStatus             = "1.3.6.1.2.1.25.3.5.1.1.1"
	oidHrPrinterDetectedErrorState = "1.3.6.1.2.1.25.3.5.1.2.1"

	// RFC 3805 / RFC 1759 Printer MIB
	oidPrtMarkerLifeCount      = "1.3.6.1.2.1.43.10.2.1.4.1.1"
	oidPrtMarkerSuppliesLevel1 = "1.3.6.1.2.1.43.11.1.1.9.1.1" // Supply 1 (toner) level
	oidPrtMarkerSuppliesMax1   = "1.3.6.1.2.1.43.11.1.1.8.1.1" // Supply 1 (toner) max capacity
	oidPrtMarkerSuppliesLevel2 = "1.3.6.1.2.1.43.11.1.1.9.1.2" // Supply 2 (drum/waste) level
	oidPrtMarkerSuppliesMax2   = "1.3.6.1.2.1.43.11.1.1.8.1.2" // Supply 2 (drum/waste) max capacity
)

// queriedOIDs contains the standard Host-Resources-MIB and Printer-MIB OIDs queried for health.
var queriedOIDs = []string{
	oidHrPrinterStatus,
	oidHrPrinterDetectedErrorState,
	oidPrtMarkerLifeCount,
	oidPrtMarkerSuppliesLevel1,
	oidPrtMarkerSuppliesMax1,
	oidPrtMarkerSuppliesLevel2,
	oidPrtMarkerSuppliesMax2,
}

// SNMPConfig encapsulates SNMP connection parameters.
type SNMPConfig struct {
	Target    string
	Port      int
	Community string
	Timeout   time.Duration
	Retries   int
}

// DefaultSNMPConfig returns standard configuration for printer SNMP monitoring.
func DefaultSNMPConfig() SNMPConfig {
	return SNMPConfig{
		Target:    defaultTarget,
		Port:      defaultPort,
		Community: defaultCommunity,
		Timeout:   defaultTimeout,
		Retries:   defaultRetries,
	}
}

// PrinterErrorFlags decodes RFC 2790 hrPrinterDetectedErrorState BITS.
type PrinterErrorFlags struct {
	LowPaper            bool // bit 0 (0x80)
	NoPaper             bool // bit 1 (0x40)
	LowToner            bool // bit 2 (0x20)
	NoToner             bool // bit 3 (0x10)
	DoorOpen            bool // bit 4 (0x08)
	Jammed              bool // bit 5 (0x04)
	Offline             bool // bit 6 (0x02)
	ServiceRequested    bool // bit 7 (0x01)
	InputTrayMissing    bool // bit 8 (0x80 in byte 1)
	OutputTrayMissing   bool // bit 9 (0x40 in byte 1)
	MarkerSupplyMissing bool // bit 10 (0x20 in byte 1)
	OutputNearFull      bool // bit 11 (0x10 in byte 1)
	OutputFull          bool // bit 12 (0x08 in byte 1)
	InputTrayEmpty      bool // bit 13 (0x04 in byte 1)
	OverduePreventMaint bool // bit 14 (0x02 in byte 1)
}

// DecodeErrorBitmask parses RFC 2790 hrPrinterDetectedErrorState octets.
func DecodeErrorBitmask(raw []byte) PrinterErrorFlags {
	var flags PrinterErrorFlags
	if len(raw) == 0 {
		return flags
	}

	b0 := raw[0]
	flags.LowPaper = b0&0x80 != 0
	flags.NoPaper = b0&0x40 != 0
	flags.LowToner = b0&0x20 != 0
	flags.NoToner = b0&0x10 != 0
	flags.DoorOpen = b0&0x08 != 0
	flags.Jammed = b0&0x04 != 0
	flags.Offline = b0&0x02 != 0
	flags.ServiceRequested = b0&0x01 != 0

	if len(raw) > 1 {
		b1 := raw[1]
		flags.InputTrayMissing = b1&0x80 != 0
		flags.OutputTrayMissing = b1&0x40 != 0
		flags.MarkerSupplyMissing = b1&0x20 != 0
		flags.OutputNearFull = b1&0x10 != 0
		flags.OutputFull = b1&0x08 != 0
		flags.InputTrayEmpty = b1&0x04 != 0
		flags.OverduePreventMaint = b1&0x02 != 0
	}
	return flags
}

type snmpSession interface {
	Get(oids []string) (*gosnmp.SnmpPacket, error)
	Close() error
}

type realSnmpSession struct {
	client *gosnmp.GoSNMP
}

func (s *realSnmpSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	return s.client.Get(oids)
}

func (s *realSnmpSession) Close() error {
	if s.client != nil && s.client.Conn != nil {
		return s.client.Conn.Close()
	}
	return nil
}

type sessionFactory func(ctx context.Context) (snmpSession, error)

func realSessionFactoryWithConfig(cfg SNMPConfig) sessionFactory {
	return func(ctx context.Context) (snmpSession, error) {
		target := cfg.Target
		if target == "" {
			target = defaultTarget
		}
		port := uint16(cfg.Port)
		if port == 0 {
			port = defaultPort
		}
		community := cfg.Community
		if community == "" {
			community = defaultCommunity
		}
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		retries := cfg.Retries
		if retries < 0 {
			retries = 0
		}

		client := &gosnmp.GoSNMP{
			Target:    target,
			Port:      port,
			Community: community,
			Version:   gosnmp.Version2c,
			Timeout:   timeout,
			Retries:   retries,
			Context:   ctx,
		}

		if err := client.Connect(); err != nil {
			return nil, fmt.Errorf("failed to connect to printer at %s:%d: %w", target, port, err)
		}

		return &realSnmpSession{client: client}, nil
	}
}

func realSessionFactoryWithTarget(target string) sessionFactory {
	cfg := DefaultSNMPConfig()
	cfg.Target = target
	return realSessionFactoryWithConfig(cfg)
}

func realSessionFactory(ctx context.Context) (snmpSession, error) {
	return realSessionFactoryWithTarget(defaultTarget)(ctx)
}

func offlineHealth(estimatedSheets int, errMsg string) contracts.PrinterHealth {
	return contracts.PrinterHealth{
		Online:          false,
		State:           "OFFLINE",
		PaperTrayStatus: "UNKNOWN",
		EstimatedSheets: estimatedSheets,
		ErrorMessage:    errMsg,
		UpdatedAt:       time.Now().UTC(),
	}
}

func normalizeOID(oid string) string {
	return strings.TrimPrefix(oid, ".")
}

func isValidPDU(pdu gosnmp.SnmpPDU) bool {
	if pdu.Type == gosnmp.NoSuchObject || pdu.Type == gosnmp.NoSuchInstance || pdu.Type == gosnmp.Null {
		return false
	}
	return true
}

func findPDU(packet *gosnmp.SnmpPacket, targetOID string, fallbackIndex int) (gosnmp.SnmpPDU, bool) {
	if packet == nil {
		return gosnmp.SnmpPDU{}, false
	}
	target := normalizeOID(targetOID)
	for _, pdu := range packet.Variables {
		if normalizeOID(pdu.Name) == target {
			return pdu, true
		}
	}
	if fallbackIndex >= 0 && fallbackIndex < len(packet.Variables) {
		pdu := packet.Variables[fallbackIndex]
		if pdu.Name == "" || normalizeOID(pdu.Name) == target {
			return pdu, true
		}
	}
	return gosnmp.SnmpPDU{}, false
}

func pduToInt64(pdu gosnmp.SnmpPDU) (int64, bool) {
	switch pdu.Type {
	case gosnmp.Integer, gosnmp.Counter32, gosnmp.Counter64, gosnmp.Gauge32, gosnmp.TimeTicks:
	default:
		return 0, false
	}
	switch v := pdu.Value.(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		return int64(v), true
	default:
		return 0, false
	}
}

// calcSupplyPercentage calculates remaining percentage following RFC 3805 semantics:
// Level: >= 0 (valid units), -1 (other), -2 (unknown), -3 (some remaining).
// MaxCapacity: > 0 (valid units), -1 (other), -2 (unknown).
// Only returns valid when maxCap > 0 and level >= 0.
func calcSupplyPercentage(pduLevel, pduMax gosnmp.SnmpPDU) (int, bool) {
	if !isValidPDU(pduLevel) || !isValidPDU(pduMax) {
		return 0, false
	}
	level, okL := pduToInt64(pduLevel)
	maxCap, okM := pduToInt64(pduMax)
	if !okL || !okM {
		return 0, false
	}
	if maxCap <= 0 || level < 0 {
		return 0, false
	}
	pct := int((level * 100) / maxCap)
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return pct, true
}

func pollHealthWithFactory(
	ctx context.Context,
	estimatedSheets int,
	factory sessionFactory,
) contracts.PrinterHealth {
	if ctx == nil {
		return offlineHealth(estimatedSheets, "context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return offlineHealth(estimatedSheets, fmt.Sprintf("context canceled: %v", err))
	}

	session, err := factory(ctx)
	if err != nil {
		return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP connection failed: %v", err))
	}
	defer session.Close()

	packet, err := session.Get(queriedOIDs)
	if err != nil {
		return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP GET request failed: %v", err))
	}
	if packet == nil {
		return offlineHealth(estimatedSheets, "SNMP response was nil")
	}

	// In SNMPv1 or strict agents, querying unsupported OIDs can return NoSuchName at packet level.
	// Fall back to essential Host-Resources-MIB OIDs if that occurs.
	if packet.Error == gosnmp.NoSuchName {
		essentialOIDs := []string{oidHrPrinterStatus, oidHrPrinterDetectedErrorState}
		fallbackPacket, fallbackErr := session.Get(essentialOIDs)
		if fallbackErr == nil && fallbackPacket != nil && fallbackPacket.Error == gosnmp.NoError {
			packet = fallbackPacket
		} else {
			return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP error status in response: %v (index %d)", packet.Error, packet.ErrorIndex))
		}
	} else if packet.Error != gosnmp.NoError {
		return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP error status in response: %v (index %d)", packet.Error, packet.ErrorIndex))
	}

	// 1. hrPrinterStatus (1.3.6.1.2.1.25.3.5.1.1.1)
	// RFC 2790: 1=other, 2=unknown, 3=idle, 4=printing, 5=warmup
	statusState := "UNKNOWN"
	statusFound := false
	var statusVal int

	pduStatus, okStatus := findPDU(packet, oidHrPrinterStatus, 0)
	if okStatus && isValidPDU(pduStatus) {
		if v, okVal := pduToInt64(pduStatus); okVal {
			statusVal = int(v)
			statusFound = true
			switch statusVal {
			case 1, 2:
				statusState = "UNKNOWN"
			case 3:
				statusState = "IDLE"
			case 4:
				statusState = "PRINTING"
			case 5:
				statusState = "WARMING_UP"
			case 6:
				statusState = "ERROR"
			case 7:
				statusState = "OFFLINE"
			default:
				statusState = "UNKNOWN"
			}
		}
	}

	// 2. hrPrinterDetectedErrorState (1.3.6.1.2.1.25.3.5.1.2.1)
	var flags PrinterErrorFlags
	errorFlagsFound := false
	pduErrorState, okErr := findPDU(packet, oidHrPrinterDetectedErrorState, 1)
	if okErr && isValidPDU(pduErrorState) {
		var rawBytes []byte
		switch v := pduErrorState.Value.(type) {
		case []byte:
			rawBytes = v
		case string:
			rawBytes = []byte(v)
		}
		if len(rawBytes) > 0 {
			flags = DecodeErrorBitmask(rawBytes)
			errorFlagsFound = true
		}
	}

	// 3. prtMarkerLifeCount (1.3.6.1.2.1.43.10.2.1.4.1.1)
	markerFound := false
	pduMarkerLife, okMarker := findPDU(packet, oidPrtMarkerLifeCount, 2)
	if okMarker && isValidPDU(pduMarkerLife) {
		if _, okVal := pduToInt64(pduMarkerLife); okVal {
			markerFound = true
		}
	}

	// 4. prtMarkerSuppliesLevel & MaxCapacity (Supply 1 - Toner)
	tonerPercent := 0
	tonerKnown := false
	pduTonerLevel, okTL := findPDU(packet, oidPrtMarkerSuppliesLevel1, 3)
	pduTonerMax, okTM := findPDU(packet, oidPrtMarkerSuppliesMax1, 4)
	if okTL && okTM {
		if pct, valid := calcSupplyPercentage(pduTonerLevel, pduTonerMax); valid {
			tonerPercent = pct
			tonerKnown = true
		}
	}

	// 5. prtMarkerSuppliesLevel & MaxCapacity (Supply 2 - Drum Unit)
	drumPercent := 0
	drumKnown := false
	pduDrumLevel, okDL := findPDU(packet, oidPrtMarkerSuppliesLevel2, 5)
	pduDrumMax, okDM := findPDU(packet, oidPrtMarkerSuppliesMax2, 6)
	if okDL && okDM {
		if pct, valid := calcSupplyPercentage(pduDrumLevel, pduDrumMax); valid {
			drumPercent = pct
			drumKnown = true
		}
	}

	// Distinguish actual communication failure from missing individual metrics.
	// If the SNMP agent responded but returned zero recognizable metrics, report UNKNOWN.
	if !statusFound && !errorFlagsFound && !markerFound && !tonerKnown && !drumKnown {
		return contracts.PrinterHealth{
			Online:             true,
			State:              "UNKNOWN",
			PaperTrayStatus:    "UNKNOWN",
			EstimatedSheets:    estimatedSheets,
			TonerLevelPercent:  0,
			TonerLevelKnown:    false,
			DrumLifePercent:    0,
			DrumLifeKnown:      false,
			InputTrayLevel:     -1,
			InputTrayCapacity:  -1,
			CoverOpen:          false,
			PaperJam:           false,
			OutputBinFull:      false,
			ErrorMessage:       "SNMP responded but returned no supported printer metrics",
			UpdatedAt:          time.Now().UTC(),
		}
	}

	// PaperTrayStatus resolution
	paperTrayStatus := "OK"
	if flags.NoPaper || flags.InputTrayEmpty || flags.InputTrayMissing {
		paperTrayStatus = "EMPTY"
	} else if flags.LowPaper {
		paperTrayStatus = "LOW"
	}

	// Online and State mapping
	isOffline := (statusState == "OFFLINE") || flags.Offline
	var online bool
	var finalState string

	if isOffline {
		online = false
		finalState = "OFFLINE"
	} else {
		online = true
		if flags.DoorOpen || flags.Jammed || flags.OutputFull || flags.NoPaper || flags.NoToner || flags.InputTrayEmpty || flags.ServiceRequested {
			finalState = "ERROR"
		} else {
			finalState = statusState
		}
	}

	// Formulation of descriptive ErrorMessage
	var issues []string
	if isOffline {
		issues = append(issues, "printer offline")
	}
	if flags.DoorOpen {
		issues = append(issues, "door open")
	}
	if flags.Jammed {
		issues = append(issues, "paper jam")
	}
	if flags.OutputFull {
		issues = append(issues, "output bin full")
	} else if flags.OutputNearFull {
		issues = append(issues, "output bin near full")
	}
	if flags.NoPaper {
		issues = append(issues, "no paper")
	} else if flags.InputTrayEmpty {
		issues = append(issues, "input tray empty")
	} else if flags.InputTrayMissing {
		issues = append(issues, "input tray missing")
	} else if flags.LowPaper {
		issues = append(issues, "low paper")
	}
	if flags.NoToner {
		issues = append(issues, "no toner")
	} else if flags.LowToner {
		issues = append(issues, "low toner")
	}
	if flags.MarkerSupplyMissing {
		issues = append(issues, "marker supply missing")
	}
	if flags.ServiceRequested {
		issues = append(issues, "service requested")
	}
	if flags.OverduePreventMaint {
		issues = append(issues, "maintenance overdue")
	}

	if finalState == "ERROR" && len(issues) == 0 {
		if statusVal == 6 {
			issues = append(issues, "printer error")
		} else {
			issues = append(issues, "unrecognized printer error")
		}
	} else if finalState == "UNKNOWN" && len(issues) == 0 && statusFound {
		issues = append(issues, fmt.Sprintf("unrecognized printer status: %d", statusVal))
	}

	errorMsg := strings.Join(issues, ", ")

	return contracts.PrinterHealth{
		Online:            online,
		State:             finalState,
		PaperTrayStatus:   paperTrayStatus,
		EstimatedSheets:   estimatedSheets,
		TonerLevelPercent: tonerPercent,
		TonerLevelKnown:   tonerKnown,
		DrumLifePercent:   drumPercent,
		DrumLifeKnown:     drumKnown,
		InputTrayLevel:    -1,
		InputTrayCapacity: -1,
		CoverOpen:         flags.DoorOpen,
		PaperJam:          flags.Jammed,
		OutputBinFull:     flags.OutputFull,
		ErrorMessage:      errorMsg,
		UpdatedAt:         time.Now().UTC(),
	}
}

// PollHealthConfig polls printer hardware health over SNMP with the provided configuration.
func PollHealthConfig(
	ctx context.Context,
	cfg SNMPConfig,
	estimatedSheets int,
) contracts.PrinterHealth {
	return pollHealthWithFactory(ctx, estimatedSheets, realSessionFactoryWithConfig(cfg))
}

// PollHealthTarget polls printer hardware health over SNMP targeting a specific host/IP.
func PollHealthTarget(
	ctx context.Context,
	target string,
	estimatedSheets int,
) contracts.PrinterHealth {
	cfg := DefaultSNMPConfig()
	cfg.Target = target
	return PollHealthConfig(ctx, cfg, estimatedSheets)
}

// PollHealth polls printer hardware health over SNMP using the default address.
func PollHealth(
	ctx context.Context,
	estimatedSheets int,
) contracts.PrinterHealth {
	return PollHealthTarget(ctx, defaultTarget, estimatedSheets)
}

// DynamicSessionFactory constructs a DynamicSNMPSession for target inspection and polling.
type DynamicSessionFactory func(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error)

// SNMPObservation captures normalized RFC 2790/3805 physical printer telemetry from one SNMP cycle.
type SNMPObservation struct {
	Available              bool                     `json:"available"`
	ObservedAt             time.Time                `json:"observed_at"`
	TargetHost             string                   `json:"target_host"`
	DiscoveryMode          string                   `json:"discovery_mode,omitempty"`
	DeviceStatus           int64                    `json:"device_status"`
	PrinterStatus          int64                    `json:"printer_status"`
	HasErrorFlags          bool                     `json:"has_error_flags"`
	ErrorFlags             PrinterErrorFlags        `json:"error_flags"`
	Alerts                 []SNMPAlert              `json:"alerts,omitempty"`
	TonerPercent           int                      `json:"toner_percent"`
	TonerKnown             bool                     `json:"toner_known"`
	DrumPercent            int                      `json:"drum_percent"`
	DrumKnown              bool                     `json:"drum_known"`
	InputTrayLevel         int                      `json:"input_tray_level"`
	InputTrayLevelKnown    bool                     `json:"input_tray_level_known"`
	InputTrayCapacity      int                      `json:"input_tray_capacity"`
	InputTrayCapacityKnown bool                     `json:"input_tray_capacity_known"`
	HasBlockingFault       bool                     `json:"has_blocking_fault"`
	BlockingFaults         []contracts.HealthReason `json:"blocking_faults,omitempty"`
	Warnings               []contracts.HealthReason `json:"warnings,omitempty"`
	Error                  string                   `json:"error,omitempty"`
	Health                 contracts.PrinterHealth  `json:"health"`
}

// SNMPHealthMonitor caches dynamically discovered SNMP indexes and executes bounded targeted
// health queries on recurring telemetry cycles without performing full hardware fingerprints each tick.
type SNMPHealthMonitor struct {
	mu      sync.Mutex
	cfg     SNMPConfig
	factory DynamicSessionFactory
	plan    *SNMPDiscoveryPlan
}

// NewSNMPMonitor creates a reusable dynamic SNMP health monitor.
func NewSNMPMonitor(cfg SNMPConfig, factory DynamicSessionFactory) *SNMPHealthMonitor {
	if factory == nil {
		factory = NewRealDynamicSNMPSession
	}
	return &SNMPHealthMonitor{
		cfg:     cfg,
		factory: factory,
	}
}

// ResetPlan clears cached SNMP discovery indexes (e.g. if target changes).
func (m *SNMPHealthMonitor) ResetPlan() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plan = nil
}

// Poll executes one bounded dynamic SNMP acquisition cycle, discovering table indexes at startup
// (or rediscovering if cached indexes fail or become stale).
func (m *SNMPHealthMonitor) Poll(ctx context.Context, estimatedSheets int) SNMPObservation {
	now := time.Now().UTC()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableSNMPObservation(m.cfg.Target, estimatedSheets, now, fmt.Sprintf("context canceled: %v", err))
	}

	m.mu.Lock()
	cfg := m.cfg
	plan := m.plan
	factory := m.factory
	m.mu.Unlock()

	if strings.TrimSpace(cfg.Target) == "" {
		return unavailableSNMPObservation("", estimatedSheets, now, "SNMP target is empty")
	}
	if cfg.Port <= 0 {
		cfg.Port = defaultPort
	}
	if cfg.Community == "" {
		cfg.Community = defaultCommunity
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}

	pollCtx, cancel := context.WithTimeout(ctx, cfg.Timeout+time.Second)
	defer cancel()

	session, err := factory(pollCtx, cfg)
	if err != nil {
		return unavailableSNMPObservation(cfg.Target, estimatedSheets, time.Now().UTC(), fmt.Sprintf("SNMP connect failed: %v", err))
	}
	defer session.Close()

	if plan == nil {
		return m.discoverAndSaveLocked(pollCtx, session, cfg, estimatedSheets)
	}

	probe, err := m.pollTargetedWithPlan(pollCtx, session, cfg, plan)
	if err != nil {
		// Invalidate cached indexes once and retry discovery in case dynamic indexes shifted or walk fallback is needed.
		m.mu.Lock()
		m.plan = nil
		m.mu.Unlock()
		return m.discoverAndSaveLocked(pollCtx, session, cfg, estimatedSheets)
	}

	return EvaluateSNMPProbe(probe, estimatedSheets, time.Now().UTC())
}

func (m *SNMPHealthMonitor) discoverAndSaveLocked(
	ctx context.Context,
	session DynamicSNMPSession,
	cfg SNMPConfig,
	estimatedSheets int,
) SNMPObservation {
	probe, err := DiscoverSNMPDynamic(ctx, session, cfg.Target, cfg.Port)
	if err != nil || probe == nil {
		errMsg := "SNMP dynamic discovery failed"
		if err != nil {
			errMsg = err.Error()
		}
		return unavailableSNMPObservation(cfg.Target, estimatedSheets, time.Now().UTC(), errMsg)
	}
	obs := EvaluateSNMPProbe(probe, estimatedSheets, time.Now().UTC())
	if obs.Available {
		plan := BuildSNMPDiscoveryPlan(probe)
		m.mu.Lock()
		m.plan = plan
		m.mu.Unlock()
	}
	return obs
}

func (m *SNMPHealthMonitor) pollTargetedWithPlan(
	ctx context.Context,
	session DynamicSNMPSession,
	cfg SNMPConfig,
	plan *SNMPDiscoveryPlan,
) (*SNMPProbeResult, error) {
	printerIdx := plan.PrinterIndex
	if printerIdx == "" {
		return nil, fmt.Errorf("cached SNMP plan lacks printer index")
	}

	oidDevStatus := "1.3.6.1.2.1.25.3.2.1.5." + printerIdx
	oidPrtStatus := "1.3.6.1.2.1.25.3.5.1.1." + printerIdx
	oidPrtErr := "1.3.6.1.2.1.25.3.5.1.2." + printerIdx

	oids := []string{oidDevStatus, oidPrtStatus, oidPrtErr}

	if plan.TonerSupplyIndex != "" {
		oids = append(oids,
			"1.3.6.1.2.1.43.11.1.1.7."+plan.TonerSupplyIndex,
			"1.3.6.1.2.1.43.11.1.1.8."+plan.TonerSupplyIndex,
			"1.3.6.1.2.1.43.11.1.1.9."+plan.TonerSupplyIndex,
		)
	}
	if plan.DrumSupplyIndex != "" {
		oids = append(oids,
			"1.3.6.1.2.1.43.11.1.1.7."+plan.DrumSupplyIndex,
			"1.3.6.1.2.1.43.11.1.1.8."+plan.DrumSupplyIndex,
			"1.3.6.1.2.1.43.11.1.1.9."+plan.DrumSupplyIndex,
		)
	}
	for _, trayIdx := range plan.InputTrayIndexes {
		oids = append(oids,
			"1.3.6.1.2.1.43.8.2.1.9."+trayIdx,
			"1.3.6.1.2.1.43.8.2.1.10."+trayIdx,
		)
	}

	packet, err := session.Get(oids)
	if err != nil || packet == nil || packet.Error != gosnmp.NoError {
		// Fall back to essential printer OIDs before declaring targeted poll failure (strict/v1 agents)
		essential := []string{oidPrtStatus, oidPrtErr}
		packet, err = session.Get(essential)
		if err != nil || packet == nil || packet.Error != gosnmp.NoError {
			if err == nil {
				err = fmt.Errorf("SNMP packet error: %v", packet)
			}
			return nil, err
		}
	}

	probe := &SNMPProbeResult{
		TargetHost:    cfg.Target,
		TargetPort:    cfg.Port,
		DiscoveryMode: "SNMP_TARGETED_PLAN",
	}

	var devStatus, prtStatus int64
	var prtErrBytes []byte
	var hasPrtStatus, hasPrtErr bool

	if pdu, ok := findPDU(packet, oidDevStatus, -1); ok && isValidPDU(pdu) {
		if v, okV := pduToInt64(pdu); okV {
			devStatus = v
		}
	}
	if pdu, ok := findPDU(packet, oidPrtStatus, -1); ok && isValidPDU(pdu) {
		if v, okV := pduToInt64(pdu); okV {
			prtStatus = v
			hasPrtStatus = true
		}
	}
	if pdu, ok := findPDU(packet, oidPrtErr, -1); ok && isValidPDU(pdu) {
		if b, okB := pduToBytes(pdu); okB {
			prtErrBytes = b
			hasPrtErr = true
		}
	}

	if !hasPrtStatus && !hasPrtErr {
		return nil, fmt.Errorf("targeted SNMP GET lacked printer status and error state for index %q", printerIdx)
	}

	probe.Printers = []SNMPPrinterDevice{{
		Index:            printerIdx,
		DeviceStatus:     int(devStatus),
		DeviceStatusDesc: FormatHRDeviceStatus(devStatus),
		Status:           int(prtStatus),
		StatusDesc:       FormatHRPrinterStatus(prtStatus),
		ErrorRaw:         prtErrBytes,
		ErrorFlags:       DecodeErrorBitmask(prtErrBytes),
	}}

	extractSupply := func(idx string) {
		if idx == "" {
			return
		}
		meta := plan.SupplyMeta[idx]
		unit := meta.Unit
		var maxCap, lvl int64 = -2, -2
		var hasAny bool
		if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.11.1.1.7."+idx, -1); ok && isValidPDU(pdu) {
			if v, okV := pduToInt64(pdu); okV {
				unit = v
			}
		}
		if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.11.1.1.8."+idx, -1); ok && isValidPDU(pdu) {
			if v, okV := pduToInt64(pdu); okV {
				maxCap = v
				hasAny = true
			}
		}
		if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.11.1.1.9."+idx, -1); ok && isValidPDU(pdu) {
			if v, okV := pduToInt64(pdu); okV {
				lvl = v
				hasAny = true
			}
		}
		if !hasAny {
			return
		}
		lvlDesc, pct := calculateSupplyLevel(lvl, maxCap, unit)
		probe.Supplies = append(probe.Supplies, SNMPSupply{
			Index:       idx,
			Description: meta.Description,
			Type:        meta.Type,
			TypeName:    FormatSupplyType(meta.Type),
			Class:       meta.Class,
			ClassName:   FormatSupplyClass(meta.Class),
			Unit:        unit,
			UnitName:    FormatSupplyUnit(unit),
			MaxCapacity: maxCap,
			Level:       lvl,
			LevelDesc:   lvlDesc,
			Percent:     pct,
		})
	}

	extractSupply(plan.TonerSupplyIndex)
	extractSupply(plan.DrumSupplyIndex)

	for _, trayIdx := range plan.InputTrayIndexes {
		var maxCap, lvl int64 = -2, -2
		var hasMax, hasLvl bool
		if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.8.2.1.9."+trayIdx, -1); ok && isValidPDU(pdu) {
			if v, okV := pduToInt64(pdu); okV {
				maxCap = v
				hasMax = true
			}
		}
		if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.8.2.1.10."+trayIdx, -1); ok && isValidPDU(pdu) {
			if v, okV := pduToInt64(pdu); okV {
				lvl = v
				hasLvl = true
			}
		}
		if hasMax || hasLvl {
			probe.Inputs = append(probe.Inputs, SNMPInputTray{
				Index:         trayIdx,
				MaxCapacity:   maxCap,
				CurrentLevel:  lvl,
				LevelKnown:    hasLvl && lvl >= 0,
				CapacityKnown: hasMax && maxCap > 0,
			})
		}
	}

	// Bounded walk of prtAlertTable for active critical alerts (skip on fallback-only agents)
	if plan.DiscoveryMode != "SNMP_GET_FALLBACK" && ctx.Err() == nil {
		if alerts, aErr := walkPrtAlertTable(session); aErr == nil && len(alerts) > 0 {
			probe.Alerts = alerts
		} else if aErr != nil && isTimeoutError(aErr) {
			return nil, fmt.Errorf("SNMP alert table walk timed out: %w", aErr)
		}
	}

	return probe, nil
}

func unavailableSNMPObservation(target string, estimatedSheets int, now time.Time, errMsg string) SNMPObservation {
	h := offlineHealth(estimatedSheets, errMsg)
	h.UpdatedAt = now
	h.TonerLevelPercent = -1
	h.DrumLifePercent = -1
	h.InputTrayLevel = -1
	h.InputTrayCapacity = -1
	return SNMPObservation{
		Available:         false,
		ObservedAt:        now,
		TargetHost:        target,
		TonerPercent:      -1,
		DrumPercent:       -1,
		InputTrayLevel:    -1,
		InputTrayCapacity: -1,
		Error:             errMsg,
		Health:            h,
	}
}

// EvaluateSNMPProbe translates a dynamic SNMPProbeResult into a canonical RFC 2790/3805
// SNMPObservation and contracts.PrinterHealth.
func EvaluateSNMPProbe(probe *SNMPProbeResult, estimatedSheets int, now time.Time) SNMPObservation {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if probe == nil {
		return unavailableSNMPObservation("", estimatedSheets, now, "nil SNMP probe result")
	}
	if len(probe.Printers) == 0 && len(probe.Alerts) == 0 && len(probe.Supplies) == 0 && len(probe.Inputs) == 0 {
		msg := probe.ProbeError
		if msg == "" {
			msg = "SNMP probe returned no printer metrics"
		}
		return unavailableSNMPObservation(probe.TargetHost, estimatedSheets, now, msg)
	}

	obs := SNMPObservation{
		Available:         true,
		ObservedAt:        now,
		TargetHost:        probe.TargetHost,
		DiscoveryMode:     probe.DiscoveryMode,
		Alerts:            append([]SNMPAlert(nil), probe.Alerts...),
		TonerPercent:      -1,
		DrumPercent:       -1,
		InputTrayLevel:    -1,
		InputTrayCapacity: -1,
	}

	var devStatus int64
	var prtStatus int64
	var flags PrinterErrorFlags
	var hasFlags bool
	if len(probe.Printers) > 0 {
		p := probe.Printers[0]
		devStatus = int64(p.DeviceStatus)
		prtStatus = int64(p.Status)
		flags = p.ErrorFlags
		hasFlags = len(p.ErrorRaw) > 0
	}
	obs.DeviceStatus = devStatus
	obs.PrinterStatus = prtStatus
	obs.ErrorFlags = flags
	obs.HasErrorFlags = hasFlags

	// Dynamic supply classification (toner & drum indexes resolved dynamically, never hardcoded .1.1/.1.2)
	plan := BuildSNMPDiscoveryPlan(probe)
	for _, s := range probe.Supplies {
		if plan != nil && s.Index == plan.TonerSupplyIndex && s.Percent != nil && s.Level >= 0 {
			pct := int(*s.Percent + 0.5)
			if pct < 0 {
				pct = 0
			} else if pct > 100 {
				pct = 100
			}
			obs.TonerPercent = pct
			obs.TonerKnown = true
		}
		if plan != nil && s.Index == plan.DrumSupplyIndex && s.Percent != nil && s.Level >= 0 {
			// RFC 3805: treat verified positive or explicit percentage drum measurements as known,
			// while unverified/missing units or capacity leave DrumKnown=false. Zero drum alone never blocks.
			if s.Level > 0 || (s.Unit == 19 && s.MaxCapacity > 0) {
				pct := int(*s.Percent + 0.5)
				if pct < 0 {
					pct = 0
				} else if pct > 100 {
					pct = 100
				}
				obs.DrumPercent = pct
				obs.DrumKnown = true
			}
		}
	}

	// Dynamic input tray levels (RFC 3805: -2 unknown is NEVER coerced to 0)
	trayEmptyConfirmed := false
	for _, tray := range probe.Inputs {
		if tray.CapacityKnown && !obs.InputTrayCapacityKnown {
			obs.InputTrayCapacity = int(tray.MaxCapacity)
			obs.InputTrayCapacityKnown = true
		}
		if tray.LevelKnown && !obs.InputTrayLevelKnown {
			obs.InputTrayLevel = int(tray.CurrentLevel)
			obs.InputTrayLevelKnown = true
		}
		if tray.LevelKnown && tray.CurrentLevel == 0 {
			trayEmptyConfirmed = true
		}
	}

	// Cover status from prtCoverTable
	coverOpen := flags.DoorOpen
	for _, cov := range probe.Covers {
		if cov.Status == 3 || cov.Status == 5 { // RFC 3805: 3=coverOpen, 5=interlockOpen
			coverOpen = true
		}
	}

	paperJam := flags.Jammed
	outputFull := flags.OutputFull
	paperEmpty := flags.NoPaper || flags.InputTrayEmpty || flags.InputTrayMissing || trayEmptyConfirmed
	tonerEmpty := flags.NoToner
	paperLow := flags.LowPaper
	tonerLow := flags.LowToner || (obs.TonerKnown && obs.TonerPercent <= 10 && obs.TonerPercent > 0)

	addFault := func(code, msg string) {
		obs.HasBlockingFault = true
		obs.BlockingFaults = append(obs.BlockingFaults, contracts.HealthReason{
			Code:    code,
			Source:  "snmp",
			Message: msg,
		})
	}
	addWarning := func(code, msg string) {
		obs.Warnings = append(obs.Warnings, contracts.HealthReason{
			Code:    code,
			Source:  "snmp",
			Message: msg,
		})
	}

	// RFC 2790 hrDeviceStatus == 5 (down)
	if devStatus == 5 {
		addFault("snmp_device_down", "SNMP hrDeviceStatus reports device down (5)")
	}
	if flags.Offline || prtStatus == 7 {
		addFault("snmp_offline", "SNMP reports printer offline")
	}
	if flags.ServiceRequested {
		addFault("snmp_service_requested", "SNMP reports service requested")
	}
	if flags.MarkerSupplyMissing {
		addFault("marker_supply_missing", "SNMP reports marker supply missing")
	}

	// RFC 3805 prtAlertTable critical alerts (severity == 3)
	for _, al := range probe.Alerts {
		descLower := strings.ToLower(al.Description)
		switch al.Severity {
		case 3: // critical / criticalBinaryChangeEvent
			switch {
			case al.Code == 8 || strings.Contains(descLower, "jam"):
				paperJam = true
				addFault("paper_jam", fmt.Sprintf("SNMP critical alert: %s", firstNonEmpty(al.Description, al.CodeName)))
			case al.Code == 18 || al.Code == 11 ||
				strings.Contains(descLower, "tray empty") ||
				strings.Contains(descLower, "out of paper") ||
				strings.Contains(descLower, "no paper") ||
				strings.Contains(descLower, "media empty") ||
				strings.Contains(descLower, "paper empty"):
				paperEmpty = true
				addFault("paper_empty", fmt.Sprintf("SNMP critical alert: %s", firstNonEmpty(al.Description, al.CodeName)))
			case al.Code == 3 || al.Code == 5 || al.Code == 9 ||
				strings.Contains(descLower, "cover open") ||
				strings.Contains(descLower, "door open") ||
				strings.Contains(descLower, "interlock open"):
				coverOpen = true
				addFault("cover_open", fmt.Sprintf("SNMP critical alert: %s", firstNonEmpty(al.Description, al.CodeName)))
			case al.Code == 22 || al.Code == 20 || (strings.Contains(descLower, "output") && strings.Contains(descLower, "full")):
				outputFull = true
				addFault("output_bin_full", fmt.Sprintf("SNMP critical alert: %s", firstNonEmpty(al.Description, al.CodeName)))
			case al.Code == 25 || al.Code == 28 || strings.Contains(descLower, "toner empty"):
				tonerEmpty = true
				addFault("toner_empty", fmt.Sprintf("SNMP critical alert: %s", firstNonEmpty(al.Description, al.CodeName)))
			default:
				addFault("snmp_critical_alert", fmt.Sprintf("SNMP critical alert (%s/%s): %s", al.GroupName, al.CodeName, al.Description))
			}
		case 4, 5: // warnings
			switch al.Code {
			case 17:
				paperLow = true
				addWarning("paper_low", firstNonEmpty(al.Description, "input media supply low"))
			case 26:
				tonerLow = true
				addWarning("toner_low", firstNonEmpty(al.Description, "toner low"))
			case 21:
				addWarning("output_bin_near_full", firstNonEmpty(al.Description, "output bin near full"))
			}
		}
	}

	if paperJam {
		addFault("paper_jam", "SNMP detected paper jam")
	}
	if coverOpen {
		addFault("cover_open", "SNMP detected cover/door open")
	}
	if outputFull {
		addFault("output_bin_full", "SNMP detected output bin full")
	}
	if paperEmpty {
		addFault("paper_empty", "SNMP detected paper empty")
	}
	if tonerEmpty {
		addFault("toner_empty", "SNMP detected toner empty")
	}
	if paperLow {
		addWarning("paper_low", "SNMP detected low paper")
	}
	if tonerLow {
		addWarning("toner_low", "SNMP detected low toner")
	}
	if flags.OutputNearFull {
		addWarning("output_bin_near_full", "SNMP detected output bin near full")
	}

	// Deduplicate reasons while preserving order
	obs.BlockingFaults = dedupeHealthReasons(obs.BlockingFaults)
	obs.Warnings = dedupeHealthReasons(obs.Warnings)

	// Derive operational state & paper tray status
	state := "UNKNOWN"
	switch prtStatus {
	case 3:
		state = "IDLE"
	case 4:
		state = "PRINTING"
	case 5:
		state = "WARMING_UP"
	case 6:
		state = "ERROR"
	case 7:
		state = "OFFLINE"
	}
	online := true
	if flags.Offline || prtStatus == 7 {
		online = false
		state = "OFFLINE"
	} else if obs.HasBlockingFault {
		state = "ERROR"
	}

	trayStatus := "UNKNOWN"
	if paperEmpty {
		trayStatus = "EMPTY"
	} else if paperLow {
		trayStatus = "LOW"
	} else if len(probe.Printers) > 0 && hasFlags {
		trayStatus = "OK"
	}

	var errMsgs []string
	for _, r := range obs.BlockingFaults {
		if r.Message != "" {
			errMsgs = append(errMsgs, r.Message)
		} else {
			errMsgs = append(errMsgs, r.Code)
		}
	}

	obs.Health = contracts.PrinterHealth{
		Online:                 online,
		State:                  state,
		PaperTrayStatus:        trayStatus,
		EstimatedSheets:        estimatedSheets,
		TonerLevelPercent:      obs.TonerPercent,
		TonerLevelKnown:        obs.TonerKnown,
		DrumLifePercent:        obs.DrumPercent,
		DrumLifeKnown:          obs.DrumKnown,
		InputTrayLevel:         obs.InputTrayLevel,
		InputTrayLevelKnown:    obs.InputTrayLevelKnown,
		InputTrayCapacity:      obs.InputTrayCapacity,
		InputTrayCapacityKnown: obs.InputTrayCapacityKnown,
		CoverOpen:              coverOpen,
		PaperJam:               paperJam,
		OutputBinFull:          outputFull,
		BlockingReasons:        obs.BlockingFaults,
		Warnings:               obs.Warnings,
		ErrorMessage:           strings.Join(errMsgs, "; "),
		UpdatedAt:              now,
	}
	return obs
}

func dedupeHealthReasons(in []contracts.HealthReason) []contracts.HealthReason {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]contracts.HealthReason, 0, len(in))
	for _, r := range in {
		key := r.Source + ":" + r.Code
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// HardwareTelemetry encapsulates physical hardware readings from SNMP.
type HardwareTelemetry struct {
	Online      bool
	LifeCount   int64
	HasLife     bool
	Flags       PrinterErrorFlags
	HasFlags    bool
	StatusState string
	ErrorMsg    string
}

func pollHardwareTelemetryWithFactory(
	ctx context.Context,
	factory sessionFactory,
) HardwareTelemetry {
	if ctx == nil || ctx.Err() != nil {
		return HardwareTelemetry{Online: false, ErrorMsg: "context canceled"}
	}
	session, err := factory(ctx)
	if err != nil {
		return HardwareTelemetry{Online: false, ErrorMsg: err.Error()}
	}
	defer session.Close()

	packet, err := session.Get([]string{oidHrPrinterStatus, oidHrPrinterDetectedErrorState, oidPrtMarkerLifeCount})
	if err != nil || packet == nil {
		// Fallback to essential OIDs if prtMarkerLifeCount is unsupported on strict agents
		packet, err = session.Get([]string{oidHrPrinterStatus, oidHrPrinterDetectedErrorState})
		if err != nil || packet == nil {
			return HardwareTelemetry{Online: false, ErrorMsg: "SNMP GET failed"}
		}
	}

	var telem HardwareTelemetry
	telem.Online = true

	// 1. Status
	if pdu, ok := findPDU(packet, oidHrPrinterStatus, 0); ok && isValidPDU(pdu) {
		if v, okVal := pduToInt64(pdu); okVal {
			switch int(v) {
			case 3:
				telem.StatusState = "IDLE"
			case 4:
				telem.StatusState = "PRINTING"
			case 5:
				telem.StatusState = "WARMING_UP"
			default:
				telem.StatusState = "UNKNOWN"
			}
		}
	}

	// 2. Error flags
	if pdu, ok := findPDU(packet, oidHrPrinterDetectedErrorState, 1); ok && isValidPDU(pdu) {
		var rawBytes []byte
		switch v := pdu.Value.(type) {
		case []byte:
			rawBytes = v
		case string:
			rawBytes = []byte(v)
		}
		if len(rawBytes) > 0 {
			telem.Flags = DecodeErrorBitmask(rawBytes)
			telem.HasFlags = true
		}
	}

	// 3. LifeCount
	if pdu, ok := findPDU(packet, oidPrtMarkerLifeCount, 2); ok && isValidPDU(pdu) {
		if v, okVal := pduToInt64(pdu); okVal {
			telem.LifeCount = v
			telem.HasLife = true
		}
	}

	return telem
}

// PollHardwareTelemetry queries physical hardware telemetry from SNMP.
func PollHardwareTelemetry(ctx context.Context, cfg SNMPConfig) HardwareTelemetry {
	return pollHardwareTelemetryWithFactory(ctx, realSessionFactoryWithConfig(cfg))
}

// PollMarkerLifeCount returns the current physical odometer count from SNMP, or -1 if unavailable.
func PollMarkerLifeCount(ctx context.Context, cfg SNMPConfig) int64 {
	t := PollHardwareTelemetry(ctx, cfg)
	if t.Online && t.HasLife {
		return t.LifeCount
	}
	return -1
}
