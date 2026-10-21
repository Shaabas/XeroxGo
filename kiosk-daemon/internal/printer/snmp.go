package printer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"xeroxgo/pkg/contracts"
)

const (
	defaultTarget    = "192.168.99.2"
	defaultPort      = 161
	defaultCommunity = "public"
	defaultTimeout   = 2 * time.Second
	defaultRetries   = 0

	oidHrPrinterStatus             = "1.3.6.1.2.1.25.3.5.1.1.1"
	oidHrPrinterDetectedErrorState = "1.3.6.1.2.1.25.3.5.1.2.1"
	oidPrtMarkerLifeCount          = "1.3.6.1.2.1.43.10.2.1.4.1.1"
)

var queriedOIDs = []string{
	oidHrPrinterStatus,
	oidHrPrinterDetectedErrorState,
	oidPrtMarkerLifeCount,
}

type PrinterErrorFlags struct {
	LowPaper bool
	NoPaper  bool
	LowToner bool
	NoToner  bool
	DoorOpen bool
	Jammed   bool
	Offline  bool
}

func DecodeErrorBitmask(raw []byte) PrinterErrorFlags {
	var flags PrinterErrorFlags
	if len(raw) == 0 {
		return flags
	}
	b := raw[0]
	flags.LowPaper = b&0x80 != 0
	flags.NoPaper = b&0x40 != 0
	flags.LowToner = b&0x20 != 0
	flags.NoToner = b&0x10 != 0
	flags.DoorOpen = b&0x08 != 0
	flags.Jammed = b&0x04 != 0
	flags.Offline = b&0x02 != 0
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

func realSessionFactory(ctx context.Context) (snmpSession, error) {
	client := &gosnmp.GoSNMP{
		Target:    defaultTarget,
		Port:      defaultPort,
		Community: defaultCommunity,
		Version:   gosnmp.Version2c,
		Timeout:   defaultTimeout,
		Retries:   defaultRetries,
		Context:   ctx,
	}

	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to printer at %s:%d: %w", defaultTarget, defaultPort, err)
	}

	return &realSnmpSession{client: client}, nil
}

func offlineHealth(estimatedSheets int, errMsg string) contracts.PrinterHealth {
	return contracts.PrinterHealth{
		Online:          false,
		State:           "OFFLINE",
		PaperTrayStatus: "OK",
		EstimatedSheets: estimatedSheets,
		ErrorMessage:    errMsg,
		UpdatedAt:       time.Now().UTC(),
	}
}

func normalizeOID(oid string) string {
	return strings.TrimPrefix(oid, ".")
}

