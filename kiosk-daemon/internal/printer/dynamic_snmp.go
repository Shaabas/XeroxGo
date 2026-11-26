package printer

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Standard MIB Table Subtree Roots (RFC 1213, RFC 2790, RFC 3805)
const (
	oidSubtreeHRPrinter   = "1.3.6.1.2.1.25.3.5.1"   // hrPrinterTable
	oidSubtreePrtGeneral  = "1.3.6.1.2.1.43.5.1.1"   // prtGeneralTable
	oidSubtreePrtCover    = "1.3.6.1.2.1.43.6.1.1"   // prtCoverTable
	oidSubtreePrtMarker   = "1.3.6.1.2.1.43.10.2.1" // prtMarkerTable
	oidSubtreePrtSupplies = "1.3.6.1.2.1.43.11.1.1" // prtMarkerSuppliesTable
	oidSubtreePrtAlert    = "1.3.6.1.2.1.43.18.1.1" // prtAlertTable
)

// DynamicSNMPSession defines the interface required for dynamic SNMP walks and fallback queries.
type DynamicSNMPSession interface {
	Get(oids []string) (*gosnmp.SnmpPacket, error)
	Walk(rootOid string, walkFn gosnmp.WalkFunc) error
	Close() error
}

// RealDynamicSNMPSession implements DynamicSNMPSession using *gosnmp.GoSNMP.
type RealDynamicSNMPSession struct {
	client *gosnmp.GoSNMP
	stop   context.CancelFunc
}

func (s *RealDynamicSNMPSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	return s.client.Get(oids)
}

func (s *RealDynamicSNMPSession) Walk(rootOid string, walkFn gosnmp.WalkFunc) error {
	return s.client.Walk(rootOid, walkFn)
}

func (s *RealDynamicSNMPSession) Close() error {
	if s.stop != nil {
		s.stop()
	}
	if s.client != nil && s.client.Conn != nil {
		return s.client.Conn.Close()
	}
	return nil
}

// NewRealDynamicSNMPSession creates a real GoSNMP session using SNMPv2c.
func NewRealDynamicSNMPSession(ctx context.Context, cfg SNMPConfig) (DynamicSNMPSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	watchCtx, stopWatch := context.WithCancel(ctx)
	conn := client.Conn
	if conn != nil {
		go func() {
			<-watchCtx.Done()
			// Unblock pending UDP read/write immediately on context cancellation without racing on client.Conn.
			_ = conn.SetDeadline(time.Now())
		}()
	}

	return &RealDynamicSNMPSession{client: client, stop: stopWatch}, nil
}

// SNMPPrinterDevice represents an entry from RFC 2790 hrPrinterTable and hrDeviceTable.
type SNMPPrinterDevice struct {
	Index            string            `json:"index"` // hrDeviceIndex
	DeviceStatus     int               `json:"device_status,omitempty"`
	DeviceStatusDesc string            `json:"device_status_desc,omitempty"`
	Status           int               `json:"status"`
	StatusDesc       string            `json:"status_desc"`
	ErrorRaw         []byte            `json:"error_raw,omitempty"`
	ErrorFlags       PrinterErrorFlags `json:"error_flags"`
}

// SNMPInputTray represents an entry from RFC 3805 prtInputTable.
type SNMPInputTray struct {
	Index         string `json:"index"` // hrDeviceIndex.inputIndex
	Name          string `json:"name,omitempty"`
	Description   string `json:"description,omitempty"`
	Unit          int64  `json:"unit,omitempty"`
	MaxCapacity   int64  `json:"max_capacity"`  // -1=other, -2=unknown, >0=capacity
	CurrentLevel  int64  `json:"current_level"` // -1=other, -2=unknown, -3=atLeastOneUnit, >=0=level
	Status        int64  `json:"status,omitempty"`
	LevelKnown    bool   `json:"level_known"`
	CapacityKnown bool   `json:"capacity_known"`
}

// SNMPCover represents an entry from RFC 3805 prtCoverTable.
type SNMPCover struct {
	Index       string `json:"index"` // hrDeviceIndex.coverIndex
	Description string `json:"description"`
	Status      int    `json:"status"`
	StatusDesc  string `json:"status_desc"`
}

// SNMPMarker represents an entry from RFC 3805 prtMarkerTable.
type SNMPMarker struct {
	Index            string `json:"index"` // hrDeviceIndex.markerIndex
	CounterUnit      int64  `json:"counter_unit"`
	CounterUnitName  string `json:"counter_unit_name"`
	IsPageCounter    bool   `json:"is_page_counter"`
	HasLifeCount     bool   `json:"has_life_count"`
	LifeCount        int64  `json:"life_count"`
	PowerOnCount     int64  `json:"power_on_count"`
	HasPowerOnCount  bool   `json:"has_power_on_count"`
}

// SNMPSupply represents an entry from RFC 3805 prtMarkerSuppliesTable.
type SNMPSupply struct {
	Index       string   `json:"index"` // hrDeviceIndex.supplyIndex
	Description string   `json:"description"`
	Type        int64    `json:"type"`
	TypeName    string   `json:"type_name"`
	Class       int64    `json:"class"`
	ClassName   string   `json:"class_name"`
	Unit        int64    `json:"unit"`
	UnitName    string   `json:"unit_name"`
	MaxCapacity int64    `json:"max_capacity"`
	Level       int64    `json:"level"`
	LevelDesc   string   `json:"level_desc"`
	Percent     *float64 `json:"percent,omitempty"`
}

// SNMPAlert represents an entry from RFC 3805 prtAlertTable.
type SNMPAlert struct {
	Index        string `json:"index"` // hrDeviceIndex.alertIndex
	Severity     int64  `json:"severity"`
	SeverityName string `json:"severity_name"`
	Training     int64  `json:"training"`
	TrainingName string `json:"training_name"`
	Group        int64  `json:"group"`
	GroupName    string `json:"group_name"`
	Code         int64  `json:"code"`
	CodeName     string `json:"code_name"`
	Description  string `json:"description"`
	Time         uint32 `json:"time"`
}

// SNMPProbeResult holds all dynamically discovered SNMP information.
type SNMPProbeResult struct {
	TargetHost    string              `json:"target_host"`
	TargetPort    int                 `json:"target_port"`
	DiscoveryMode string              `json:"discovery_mode"` // SNMP_TABLE_WALK, SNMP_GET_FALLBACK, or SNMP_OFFLINE
	SysDescr      string              `json:"sys_descr,omitempty"`
	SysObjectID   string              `json:"sys_object_id,omitempty"`
	SysUpTime     string              `json:"sys_uptime,omitempty"`
	PrinterName   string              `json:"printer_name,omitempty"`
	Printers      []SNMPPrinterDevice `json:"printers,omitempty"`
	Covers        []SNMPCover         `json:"covers,omitempty"`
	Inputs        []SNMPInputTray     `json:"inputs,omitempty"`
	Markers       []SNMPMarker        `json:"markers,omitempty"`
	Supplies      []SNMPSupply        `json:"supplies,omitempty"`
	Alerts        []SNMPAlert         `json:"alerts,omitempty"`
	ProbeError    string              `json:"probe_error,omitempty"`
}

// DiscoverSNMPDynamic walks standard SNMP tables dynamically.
// If table walk is unsupported, times out, or returns no data, it falls back to querying standard OIDs via GET.
func DiscoverSNMPDynamic(ctx context.Context, session DynamicSNMPSession, host string, port int) (*SNMPProbeResult, error) {
	res := &SNMPProbeResult{
		TargetHost:    host,
		TargetPort:    port,
		DiscoveryMode: "SNMP_TABLE_WALK",
	}

	if session == nil {
		res.DiscoveryMode = "SNMP_OFFLINE"
		res.ProbeError = "nil SNMP session"
		return res, fmt.Errorf("snmp session is nil")
	}

	// 1. Query System Group scalar OIDs (sysDescr, sysObjectID, sysUpTime)
	if err := ctx.Err(); err != nil {
		res.DiscoveryMode = "SNMP_OFFLINE"
		res.ProbeError = err.Error()
		return res, err
	}
	sysOIDs := []string{
		"1.3.6.1.2.1.1.1.0", // sysDescr
		"1.3.6.1.2.1.1.2.0", // sysObjectID
		"1.3.6.1.2.1.1.3.0", // sysUpTime
	}
	if sysPacket, err := session.Get(sysOIDs); err == nil && sysPacket != nil {
		for _, v := range sysPacket.Variables {
			norm := normalizeOID(v.Name)
			switch norm {
			case "1.3.6.1.2.1.1.1.0":
				if s, ok := pduToString(v); ok {
					res.SysDescr = s
				}
			case "1.3.6.1.2.1.1.2.0":
				if s, ok := pduToString(v); ok {
					res.SysObjectID = s
				}
			case "1.3.6.1.2.1.1.3.0":
				if val, ok := pduToInt64(v); ok {
					d := time.Duration(val*10) * time.Millisecond
					res.SysUpTime = d.String()
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		res.DiscoveryMode = "SNMP_OFFLINE"
		res.ProbeError = err.Error()
		return res, err
	}

	// 2. Dynamic Table Walks
	walkCount := 0

	// Walk hrPrinterTable (1.3.6.1.2.1.25.3.5.1)
	hrPrinters, errHR := walkHRPrinterTable(session)
	if errHR == nil && len(hrPrinters) > 0 {
		res.Printers = hrPrinters
		walkCount++
	}

	// If the initial table walk timed out, the SNMP agent is not responding to Walk (GetNext).
	// Immediately stop attempting subsequent walks and proceed to standard GET fallback.
	if !isTimeoutError(errHR) {
		// Walk hrDeviceTable hrDeviceStatus (1.3.6.1.2.1.25.3.2.1.5) and join onto hrPrinterTable devices
		if devStatuses, errDev := walkHRDeviceStatus(session); errDev == nil && len(devStatuses) > 0 {
			for i := range res.Printers {
				if ds, ok := devStatuses[res.Printers[i].Index]; ok {
					res.Printers[i].DeviceStatus = int(ds)
					res.Printers[i].DeviceStatusDesc = FormatHRDeviceStatus(ds)
				}
			}
		}

		// Walk prtGeneralTable (1.3.6.1.2.1.43.5.1.1)
		prtName := walkPrtGeneralTable(session)
		if prtName != "" {
			res.PrinterName = prtName
		}

		// Walk prtCoverTable (1.3.6.1.2.1.43.6.1.1)
		covers, errCover := walkPrtCoverTable(session)
		if errCover == nil && len(covers) > 0 {
			res.Covers = covers
			walkCount++
		}

		// Walk prtInputTable (1.3.6.1.2.1.43.8.2.1)
		inputs, errInputs := walkPrtInputTable(session)
		if errInputs == nil && len(inputs) > 0 {
			res.Inputs = inputs
			walkCount++
		}

		// Walk prtMarkerTable (1.3.6.1.2.1.43.10.2.1)
		markers, errMarkers := walkPrtMarkerTable(session)
		if errMarkers == nil && len(markers) > 0 {
			res.Markers = markers
			walkCount++
		}

		// Walk prtMarkerSuppliesTable (1.3.6.1.2.1.43.11.1.1)
		supplies, errSupplies := walkPrtMarkerSuppliesTable(session)
		if errSupplies == nil && len(supplies) > 0 {
			res.Supplies = supplies
			walkCount++
		}

		// Walk prtAlertTable (1.3.6.1.2.1.43.18.1.1)
		alerts, errAlerts := walkPrtAlertTable(session)
		if errAlerts == nil && len(alerts) > 0 {
			res.Alerts = alerts
			walkCount++
		}
	}

	// 3. Fallback: If no table rows were discovered via Walk, attempt standard GET queries
	if walkCount == 0 {
		fallbackResult, errFallback := querySNMPStandardFallback(session)
		if errFallback == nil && fallbackResult != nil {
			res.DiscoveryMode = "SNMP_GET_FALLBACK"
			if len(fallbackResult.Printers) > 0 {
				res.Printers = fallbackResult.Printers
			}
			if len(fallbackResult.Markers) > 0 {
				res.Markers = fallbackResult.Markers
			}
			if len(fallbackResult.Supplies) > 0 {
				res.Supplies = fallbackResult.Supplies
			}
			if res.SysDescr == "" && fallbackResult.SysDescr != "" {
				res.SysDescr = fallbackResult.SysDescr
			}
		} else if res.SysDescr == "" {
			res.DiscoveryMode = "SNMP_FAILED"
			res.ProbeError = "SNMP walk and fallback GET returned no data"
			return res, fmt.Errorf("SNMP query failed on %s:%d", host, port)
		}
	}

	return res, nil
}

// walkHRPrinterTable discovers all hrPrinterTable rows (RFC 2790).
// Columns: 1 = hrPrinterStatus, 2 = hrPrinterDetectedErrorState.
func walkHRPrinterTable(session DynamicSNMPSession) ([]SNMPPrinterDevice, error) {
	root := "1.3.6.1.2.1.25.3.5.1"
	statusMap := make(map[string]int)
	errorMap := make(map[string][]byte)

	const maxAlertWalkPDUs = 512
	walkPDUs := 0
	err := session.Walk(root, func(pdu gosnmp.SnmpPDU) error {
		walkPDUs++
		if walkPDUs > maxAlertWalkPDUs {
			return fmt.Errorf("SNMP table walk exceeded %d PDUs", maxAlertWalkPDUs)
		}
		norm := normalizeOID(pdu.Name)
		col, idx, ok := parseTableOID(norm, root)
		if !ok {
			return nil
		}
		switch col {
		case 1: // hrPrinterStatus
			if val, ok := pduToInt64(pdu); ok {
				statusMap[idx] = int(val)
			}
		case 2: // hrPrinterDetectedErrorState
			if b, ok := pduToBytes(pdu); ok {
				errorMap[idx] = b
			}
		}
		return nil
	})
	if err != nil && len(statusMap) == 0 && len(errorMap) == 0 {
		return nil, err
	}

	var devices []SNMPPrinterDevice
	// Iterate through discovered indexes
	seen := make(map[string]bool)
	for idx := range statusMap {
		seen[idx] = true
	}
	for idx := range errorMap {
		seen[idx] = true
	}

	for _, idx := range sortedKeys(seen) {
		st := statusMap[idx]
		errBytes := errorMap[idx]
		devices = append(devices, SNMPPrinterDevice{
			Index:      idx,
			Status:     st,
			StatusDesc: FormatHRPrinterStatus(int64(st)),
			ErrorRaw:   errBytes,
			ErrorFlags: DecodeErrorBitmask(errBytes),
		})
	}

	return devices, nil
}

// walkHRDeviceStatus discovers hrDeviceStatus (column 5 of hrDeviceTable: 1.3.6.1.2.1.25.3.2.1.5).
func walkHRDeviceStatus(session DynamicSNMPSession) (map[string]int64, error) {
	root := "1.3.6.1.2.1.25.3.2.1"
	statusMap := make(map[string]int64)
	err := session.Walk(root+".5", func(pdu gosnmp.SnmpPDU) error {
		norm := normalizeOID(pdu.Name)
		col, idx, ok := parseTableOID(norm, root)
		if !ok || col != 5 {
			return nil
		}
		if val, ok := pduToInt64(pdu); ok {
			statusMap[idx] = val
		}
		return nil
	})
	if err != nil && len(statusMap) == 0 {
		return nil, err
	}
	return statusMap, nil
}

// walkPrtInputTable discovers prtInputTable rows (RFC 3805: 1.3.6.1.2.1.43.8.2.1).
// Columns: 8=CapacityUnit, 9=MaxCapacity, 10=CurrentLevel, 11=Status, 13=Name, 18=Description.
func walkPrtInputTable(session DynamicSNMPSession) ([]SNMPInputTray, error) {
	root := "1.3.6.1.2.1.43.8.2.1"
	unitMap := make(map[string]int64)
	maxMap := make(map[string]int64)
	lvlMap := make(map[string]int64)
	statusMap := make(map[string]int64)
	nameMap := make(map[string]string)
	descMap := make(map[string]string)

	const maxAlertWalkPDUs = 512
	walkPDUs := 0
	err := session.Walk(root, func(pdu gosnmp.SnmpPDU) error {
		walkPDUs++
		if walkPDUs > maxAlertWalkPDUs {
			return fmt.Errorf("SNMP table walk exceeded %d PDUs", maxAlertWalkPDUs)
		}
		norm := normalizeOID(pdu.Name)
		col, idx, ok := parseTableOID(norm, root)
		if !ok {
			return nil
		}
		switch col {
		case 8:
			if v, ok := pduToInt64(pdu); ok {
				unitMap[idx] = v
			}
		case 9:
			if v, ok := pduToInt64(pdu); ok {
				maxMap[idx] = v
			}
		case 10:
			if v, ok := pduToInt64(pdu); ok {
				lvlMap[idx] = v
			}
		case 11:
			if v, ok := pduToInt64(pdu); ok {
				statusMap[idx] = v
			}
		case 13:
			if s, ok := pduToString(pdu); ok {
				nameMap[idx] = s
			}
		case 18:
			if s, ok := pduToString(pdu); ok {
				descMap[idx] = s
			}
		}
		return nil
	})
	if err != nil && len(lvlMap) == 0 && len(maxMap) == 0 && len(nameMap) == 0 {
		return nil, err
	}

	seen := make(map[string]bool)
	for idx := range lvlMap {
		seen[idx] = true
	}
	for idx := range maxMap {
		seen[idx] = true
	}
	for idx := range nameMap {
		seen[idx] = true
	}
	for idx := range descMap {
		seen[idx] = true
	}

	var inputs []SNMPInputTray
	for _, idx := range sortedKeys(seen) {
		maxCap, hasMax := maxMap[idx]
		if !hasMax {
			maxCap = -2
		}
		curLvl, hasLvl := lvlMap[idx]
		if !hasLvl {
			curLvl = -2
		}
		inputs = append(inputs, SNMPInputTray{
			Index:         idx,
			Name:          nameMap[idx],
			Description:   descMap[idx],
			Unit:          unitMap[idx],
			MaxCapacity:   maxCap,
			CurrentLevel:  curLvl,
			Status:        statusMap[idx],
			LevelKnown:    hasLvl && curLvl >= 0,
			CapacityKnown: hasMax && maxCap > 0,
		})
	}
	return inputs, nil
}

// walkPrtGeneralTable queries prtGeneralPrinterName (RFC 3805 prtGeneralTable).
// Column 16 = prtGeneralPrinterName
func walkPrtGeneralTable(session DynamicSNMPSession) string {
	root := "1.3.6.1.2.1.43.5.1.1"
	var name string
	_ = session.Walk(root, func(pdu gosnmp.SnmpPDU) error {
		norm := normalizeOID(pdu.Name)
		col, _, ok := parseTableOID(norm, root)
		if ok && col == 16 {
			if s, ok := pduToString(pdu); ok && strings.TrimSpace(s) != "" {
				name = strings.TrimSpace(s)
			}
		}
		return nil
	})
	return name
}

// walkPrtCoverTable discovers prtCoverTable rows (RFC 3805).
// Columns: 2 = prtCoverDescription, 3 = prtCoverStatus.
func walkPrtCoverTable(session DynamicSNMPSession) ([]SNMPCover, error) {
	root := "1.3.6.1.2.1.43.6.1.1"
	descMap := make(map[string]string)
	statusMap := make(map[string]int)

	const maxAlertWalkPDUs = 512
	walkPDUs := 0
	err := session.Walk(root, func(pdu gosnmp.SnmpPDU) error {
		walkPDUs++
		if walkPDUs > maxAlertWalkPDUs {
			return fmt.Errorf("SNMP table walk exceeded %d PDUs", maxAlertWalkPDUs)
		}
		norm := normalizeOID(pdu.Name)
		col, idx, ok := parseTableOID(norm, root)
		if !ok {
			return nil
		}
		switch col {
		case 2:
			if s, ok := pduToString(pdu); ok {
				descMap[idx] = s
			}
		case 3:
			if v, ok := pduToInt64(pdu); ok {
				statusMap[idx] = int(v)
			}
		}
		return nil
	})
	if err != nil && len(statusMap) == 0 && len(descMap) == 0 {
		return nil, err
	}

	var covers []SNMPCover
	seen := make(map[string]bool)
	for idx := range statusMap {
		seen[idx] = true
	}
	for idx := range descMap {
		seen[idx] = true
	}
	for _, idx := range sortedKeys(seen) {
		st := statusMap[idx]
		covers = append(covers, SNMPCover{
			Index:       idx,
			Description: descMap[idx],
			Status:      st,
			StatusDesc:  FormatCoverStatus(int64(st)),
		})
	}
	return covers, nil
}

// walkPrtMarkerTable discovers prtMarkerTable rows (RFC 3805).
// Columns: 3 = prtMarkerCounterUnit, 4 = prtMarkerLifeCount, 5 = prtMarkerPowerOnCount.
func walkPrtMarkerTable(session DynamicSNMPSession) ([]SNMPMarker, error) {
	root := "1.3.6.1.2.1.43.10.2.1"
	unitMap := make(map[string]int64)
	lifeMap := make(map[string]int64)
	powerMap := make(map[string]int64)

	const maxAlertWalkPDUs = 512
	walkPDUs := 0
	err := session.Walk(root, func(pdu gosnmp.SnmpPDU) error {
		walkPDUs++
		if walkPDUs > maxAlertWalkPDUs {
			return fmt.Errorf("SNMP table walk exceeded %d PDUs", maxAlertWalkPDUs)
		}
		norm := normalizeOID(pdu.Name)
		col, idx, ok := parseTableOID(norm, root)
		if !ok {
			return nil
		}
		switch col {
		case 3: // prtMarkerCounterUnit
			if v, ok := pduToInt64(pdu); ok {
				unitMap[idx] = v
			}
		case 4: // prtMarkerLifeCount
			if v, ok := pduToInt64(pdu); ok {
				lifeMap[idx] = v
			}
		case 5: // prtMarkerPowerOnCount
			if v, ok := pduToInt64(pdu); ok {
				powerMap[idx] = v
			}
		}
		return nil
	})
	if err != nil && len(lifeMap) == 0 && len(unitMap) == 0 {
		return nil, err
	}

	var markers []SNMPMarker
	seen := make(map[string]bool)
	for idx := range lifeMap {
		seen[idx] = true
	}
	for idx := range unitMap {
		seen[idx] = true
	}
	for idx := range powerMap {
		seen[idx] = true
	}

	for _, idx := range sortedKeys(seen) {
		unit, hasUnit := unitMap[idx]
		if !hasUnit {
			unit = -1
		}
		unitName, isPage := FormatMarkerCounterUnit(unit)
		life, hasLife := lifeMap[idx]
		if !hasLife {
			life = -1
		}
		pCount, hasP := powerMap[idx]
		if !hasP {
			pCount = -1
		}
		markers = append(markers, SNMPMarker{
			Index:           idx,
			CounterUnit:     unit,
			CounterUnitName: unitName,
			IsPageCounter:   isPage,
			HasLifeCount:    hasLife,
			LifeCount:       life,
			PowerOnCount:    pCount,
			HasPowerOnCount: hasP,
		})
	}
	return markers, nil
}

// walkPrtMarkerSuppliesTable discovers prtMarkerSuppliesTable rows (RFC 3805).
// Columns:
// 4 = prtMarkerSuppliesClass
// 5 = prtMarkerSuppliesType
// 6 = prtMarkerSuppliesDescription
// 7 = prtMarkerSuppliesSupplyUnit
// 8 = prtMarkerSuppliesMaxCapacity
// 9 = prtMarkerSuppliesLevel
func walkPrtMarkerSuppliesTable(session DynamicSNMPSession) ([]SNMPSupply, error) {
	root := "1.3.6.1.2.1.43.11.1.1"
	classMap := make(map[string]int64)
	typeMap := make(map[string]int64)
	descMap := make(map[string]string)
	unitMap := make(map[string]int64)
	maxMap := make(map[string]int64)
	lvlMap := make(map[string]int64)

	const maxAlertWalkPDUs = 512
	walkPDUs := 0
	err := session.Walk(root, func(pdu gosnmp.SnmpPDU) error {
		walkPDUs++
		if walkPDUs > maxAlertWalkPDUs {
			return fmt.Errorf("SNMP table walk exceeded %d PDUs", maxAlertWalkPDUs)
		}
		norm := normalizeOID(pdu.Name)
		col, idx, ok := parseTableOID(norm, root)
		if !ok {
			return nil
		}
		switch col {
		case 4:
			if v, ok := pduToInt64(pdu); ok {
				classMap[idx] = v
			}
		case 5:
			if v, ok := pduToInt64(pdu); ok {
				typeMap[idx] = v
			}
		case 6:
			if s, ok := pduToString(pdu); ok {
				descMap[idx] = s
			}
		case 7:
			if v, ok := pduToInt64(pdu); ok {
				unitMap[idx] = v
			}
		case 8:
			if v, ok := pduToInt64(pdu); ok {
				maxMap[idx] = v
			}
		case 9:
			if v, ok := pduToInt64(pdu); ok {
				lvlMap[idx] = v
			}
		}
		return nil
	})
	if err != nil && len(lvlMap) == 0 && len(descMap) == 0 {
		return nil, err
	}

	var supplies []SNMPSupply
	seen := make(map[string]bool)
	for idx := range lvlMap {
		seen[idx] = true
	}
	for idx := range descMap {
		seen[idx] = true
	}

	for _, idx := range sortedKeys(seen) {
		cls := classMap[idx]
		typ := typeMap[idx]
		unit := unitMap[idx]
		desc := descMap[idx]
		maxVal := maxMap[idx]
		lvl := lvlMap[idx]

		lvlDesc, percent := calculateSupplyLevel(lvl, maxVal, unit)

		supplies = append(supplies, SNMPSupply{
			Index:       idx,
			Description: desc,
			Type:        typ,
			TypeName:    FormatSupplyType(typ),
			Class:       cls,
			ClassName:   FormatSupplyClass(cls),
			Unit:        unit,
			UnitName:    FormatSupplyUnit(unit),
			MaxCapacity: maxVal,
			Level:       lvl,
			LevelDesc:   lvlDesc,
			Percent:     percent,
		})
	}
	return supplies, nil
}

// walkPrtAlertTable discovers prtAlertTable rows (RFC 3805).
// Columns:
// 2 = prtAlertSeverityLevel
// 3 = prtAlertTrainingLevel
// 4 = prtAlertGroup
// 5 = prtAlertGroupIndex
// 6 = prtAlertLocation
// 7 = prtAlertCode
// 8 = prtAlertDescription
// 9 = prtAlertTime
func walkPrtAlertTable(session DynamicSNMPSession) ([]SNMPAlert, error) {
	root := "1.3.6.1.2.1.43.18.1.1"
	sevMap := make(map[string]int64)
	trnMap := make(map[string]int64)
	grpMap := make(map[string]int64)
	codMap := make(map[string]int64)
	dscMap := make(map[string]string)
	timMap := make(map[string]uint32)

	const maxAlertWalkPDUs = 512
	walkPDUs := 0
	err := session.Walk(root, func(pdu gosnmp.SnmpPDU) error {
		walkPDUs++
		if walkPDUs > maxAlertWalkPDUs {
			return fmt.Errorf("SNMP table walk exceeded %d PDUs", maxAlertWalkPDUs)
		}
		norm := normalizeOID(pdu.Name)
		col, idx, ok := parseTableOID(norm, root)
		if !ok {
			return nil
		}
		switch col {
		case 2:
			if v, ok := pduToInt64(pdu); ok {
				sevMap[idx] = v
			}
		case 3:
			if v, ok := pduToInt64(pdu); ok {
				trnMap[idx] = v
			}
		case 4:
			if v, ok := pduToInt64(pdu); ok {
				grpMap[idx] = v
			}
		case 7:
			if v, ok := pduToInt64(pdu); ok {
				codMap[idx] = v
			}
		case 8:
			if s, ok := pduToString(pdu); ok {
				dscMap[idx] = s
			}
		case 9:
			if v, ok := pduToInt64(pdu); ok {
				timMap[idx] = uint32(v)
			}
		}
		return nil
	})
	if err != nil && len(sevMap) == 0 && len(dscMap) == 0 {
		return nil, err
	}

	var alerts []SNMPAlert
	seen := make(map[string]bool)
	for idx := range sevMap {
		seen[idx] = true
	}
	for idx := range dscMap {
		seen[idx] = true
	}

	for _, idx := range sortedKeys(seen) {
		sev := sevMap[idx]
		trn := trnMap[idx]
		grp := grpMap[idx]
		cod := codMap[idx]
		desc := dscMap[idx]
		tm := timMap[idx]

		alerts = append(alerts, SNMPAlert{
			Index:        idx,
			Severity:     sev,
			SeverityName: FormatAlertSeverity(sev),
			Training:     trn,
			TrainingName: FormatAlertTraining(trn),
			Group:        grp,
			GroupName:    FormatAlertGroup(grp),
			Code:         cod,
			CodeName:     FormatAlertCode(cod),
			Description:  desc,
			Time:         tm,
		})
	}
	return alerts, nil
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline") || strings.Contains(errStr, "refused")
}

// querySNMPStandardFallback executes direct GET requests on standard OIDs if Walk is unsupported.
func querySNMPStandardFallback(session DynamicSNMPSession) (*SNMPProbeResult, error) {
	oids := []string{
		oidSysDescr,                    // 1.3.6.1.2.1.1.1.0
		oidHrPrinterStatus,             // 1.3.6.1.2.1.25.3.5.1.1.1
		oidHrPrinterDetectedErrorState, // 1.3.6.1.2.1.25.3.5.1.2.1
		oidPrtMarkerLifeCount,          // 1.3.6.1.2.1.43.10.2.1.4.1.1
		"1.3.6.1.2.1.43.11.1.1.9.1.1",  // fallback prtMarkerSuppliesLevel.1.1
		"1.3.6.1.2.1.43.11.1.1.8.1.1",  // fallback prtMarkerSuppliesMaxCapacity.1.1
		"1.3.6.1.2.1.43.11.1.1.9.1.2",  // fallback prtMarkerSuppliesLevel.1.2
		"1.3.6.1.2.1.43.11.1.1.8.1.2",  // fallback prtMarkerSuppliesMaxCapacity.1.2
	}

	packet, err := session.Get(oids)
	if err != nil || packet == nil {
		return nil, err
	}

	res := &SNMPProbeResult{
		DiscoveryMode: "SNMP_GET_FALLBACK",
	}

	// 1. sysDescr
	if pdu, ok := findPDU(packet, oidSysDescr, -1); ok && isValidPDU(pdu) {
		if s, ok := pduToString(pdu); ok {
			res.SysDescr = s
		}
	}

	// 2. hrPrinterStatus & error
	var hrStatus int
	var hrErrBytes []byte
	if pdu, ok := findPDU(packet, oidHrPrinterStatus, -1); ok && isValidPDU(pdu) {
		if v, ok := pduToInt64(pdu); ok {
			hrStatus = int(v)
		}
	}
	if pdu, ok := findPDU(packet, oidHrPrinterDetectedErrorState, -1); ok && isValidPDU(pdu) {
		if b, ok := pduToBytes(pdu); ok {
			hrErrBytes = b
		}
	}
	if hrStatus > 0 || len(hrErrBytes) > 0 {
		res.Printers = append(res.Printers, SNMPPrinterDevice{
			Index:      "1",
			Status:     hrStatus,
			StatusDesc: FormatHRPrinterStatus(int64(hrStatus)),
			ErrorRaw:   hrErrBytes,
			ErrorFlags: DecodeErrorBitmask(hrErrBytes),
		})
	}

	// 3. Marker life count & counter unit
	// Query counter unit separately to avoid breaking the batch on agents lacking unit OID
	var markerUnit int64 = -1
	if unitPacket, uErr := session.Get([]string{"1.3.6.1.2.1.43.10.2.1.3.1.1"}); uErr == nil && unitPacket != nil {
		if pdu, ok := findPDU(unitPacket, "1.3.6.1.2.1.43.10.2.1.3.1.1", -1); ok && isValidPDU(pdu) {
			if v, ok := pduToInt64(pdu); ok {
				markerUnit = v
			}
		}
	}

	if pdu, ok := findPDU(packet, oidPrtMarkerLifeCount, -1); ok && isValidPDU(pdu) {
		if v, ok := pduToInt64(pdu); ok {
			unitName, isPage := FormatMarkerCounterUnit(markerUnit)
			res.Markers = append(res.Markers, SNMPMarker{
				Index:           "1.1",
				CounterUnit:     markerUnit,
				CounterUnitName: unitName,
				IsPageCounter:   isPage,
				HasLifeCount:    true,
				LifeCount:       v,
			})
		}
	}

	// 4. Supplies (Toner & Drum standard fallbacks)
	// Query metadata for standard supply rows without fabricating identities or units
	var s1Desc, s2Desc string
	var s1Type, s2Type int64 = 2, 2 // RFC 3805: 2 = unknown
	var s1Class, s2Class int64 = -1, -1
	var s1Unit, s2Unit int64 = -1, -1

	suppliesMetaOIDs := []string{
		"1.3.6.1.2.1.43.11.1.1.4.1.1", // class 1.1
		"1.3.6.1.2.1.43.11.1.1.5.1.1", // type 1.1
		"1.3.6.1.2.1.43.11.1.1.6.1.1", // desc 1.1
		"1.3.6.1.2.1.43.11.1.1.7.1.1", // unit 1.1
		"1.3.6.1.2.1.43.11.1.1.4.1.2", // class 1.2
		"1.3.6.1.2.1.43.11.1.1.5.1.2", // type 1.2
		"1.3.6.1.2.1.43.11.1.1.6.1.2", // desc 1.2
		"1.3.6.1.2.1.43.11.1.1.7.1.2", // unit 1.2
	}
	if metaPacket, mErr := session.Get(suppliesMetaOIDs); mErr == nil && metaPacket != nil {
		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.6.1.1", -1); ok && isValidPDU(pdu) {
			if s, ok := pduToString(pdu); ok {
				s1Desc = s
			}
		}
		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.5.1.1", -1); ok && isValidPDU(pdu) {
			if v, ok := pduToInt64(pdu); ok {
				s1Type = v
			}
		}
		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.4.1.1", -1); ok && isValidPDU(pdu) {
			if v, ok := pduToInt64(pdu); ok {
				s1Class = v
			}
		}
		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.7.1.1", -1); ok && isValidPDU(pdu) {
			if v, ok := pduToInt64(pdu); ok {
				s1Unit = v
			}
		}

		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.6.1.2", -1); ok && isValidPDU(pdu) {
			if s, ok := pduToString(pdu); ok {
				s2Desc = s
			}
		}
		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.5.1.2", -1); ok && isValidPDU(pdu) {
			if v, ok := pduToInt64(pdu); ok {
				s2Type = v
			}
		}
		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.4.1.2", -1); ok && isValidPDU(pdu) {
			if v, ok := pduToInt64(pdu); ok {
				s2Class = v
			}
		}
		if pdu, ok := findPDU(metaPacket, "1.3.6.1.2.1.43.11.1.1.7.1.2", -1); ok && isValidPDU(pdu) {
			if v, ok := pduToInt64(pdu); ok {
				s2Unit = v
			}
		}
	}

	if s1Desc == "" {
		s1Desc = "UNKNOWN (unqueried)"
	}
	if s2Desc == "" {
		s2Desc = "UNKNOWN (unqueried)"
	}

	var tonerLevel, tonerMax int64 = -2, -2
	if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.11.1.1.9.1.1", -1); ok && isValidPDU(pdu) {
		if v, ok := pduToInt64(pdu); ok {
			tonerLevel = v
		}
	}
	if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.11.1.1.8.1.1", -1); ok && isValidPDU(pdu) {
		if v, ok := pduToInt64(pdu); ok {
			tonerMax = v
		}
	}
	if tonerLevel != -2 || tonerMax != -2 {
		lvlDesc, pct := calculateSupplyLevel(tonerLevel, tonerMax, s1Unit)
		res.Supplies = append(res.Supplies, SNMPSupply{
			Index:       "1.1",
			Description: s1Desc,
			Type:        s1Type,
			TypeName:    FormatSupplyType(s1Type),
			Class:       s1Class,
			ClassName:   FormatSupplyClass(s1Class),
			Unit:        s1Unit,
			UnitName:    FormatSupplyUnit(s1Unit),
			MaxCapacity: tonerMax,
			Level:       tonerLevel,
			LevelDesc:   lvlDesc,
			Percent:     pct,
		})
	}

	var drumLevel, drumMax int64 = -2, -2
	if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.11.1.1.9.1.2", -1); ok && isValidPDU(pdu) {
		if v, ok := pduToInt64(pdu); ok {
			drumLevel = v
		}
	}
	if pdu, ok := findPDU(packet, "1.3.6.1.2.1.43.11.1.1.8.1.2", -1); ok && isValidPDU(pdu) {
		if v, ok := pduToInt64(pdu); ok {
			drumMax = v
		}
	}
	if drumLevel != -2 || drumMax != -2 {
		lvlDesc, pct := calculateSupplyLevel(drumLevel, drumMax, s2Unit)
		res.Supplies = append(res.Supplies, SNMPSupply{
			Index:       "1.2",
			Description: s2Desc,
			Type:        s2Type,
			TypeName:    FormatSupplyType(s2Type),
			Class:       s2Class,
			ClassName:   FormatSupplyClass(s2Class),
			Unit:        s2Unit,
			UnitName:    FormatSupplyUnit(s2Unit),
			MaxCapacity: drumMax,
			Level:       drumLevel,
			LevelDesc:   lvlDesc,
			Percent:     pct,
		})
	}

	return res, nil
}