func findPDU(packet *gosnmp.SnmpPacket, targetOID string, fallbackIndex int) (gosnmp.SnmpPDU, bool) {
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
	if packet.Error != gosnmp.NoError {
		return offlineHealth(estimatedSheets, fmt.Sprintf("SNMP error status in response: %v (index %d)", packet.Error, packet.ErrorIndex))
	}

	// 1. hrPrinterStatus (1.3.6.1.2.1.25.3.5.1.1.1)
	pduStatus, ok := findPDU(packet, oidHrPrinterStatus, 0)
	if !ok {
		return offlineHealth(estimatedSheets, fmt.Sprintf("missing required OID: %s", oidHrPrinterStatus))
	}
	if pduStatus.Type != gosnmp.Integer {
		return offlineHealth(estimatedSheets, fmt.Sprintf("invalid PDU type for %s: got %v, want Integer", oidHrPrinterStatus, pduStatus.Type))
	}

	var statusVal int
	switch v := pduStatus.Value.(type) {
	case int:
		statusVal = v
	case int32:
		statusVal = int(v)
	case int64:
		statusVal = int(v)
	case uint:
		statusVal = int(v)
	case uint32:
		statusVal = int(v)
	case uint64:
		statusVal = int(v)
	default:
		return offlineHealth(estimatedSheets, fmt.Sprintf("cannot parse %s value of type %T as integer", oidHrPrinterStatus, pduStatus.Value))
	}

	// 2. hrPrinterDetectedErrorState (1.3.6.1.2.1.25.3.5.1.2.1)
	pduErrorState, ok := findPDU(packet, oidHrPrinterDetectedErrorState, 1)
	if !ok {
		return offlineHealth(estimatedSheets, fmt.Sprintf("missing required OID: %s", oidHrPrinterDetectedErrorState))
	}
	if pduErrorState.Type != gosnmp.OctetString {
		return offlineHealth(estimatedSheets, fmt.Sprintf("invalid PDU type for %s: got %v, want OctetString", oidHrPrinterDetectedErrorState, pduErrorState.Type))
	}

	var rawBytes []byte
	switch v := pduErrorState.Value.(type) {
	case []byte:
		rawBytes = v
	case string:
		rawBytes = []byte(v)
	default:
		return offlineHealth(estimatedSheets, fmt.Sprintf("invalid OctetString value type for %s: %T", oidHrPrinterDetectedErrorState, pduErrorState.Value))
	}
	flags := DecodeErrorBitmask(rawBytes)

	// 3. prtMarkerLifeCount (1.3.6.1.2.1.43.10.2.1.4.1.1)
	pduMarkerLife, ok := findPDU(packet, oidPrtMarkerLifeCount, 2)
	if !ok {
		return offlineHealth(estimatedSheets, fmt.Sprintf("missing required OID: %s", oidPrtMarkerLifeCount))
	}
	switch pduMarkerLife.Type {
	case gosnmp.Counter32, gosnmp.Counter64, gosnmp.Integer, gosnmp.Gauge32:
	default:
		return offlineHealth(estimatedSheets, fmt.Sprintf("invalid PDU type for %s: got %v", oidPrtMarkerLifeCount, pduMarkerLife.Type))
	}

	var markerCount int64
	switch v := pduMarkerLife.Value.(type) {
	case int:
		markerCount = int64(v)
	case int32:
		markerCount = int64(v)
	case int64:
		markerCount = v
	case uint:
		markerCount = int64(v)
	case uint32:
		markerCount = int64(v)
	case uint64:
		markerCount = int64(v)
	default:
		return offlineHealth(estimatedSheets, fmt.Sprintf("cannot parse %s value of type %T as integer/counter", oidPrtMarkerLifeCount, pduMarkerLife.Value))
	}
	if markerCount < 0 {
		return offlineHealth(estimatedSheets, fmt.Sprintf("invalid negative marker life count: %d", markerCount))
	}

	// Map status
	var statusState string
	switch statusVal {
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
		statusState = "ERROR"
	}

	// Integrate flags
	// PaperTrayStatus
	paperTrayStatus := "OK"
	if flags.NoPaper {
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
		if flags.DoorOpen || flags.Jammed || flags.NoPaper || flags.NoToner {
			finalState = "ERROR"
		} else {
			finalState = statusState
		}
	}

	// ErrorMessage formulation
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
	if flags.NoPaper {
		issues = append(issues, "no paper")
	} else if flags.LowPaper {
		issues = append(issues, "low paper")
	}
	if flags.NoToner {
		issues = append(issues, "no toner")
	} else if flags.LowToner {
		issues = append(issues, "low toner")
	}
	if statusState == "ERROR" && len(issues) == 0 {
		if statusVal == 6 {
			issues = append(issues, "printer error")
		} else {
			issues = append(issues, fmt.Sprintf("unrecognized printer status: %d", statusVal))
		}
	}

	errorMsg := strings.Join(issues, ", ")

	// Telemetry limitation note:
	// Brother HL-L5210DN standard MIB OIDs polled here (hrPrinterStatus, hrPrinterDetectedErrorState,
	// and prtMarkerLifeCount) do not provide granular toner or drum life percentages.
	// Therefore TonerLevelPercent and DrumLifePercent remain at their zero values for this Brick.
	return contracts.PrinterHealth{
		Online:            online,
		State:             finalState,
		PaperTrayStatus:   paperTrayStatus,
		EstimatedSheets:   estimatedSheets,
		TonerLevelPercent: 0,
		DrumLifePercent:   0,
		CoverOpen:         flags.DoorOpen,
		PaperJam:          flags.Jammed,
		OutputBinFull:     false,
		ErrorMessage:      errorMsg,
		UpdatedAt:         time.Now().UTC(),
	}
}

func PollHealth(
	ctx context.Context,
	estimatedSheets int,
) contracts.PrinterHealth {
	return pollHealthWithFactory(ctx, estimatedSheets, realSessionFactory)
}