// parseTableOID splits a table PDU OID into its column number and row index.
// Example: root = "1.3.6.1.2.1.43.11.1.1", oid = "1.3.6.1.2.1.43.11.1.1.6.1.2"
// Returns: col = 6, index = "1.2", ok = true.
func parseTableOID(oid string, root string) (col int, index string, ok bool) {
	normOID := normalizeOID(oid)
	normRoot := normalizeOID(root)

	if !strings.HasPrefix(normOID, normRoot+".") {
		return 0, "", false
	}

	remainder := strings.TrimPrefix(normOID, normRoot+".")
	parts := strings.Split(remainder, ".")
	if len(parts) < 2 {
		return 0, "", false
	}

	colNum, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", false
	}

	indexStr := strings.Join(parts[1:], ".")
	return colNum, indexStr, true
}

func calculateSupplyLevel(lvl int64, maxVal int64, unit int64) (string, *float64) {
	switch lvl {
	case -1:
		return "other", nil
	case -2:
		return "unknown", nil
	case -3:
		return "atLeastOneEmpty", nil
	}

	if lvl < 0 {
		return fmt.Sprintf("invalid(%d)", lvl), nil
	}

	// RFC 3805: If unit is percent (19), level is directly 0..100
	if unit == 19 {
		pct := float64(lvl)
		return fmt.Sprintf("%.0f%%", pct), &pct
	}

	// If unit is unqueried/unknown (-1 or <= 0), NEVER fabricate a percentage or percentage unit!
	if unit <= 0 {
		if maxVal > 0 {
			return fmt.Sprintf("%d/%d (raw, unit unverified)", lvl, maxVal), nil
		}
		return fmt.Sprintf("%d (raw, unit unverified)", lvl), nil
	}

	// If max is known and positive and unit is verified
	if maxVal > 0 {
		pct := (float64(lvl) / float64(maxVal)) * 100.0
		if pct > 100.0 {
			pct = 100.0
		}
		return fmt.Sprintf("%.0f%% (%d/%d)", pct, lvl, maxVal), &pct
	}

	// Max not positive
	return fmt.Sprintf("%d (raw)", lvl), nil
}

// RFC 3805 / RFC 2790 formatting decoders

// SNMPDiscoveryPlan stores dynamically discovered SNMP indexes and supply metadata
// so recurring health acquisition executes bounded targeted GETs rather than full MIB walks.
type SNMPDiscoveryPlan struct {
	TargetHost       string                    `json:"target_host"`
	TargetPort       int                       `json:"target_port"`
	DiscoveryMode    string                    `json:"discovery_mode"`
	PrinterIndex     string                    `json:"printer_index,omitempty"`
	MarkerIndex      string                    `json:"marker_index,omitempty"`
	TonerSupplyIndex string                    `json:"toner_supply_index,omitempty"`
	DrumSupplyIndex  string                    `json:"drum_supply_index,omitempty"`
	SupplyMeta       map[string]SNMPSupplyMeta `json:"supply_meta,omitempty"`
	InputTrayIndexes []string                  `json:"input_tray_indexes,omitempty"`
	DiscoveredAt     time.Time                 `json:"discovered_at"`
}

// SNMPSupplyMeta captures immutable supply metadata discovered during startup table walk.
type SNMPSupplyMeta struct {
	Description string `json:"description,omitempty"`
	Type        int64  `json:"type"`
	Class       int64  `json:"class"`
	Unit        int64  `json:"unit"`
}

// BuildSNMPDiscoveryPlan derives an SNMPDiscoveryPlan from a dynamic SNMP probe result.
// Fixed indexes such as .1.1 or .1.2 are never assumed when table indexes are discovered.
func BuildSNMPDiscoveryPlan(res *SNMPProbeResult) *SNMPDiscoveryPlan {
	if res == nil {
		return nil
	}
	plan := &SNMPDiscoveryPlan{
		TargetHost:    res.TargetHost,
		TargetPort:    res.TargetPort,
		DiscoveryMode: res.DiscoveryMode,
		SupplyMeta:    make(map[string]SNMPSupplyMeta),
		DiscoveredAt:  time.Now().UTC(),
	}

	if len(res.Printers) > 0 {
		plan.PrinterIndex = res.Printers[0].Index
	}
	for _, m := range res.Markers {
		if m.IsPageCounter && m.HasLifeCount {
			plan.MarkerIndex = m.Index
			break
		}
	}
	if plan.MarkerIndex == "" && len(res.Markers) > 0 {
		plan.MarkerIndex = res.Markers[0].Index
	}

	for _, s := range res.Supplies {
		plan.SupplyMeta[s.Index] = SNMPSupplyMeta{
			Description: s.Description,
			Type:        s.Type,
			Class:       s.Class,
			Unit:        s.Unit,
		}
		if plan.TonerSupplyIndex == "" && isTonerOrInkSupply(s) {
			plan.TonerSupplyIndex = s.Index
		} else if plan.DrumSupplyIndex == "" && isDrumOrOPCSupply(s) {
			plan.DrumSupplyIndex = s.Index
		}
	}

	for i, tray := range res.Inputs {
		if i >= 4 {
			break
		}
		plan.InputTrayIndexes = append(plan.InputTrayIndexes, tray.Index)
	}

	return plan
}

func isTonerOrInkSupply(s SNMPSupply) bool {
	// RFC 3805 PrtMarkerSuppliesTypeTC:
	// 3=toner, 5=ink, 6=inkCartridge, 21=tonerCartridge, 22=inkRibbon
	// Explicitly exclude 4=wasteToner, 7=inkRibbon?, 8=wasteInk, 9=opc
	switch s.Type {
	case 3, 5, 6, 21, 22:
		return true
	case 4, 8, 9, 10, 15, 20:
		return false
	}
	desc := strings.ToLower(s.Description)
	if strings.Contains(desc, "waste") || strings.Contains(desc, "drum") || strings.Contains(desc, "opc") || strings.Contains(desc, "imaging") {
		return false
	}
	return strings.Contains(desc, "toner") || strings.Contains(desc, "ink") || strings.Contains(desc, "cartridge")
}

func isDrumOrOPCSupply(s SNMPSupply) bool {
	// RFC 3805 PrtMarkerSuppliesTypeTC: 9=opc (organic photoconductor / drum), 20=fuser/drum unit in some devices
	if s.Type == 9 {
		return true
	}
	desc := strings.ToLower(s.Description)
	return strings.Contains(desc, "drum") || strings.Contains(desc, "opc") || strings.Contains(desc, "photoconductor") || strings.Contains(desc, "imaging")
}

// FormatHRDeviceStatus decodes RFC 2790 hrDeviceStatus.
func FormatHRDeviceStatus(status int64) string {
	switch status {
	case 1:
		return "unknown"
	case 2:
		return "running"
	case 3:
		return "warning"
	case 4:
		return "testing"
	case 5:
		return "down"
	default:
		return fmt.Sprintf("unknown(%d)", status)
	}
}

func FormatHRPrinterStatus(status int64) string {
	switch status {
	case 1:
		return "other"
	case 2:
		return "unknown"
	case 3:
		return "idle"
	case 4:
		return "printing"
	case 5:
		return "warmup"
	default:
		return fmt.Sprintf("unknown(%d)", status)
	}
}

// FormatMarkerCounterUnit decodes RFC 3805 PrtMarkerCounterUnitTC.
// Returns unit name and whether this is a recognized page counter unit (sheets or impressions).
func FormatMarkerCounterUnit(unit int64) (name string, isValidPageUnit bool) {
	switch unit {
	case 3:
		return "tenThousandthsOfInches", false
	case 4:
		return "micrometers", false
	case 5:
		return "characters", false
	case 6:
		return "lines", false
	case 7:
		return "impressions", true
	case 8:
		return "sheets", true
	case 9:
		return "dotRow", false
	case 11:
		return "hours", false
	case 16:
		return "feet", false
	case 17:
		return "meters", false
	default:
		return fmt.Sprintf("unknown(%d)", unit), false
	}
}

func FormatSupplyClass(class int64) string {
	switch class {
	case 1:
		return "other"
	case 3:
		return "supplyThatIsConsumed"
	case 4:
		return "receptacleThatIsFilled"
	default:
		return "unknown"
	}
}

func FormatSupplyType(typ int64) string {
	switch typ {
	case 1:
		return "other"
	case 2:
		return "unknown"
	case 3:
		return "toner"
	case 4:
		return "wasteToner"
	case 5:
		return "ink"
	case 6:
		return "inkCartridge"
	case 7:
		return "inkRibbon"
	case 8:
		return "wasteInk"
	case 9:
		return "opc"
	case 10:
		return "developer"
	case 11:
		return "fuserOil"
	case 12:
		return "solidWax"
	case 13:
		return "ribbonWax"
	case 14:
		return "wasteWax"
	case 15:
		return "fuser"
	case 16:
		return "cleaningCartridge"
	case 17:
		return "fuserCleaningPack"
	case 18:
		return "transferUnit"
	case 19:
		return "tonerCartridge"
	case 20:
		return "fuserOiler"
	case 21:
		return "water"
	case 22:
		return "wasteWater"
	case 23:
		return "glueCold"
	case 24:
		return "glueHot"
	case 25:
		return "wire"
	case 26:
		return "staples"
	case 27:
		return "surplusToner"
	case 28:
		return "developerCartridge"
	case 29:
		return "cleanerUnit"
	case 30:
		return "fuserCleaningPad"
	case 31:
		return "wasteTonerBag"
	default:
		return fmt.Sprintf("type(%d)", typ)
	}
}

func FormatSupplyUnit(unit int64) string {
	switch unit {
	case 3:
		return "tenThousandthsOfInches"
	case 4:
		return "micrometers"
	case 7:
		return "impressions"
	case 8:
		return "sheets"
	case 11:
		return "hours"
	case 12:
		return "thousandthsOfOunces"
	case 13:
		return "tenthsOfGrams"
	case 14:
		return "hundredthsOfFluidOunces"
	case 15:
		return "tenthsOfMilliliters"
	case 16:
		return "feet"
	case 17:
		return "meters"
	case 18:
		return "items"
	case 19:
		return "percent"
	default:
		if unit <= 0 {
			return "unknown"
		}
		return fmt.Sprintf("unit(%d)", unit)
	}
}

func FormatCoverStatus(status int64) string {
	switch status {
	case 1:
		return "other"
	case 2:
		return "unknown"
	case 3:
		return "coverOpen"
	case 4:
		return "coverClosed"
	case 5:
		return "interlockOpen"
	case 6:
		return "interlockClosed"
	default:
		return fmt.Sprintf("status(%d)", status)
	}
}

func FormatAlertSeverity(sev int64) string {
	switch sev {
	case 3:
		return "critical"
	case 4:
		return "warning"
	case 5:
		return "warningBinaryChangeEvent"
	default:
		return fmt.Sprintf("severity(%d)", sev)
	}
}

func FormatAlertTraining(trn int64) string {
	switch trn {
	case 1:
		return "other"
	case 2:
		return "unknown"
	case 3:
		return "untrained"
	case 4:
		return "trained"
	case 5:
		return "fieldService"
	case 6:
		return "management"
	default:
		return fmt.Sprintf("training(%d)", trn)
	}
}

func FormatAlertGroup(grp int64) string {
	switch grp {
	case 1:
		return "other"
	case 2:
		return "unknown"
	case 3:
		return "generalPrinter"
	case 4:
		return "cover"
	case 5:
		return "localization"
	case 6:
		return "input"
	case 7:
		return "output"
	case 8:
		return "marker"
	case 9:
		return "markerSupplies"
	case 10:
		return "markerColorant"
	case 11:
		return "mediaPath"
	case 12:
		return "channel"
	case 13:
		return "interpreter"
	case 14:
		return "console"
	default:
		return fmt.Sprintf("group(%d)", grp)
	}
}

func FormatAlertCode(code int64) string {
	switch code {
	case 1:
		return "other"
	case 2:
		return "unknown"
	case 3:
		return "coverOpen"
	case 4:
		return "coverClosed"
	case 5:
		return "interlockOpen"
	case 6:
		return "interlockClosed"
	case 8:
		return "jam"
	case 9:
		return "doorOpen"
	case 11:
		return "inputMediaTrayMissing"
	case 12:
		return "inputMediaSizeChange"
	case 13:
		return "inputMediaWeightChange"
	case 14:
		return "inputMediaTypeChange"
	case 15:
		return "inputMediaColorChange"
	case 16:
		return "inputMediaFormPartsChange"
	case 17:
		return "inputMediaSupplyLow"
	case 18:
		return "inputMediaSupplyEmpty"
	case 20:
		return "outputMediaTrayMissing"
	case 21:
		return "outputMediaTrayAlmostFull"
	case 22:
		return "outputMediaTrayFull"
	case 23:
		return "markerFuserUnderTemperature"
	case 24:
		return "markerFuserOverTemperature"
	case 25:
		return "markerTonerEmpty"
	case 26:
		return "markerTonerAlmostEmpty"
	case 27:
		return "markerWasteTonerReceptacleAlmostFull"
	case 28:
		return "markerWasteTonerReceptacleFull"
	case 30:
		return "markerDeveloperEmpty"
	case 31:
		return "markerDeveloperAlmostEmpty"
	default:
		return fmt.Sprintf("code(%d)", code)
	}
}

func pduToString(pdu gosnmp.SnmpPDU) (string, bool) {
	switch pdu.Type {
	case gosnmp.OctetString:
		if b, ok := pdu.Value.([]byte); ok {
			return string(b), true
		}
		if s, ok := pdu.Value.(string); ok {
			return s, true
		}
	}
	return "", false
}

func pduToBytes(pdu gosnmp.SnmpPDU) ([]byte, bool) {
	switch pdu.Type {
	case gosnmp.OctetString:
		if b, ok := pdu.Value.([]byte); ok {
			return b, true
		}
		if s, ok := pdu.Value.(string); ok {
			return []byte(s), true
		}
	}
	return nil, false
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
