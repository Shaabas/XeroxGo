package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Supported MIB OIDs matching XeroxGo queries
const (
	oidSysDescr                    = "1.3.6.1.2.1.1.1.0"
	oidSysObjectID                 = "1.3.6.1.2.1.1.2.0"
	oidSysUpTime                   = "1.3.6.1.2.1.1.3.0"
	oidHrPrinterStatus             = "1.3.6.1.2.1.25.3.5.1.1.1"
	oidHrPrinterDetectedErrorState = "1.3.6.1.2.1.25.3.5.1.2.1"
	oidPrtMarkerLifeCount          = "1.3.6.1.2.1.43.10.2.1.4.1.1"
	oidPrtMarkerSuppliesLevel1     = "1.3.6.1.2.1.43.11.1.1.9.1.1"
	oidPrtMarkerSuppliesMax1       = "1.3.6.1.2.1.43.11.1.1.8.1.1"
	oidPrtMarkerSuppliesLevel2     = "1.3.6.1.2.1.43.11.1.1.9.1.2"
	oidPrtMarkerSuppliesMax2       = "1.3.6.1.2.1.43.11.1.1.8.1.2"
)

// Standard MIB Table Subtree Roots (RFC 1213, RFC 2790, RFC 3805)
const (
	oidSubtreeHRPrinter   = "1.3.6.1.2.1.25.3.5.1"
	oidSubtreePrtGeneral  = "1.3.6.1.2.1.43.5.1.1"
	oidSubtreePrtCover    = "1.3.6.1.2.1.43.6.1.1"
	oidSubtreePrtMarker   = "1.3.6.1.2.1.43.10.2.1"
	oidSubtreePrtSupplies = "1.3.6.1.2.1.43.11.1.1"
	oidSubtreePrtAlert    = "1.3.6.1.2.1.43.18.1.1"
)

// RFC 2790 hrPrinterDetectedErrorState bitmasks
// Byte 0 (bits 0..7)
const (
	FlagLowPaper         byte = 0x80
	FlagNoPaper          byte = 0x40
	FlagLowToner         byte = 0x20
	FlagNoToner          byte = 0x10
	FlagDoorOpen         byte = 0x08
	FlagJammed           byte = 0x04
	FlagOffline          byte = 0x02
	FlagServiceRequested byte = 0x01
)

// Byte 1 (bits 8..15)
const (
	FlagInputTrayMissing    byte = 0x80
	FlagOutputTrayMissing   byte = 0x40
	FlagMarkerSupplyMissing byte = 0x20
	FlagOutputNearFull      byte = 0x10
	FlagOutputFull          byte = 0x08
	FlagInputTrayEmpty      byte = 0x04
	FlagOverduePreventMaint byte = 0x02
)

// RFC 3805 PrtMarkerCounterUnitTC
const (
	MarkerUnitTenThousandthsOfInches = 3
	MarkerUnitMicrometers            = 4
	MarkerUnitCharacters             = 5
	MarkerUnitLines                  = 6
	MarkerUnitImpressions            = 7
	MarkerUnitSheets                 = 8
	MarkerUnitDotRow                 = 9
	MarkerUnitHours                  = 11
	MarkerUnitFeet                   = 16
)

// MarkerState represents an RFC 3805 prtMarkerTable row entry.
type MarkerState struct {
	Index         string `json:"index"` // hrDeviceIndex.markerIndex, e.g. "1.1" or "2.3"
	CounterUnit   int    `json:"counter_unit"`
	LifeCount     uint32 `json:"life_count"`
	PowerOnCount  uint32 `json:"power_on_count,omitempty"`
	OmitLifeCount bool   `json:"omit_life_count,omitempty"`
}

// SupplyState represents an RFC 3805 prtMarkerSuppliesTable row entry.
type SupplyState struct {
	Index       string `json:"index"` // hrDeviceIndex.supplyIndex, e.g. "1.1" or "2.1"
	Description string `json:"description"`
	Type        int    `json:"type"`        // 3 = toner, 15 = opc/drum
	Class       int    `json:"class"`       // 3 = supplyThatIsConsumed, 4 = receptacleThatIsFilled
	Unit        int    `json:"unit"`        // 15 = percent, 8 = sheets, 19 = items
	MaxCapacity int    `json:"max_capacity"`
	Level       int    `json:"level"`
}

// CoverState represents an RFC 3805 prtCoverTable row entry.
type CoverState struct {
	Index       string `json:"index"` // hrDeviceIndex.coverIndex, e.g. "1.1" or "2.1"
	Description string `json:"description"`
	Status      int    `json:"status"` // 3 = coverOpen, 4 = coverClosed, 5 = interlockOpen, 6 = interlockClosed
}

// AlertState represents an RFC 3805 prtAlertTable row entry.
type AlertState struct {
	Index       string `json:"index"` // hrDeviceIndex.alertIndex, e.g. "1.1"
	Severity    int    `json:"severity"`
	Training    int    `json:"training"`
	Group       int    `json:"group"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	Time        uint32 `json:"time"`
}

// SNMPState encapsulates the simulated printer telemetry state.
type SNMPState struct {
	SysDescr        string        `json:"sys_descr"`
	SysObjectID     string        `json:"sys_object_id,omitempty"`
	SysUpTime       uint32        `json:"sys_uptime,omitempty"`
	PrinterName     string        `json:"printer_name,omitempty"`
	PrinterIndex    string        `json:"printer_index,omitempty"`
	HrPrinterStatus int           `json:"hr_printer_status"`
	ErrorByte0      byte          `json:"error_byte_0"`
	ErrorByte1      byte          `json:"error_byte_1"`
	MarkerLifeCount uint32        `json:"marker_life_count"`
	TonerLevel      int           `json:"toner_level"`
	TonerMax        int           `json:"toner_max"`
	DrumLevel       int           `json:"drum_level"`
	DrumMax         int           `json:"drum_max"`
	Markers         []MarkerState `json:"markers,omitempty"`
	Supplies        []SupplyState `json:"supplies,omitempty"`
	SuppliesCleared bool          `json:"supplies_cleared,omitempty"`
	Covers          []CoverState  `json:"covers,omitempty"`
	Alerts          []AlertState  `json:"alerts,omitempty"`
	OmittedOIDs     []string      `json:"omitted_oids,omitempty"`
	Offline         bool          `json:"offline,omitempty"`
}

// DefaultState returns healthy/idle default state.
func DefaultState() SNMPState {
	return SNMPState{
		SysDescr:        "Xerox WorkCentre 3025 (Local Emulator)",
		SysObjectID:     "1.3.6.1.4.1.2699.1",
		SysUpTime:       360000, // 3600 seconds in 10ms ticks
		PrinterName:     "Xerox WorkCentre 3025 (Local Emulator)",
		PrinterIndex:    "1",
		HrPrinterStatus: 3, // idle
		ErrorByte0:      0x00,
		ErrorByte1:      0x00,
		MarkerLifeCount: 1000,
		TonerLevel:      85,
		TonerMax:        100,
		DrumLevel:       95,
		DrumMax:         100,
		Markers: []MarkerState{
			{
				Index:       "1.1",
				CounterUnit: MarkerUnitSheets, // sheets(8)
				LifeCount:   1000,
			},
		},
		Supplies: []SupplyState{
			{
				Index:       "1.1",
				Description: "Black Toner Cartridge (Synthetic Test Fixture)",
				Type:        3,  // toner
				Class:       3,  // supplyThatIsConsumed
				Unit:        15, // percent
				MaxCapacity: 100,
				Level:       85,
			},
			{
				Index:       "1.2",
				Description: "OPC Drum Unit (Synthetic Test Fixture)",
				Type:        15, // opc / drum
				Class:       3,  // supplyThatIsConsumed
				Unit:        15, // percent
				MaxCapacity: 100,
				Level:       95,
			},
		},
		Covers: []CoverState{
			{
				Index:       "1.1",
				Description: "Front Cover",
				Status:      4, // coverClosed
			},
		},
		Alerts:          nil,
		OmittedOIDs:     nil,
		Offline:         false,
		SuppliesCleared: false,
	}
}

func normalizeOID(oid string) string {
	return strings.TrimPrefix(oid, ".")
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	if strings.HasPrefix(h, "127.") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// parseOIDParts parses an OID string into a slice of unsigned 32-bit integers.
func parseOIDParts(oid string) ([]uint32, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(oid), ".")
	if clean == "" {
		return nil, nil
	}
	parts := strings.Split(clean, ".")
	res := make([]uint32, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, err
		}
		res[i] = uint32(v)
	}
	return res, nil
}

// compareOIDs performs standard RFC 1157 / RFC 1905 lexicographical numeric OID comparison.
// Returns -1 if oidA < oidB, 1 if oidA > oidB, 0 if oidA == oidB.
func compareOIDs(oidA, oidB string) int {
	partsA, errA := parseOIDParts(oidA)
	partsB, errB := parseOIDParts(oidB)
	if errA != nil || errB != nil {
		return strings.Compare(normalizeOID(oidA), normalizeOID(oidB))
	}
	minLen := len(partsA)
	if len(partsB) < minLen {
		minLen = len(partsB)
	}
	for i := 0; i < minLen; i++ {
		if partsA[i] < partsB[i] {
			return -1
		}
		if partsA[i] > partsB[i] {
			return 1
		}
	}
	if len(partsA) < len(partsB) {
		return -1
	}
	if len(partsA) > len(partsB) {
		return 1
	}
	return 0
}

// Sync ensures synchronization between legacy flat fields and structured table slices.
func (s *SNMPState) Sync() {
	if s.PrinterIndex == "" {
		s.PrinterIndex = "1"
	}
	if s.SysObjectID == "" {
		s.SysObjectID = "1.3.6.1.4.1.2699.1"
	}
	if s.SysUpTime == 0 {
		s.SysUpTime = 360000
	}
	if s.PrinterName == "" {
		s.PrinterName = s.SysDescr
	}

	// Markers synchronization
	if len(s.Markers) == 0 {
		mIdx := s.PrinterIndex + ".1"
		s.Markers = []MarkerState{
			{
				Index:       mIdx,
				CounterUnit: MarkerUnitSheets,
				LifeCount:   s.MarkerLifeCount,
			},
		}
	} else {
		// If legacy MarkerLifeCount was changed, sync to primary marker
		if s.MarkerLifeCount != s.Markers[0].LifeCount {
			s.Markers[0].LifeCount = s.MarkerLifeCount
		}
	}

	// Supplies synchronization
	if len(s.Supplies) == 0 && !s.SuppliesCleared {
		tIdx := s.PrinterIndex + ".1"
		dIdx := s.PrinterIndex + ".2"
		s.Supplies = []SupplyState{
			{
				Index:       tIdx,
				Description: "Black Toner Cartridge (Synthetic Test Fixture)",
				Type:        3,  // toner
				Class:       3,  // consumed
				Unit:        15, // percent
				MaxCapacity: s.TonerMax,
				Level:       s.TonerLevel,
			},
			{
				Index:       dIdx,
				Description: "OPC Drum Unit (Synthetic Test Fixture)",
				Type:        15, // drum
				Class:       3,  // consumed
				Unit:        15, // percent
				MaxCapacity: s.DrumMax,
				Level:       s.DrumLevel,
			},
		}
	} else if len(s.Supplies) > 0 {
		for i := range s.Supplies {
			if s.Supplies[i].Type == 3 {
				if s.TonerMax > 0 && s.TonerLevel >= -2 {
					s.Supplies[i].MaxCapacity = s.TonerMax
					s.Supplies[i].Level = s.TonerLevel
				}
			} else if s.Supplies[i].Type == 15 {
				if s.DrumMax > 0 && s.DrumLevel >= -2 {
					s.Supplies[i].MaxCapacity = s.DrumMax
					s.Supplies[i].Level = s.DrumLevel
				}
			}
		}
	}

	// Covers synchronization with error flags
	if len(s.Covers) == 0 {
		cIdx := s.PrinterIndex + ".1"
		cStatus := 4 // coverClosed
		if s.ErrorByte0&FlagDoorOpen != 0 {
			cStatus = 3 // coverOpen
		}
		s.Covers = []CoverState{
			{
				Index:       cIdx,
				Description: "Front Access Cover",
				Status:      cStatus,
			},
		}
	} else {
		if s.ErrorByte0&FlagDoorOpen != 0 {
			s.Covers[0].Status = 3 // coverOpen
		} else if s.Covers[0].Status == 3 {
			s.Covers[0].Status = 4 // coverClosed
		}
	}
}

// buildActiveMIB compiles all scalar and RFC table variable bindings into a numerically sorted PDU slice.
func (s *SNMPState) buildActiveMIB() []gosnmp.SnmpPDU {
	s.Sync()
	var pdus []gosnmp.SnmpPDU

	// RFC 1213 / SNMPv2-MIB scalars
	pdus = append(pdus,
		gosnmp.SnmpPDU{
			Name:  "." + normalizeOID(oidSysDescr),
			Type:  gosnmp.OctetString,
			Value: []byte(s.SysDescr),
		},
		gosnmp.SnmpPDU{
			Name:  "." + normalizeOID(oidSysObjectID),
			Type:  gosnmp.ObjectIdentifier,
			Value: "." + normalizeOID(s.SysObjectID),
		},
		gosnmp.SnmpPDU{
			Name:  "." + normalizeOID(oidSysUpTime),
			Type:  gosnmp.TimeTicks,
			Value: s.SysUpTime,
		},
	)

	pIdx := s.PrinterIndex
	if pIdx == "" {
		pIdx = "1"
	}

	// RFC 2790 hrPrinterTable: 1.3.6.1.2.1.25.3.5.1
	// Col 1: hrPrinterStatus
	pdus = append(pdus, gosnmp.SnmpPDU{
		Name:  fmt.Sprintf(".1.3.6.1.2.1.25.3.5.1.1.%s", pIdx),
		Type:  gosnmp.Integer,
		Value: s.HrPrinterStatus,
	})
	// Col 2: hrPrinterDetectedErrorState
	pdus = append(pdus, gosnmp.SnmpPDU{
		Name:  fmt.Sprintf(".1.3.6.1.2.1.25.3.5.1.2.%s", pIdx),
		Type:  gosnmp.OctetString,
		Value: []byte{s.ErrorByte0, s.ErrorByte1},
	})

	// RFC 3805 prtGeneralTable: 1.3.6.1.2.1.43.5.1.1
	// Col 16: prtGeneralPrinterName
	prtName := s.PrinterName
	if prtName == "" {
		prtName = s.SysDescr
	}
	pdus = append(pdus, gosnmp.SnmpPDU{
		Name:  fmt.Sprintf(".1.3.6.1.2.1.43.5.1.1.16.%s", pIdx),
		Type:  gosnmp.OctetString,
		Value: []byte(prtName),
	})

	// RFC 3805 prtCoverTable: 1.3.6.1.2.1.43.6.1.1
	// Col 2: prtCoverDescription
	// Col 3: prtCoverStatus
	for _, cov := range s.Covers {
		pdus = append(pdus,
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.6.1.1.2.%s", cov.Index),
				Type:  gosnmp.OctetString,
				Value: []byte(cov.Description),
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.6.1.1.3.%s", cov.Index),
				Type:  gosnmp.Integer,
				Value: cov.Status,
			},
		)
	}

	// RFC 3805 prtMarkerTable: 1.3.6.1.2.1.43.10.2.1
	// Col 3: prtMarkerCounterUnit
	// Col 4: prtMarkerLifeCount
	// Col 5: prtMarkerPowerOnCount
	for _, m := range s.Markers {
		pdus = append(pdus, gosnmp.SnmpPDU{
			Name:  fmt.Sprintf(".1.3.6.1.2.1.43.10.2.1.3.%s", m.Index),
			Type:  gosnmp.Integer,
			Value: m.CounterUnit,
		})
		if !m.OmitLifeCount {
			pdus = append(pdus, gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.10.2.1.4.%s", m.Index),
				Type:  gosnmp.Counter32,
				Value: m.LifeCount,
			})
		}
		if m.PowerOnCount > 0 {
			pdus = append(pdus, gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.10.2.1.5.%s", m.Index),
				Type:  gosnmp.Counter32,
				Value: m.PowerOnCount,
			})
		}
	}

	// RFC 3805 prtMarkerSuppliesTable: 1.3.6.1.2.1.43.11.1.1
	// Col 4: prtMarkerSuppliesClass
	// Col 5: prtMarkerSuppliesType
	// Col 6: prtMarkerSuppliesDescription
	// Col 7: prtMarkerSuppliesSupplyUnit
	// Col 8: prtMarkerSuppliesMaxCapacity
	// Col 9: prtMarkerSuppliesLevel
	for _, sup := range s.Supplies {
		pdus = append(pdus,
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.11.1.1.4.%s", sup.Index),
				Type:  gosnmp.Integer,
				Value: sup.Class,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.11.1.1.5.%s", sup.Index),
				Type:  gosnmp.Integer,
				Value: sup.Type,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.11.1.1.6.%s", sup.Index),
				Type:  gosnmp.OctetString,
				Value: []byte(sup.Description),
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.11.1.1.7.%s", sup.Index),
				Type:  gosnmp.Integer,
				Value: sup.Unit,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.11.1.1.8.%s", sup.Index),
				Type:  gosnmp.Integer,
				Value: sup.MaxCapacity,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.11.1.1.9.%s", sup.Index),
				Type:  gosnmp.Integer,
				Value: sup.Level,
			},
		)
	}

	// RFC 3805 prtAlertTable: 1.3.6.1.2.1.43.18.1.1
	for _, alt := range s.Alerts {
		pdus = append(pdus,
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.18.1.1.2.%s", alt.Index),
				Type:  gosnmp.Integer,
				Value: alt.Severity,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.18.1.1.3.%s", alt.Index),
				Type:  gosnmp.Integer,
				Value: alt.Training,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.18.1.1.4.%s", alt.Index),
				Type:  gosnmp.Integer,
				Value: alt.Group,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.18.1.1.7.%s", alt.Index),
				Type:  gosnmp.Integer,
				Value: alt.Code,
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.18.1.1.8.%s", alt.Index),
				Type:  gosnmp.OctetString,
				Value: []byte(alt.Description),
			},
			gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".1.3.6.1.2.1.43.18.1.1.9.%s", alt.Index),
				Type:  gosnmp.TimeTicks,
				Value: alt.Time,
			},
		)
	}

	// Filter out OmittedOIDs
	omittedSet := make(map[string]bool)
	for _, o := range s.OmittedOIDs {
		omittedSet[normalizeOID(o)] = true
	}

	var filtered []gosnmp.SnmpPDU
	for _, p := range pdus {
		if !omittedSet[normalizeOID(p.Name)] {
			filtered = append(filtered, p)
		}
	}

	// Sort numerically by OID
	sort.Slice(filtered, func(i, j int) bool {
		return compareOIDs(filtered[i].Name, filtered[j].Name) < 0
	})

	return filtered
}

// StateStore manages state loading, saving, and thread-safe dynamic reloads.
type StateStore struct {
	mu        sync.RWMutex
	filePath  string
	state     SNMPState
	lastMtime time.Time
}

func NewStateStore(path string) (*StateStore, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	store := &StateStore{
		filePath: absPath,
		state:    DefaultState(),
	}
	if err := store.loadOrInit(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *StateStore) loadOrInit() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.filePath)
	if os.IsNotExist(err) {
		s.state = DefaultState()
		return s.saveLocked()
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	var state SNMPState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("invalid json in state file %s: %w", s.filePath, err)
	}
	state.Sync()
	s.state = state
	s.lastMtime = info.ModTime()
	return nil
}

func (s *StateStore) ReloadIfChanged() {
	info, err := os.Stat(s.filePath)
	if err != nil {
		return
	}
	if info.ModTime().Equal(s.lastMtime) {
		return
	}
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return
	}
	var state SNMPState
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	state.Sync()
	s.mu.Lock()
	s.state = state
	s.lastMtime = info.ModTime()
	s.mu.Unlock()
}

func (s *StateStore) GetState() SNMPState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *StateStore) UpdateState(fn func(*SNMPState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	s.state.Sync()
	return s.saveLocked()
}

func (s *StateStore) saveLocked() error {
	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := s.filePath + ".tmp." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpFile, s.filePath); err != nil {
		os.Remove(tmpFile)
		return err
	}
	if info, err := os.Stat(s.filePath); err == nil {
		s.lastMtime = info.ModTime()
	}
	return nil
}

// Server encapsulates the SNMPv2c UDP responder.
type Server struct {
	host      string
	port      int
	community string
	store     *StateStore
	pidFile   string
	conn      net.PacketConn
	stopCh    chan struct{}
	stopped   sync.WaitGroup
}

func NewServer(host string, port int, community string, store *StateStore, pidFile string) (*Server, error) {
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("safety violation: host '%s' is not loopback. Refusing to bind", host)
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port: %d", port)
	}
	if community == "" {
		community = "public"
	}
	return &Server{
		host:      host,
		port:      port,
		community: community,
		store:     store,
		pidFile:   pidFile,
		stopCh:    make(chan struct{}),
	}, nil
}

func (srv *Server) Start() error {
	bindAddr := fmt.Sprintf("%s:%d", srv.host, srv.port)
	conn, err := net.ListenPacket("udp", bindAddr)
	if err != nil {
		return fmt.Errorf("failed to bind UDP port %s: %w", bindAddr, err)
	}
	srv.conn = conn

	// If dynamic port was chosen, update port
	if udpAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		srv.port = udpAddr.Port
	}

	if srv.pidFile != "" {
		if err := writePID(srv.pidFile); err != nil {
			conn.Close()
			return err
		}
	}

	srv.stopped.Add(1)
	go srv.listenLoop()
	return nil
}

func (srv *Server) Port() int {
	return srv.port
}

func (srv *Server) Stop() {
	close(srv.stopCh)
	if srv.conn != nil {
		srv.conn.Close()
	}
	srv.stopped.Wait()
	if srv.pidFile != "" {
		os.Remove(srv.pidFile)
	}
}

func (srv *Server) listenLoop() {
	defer srv.stopped.Done()
	buf := make([]byte, 8192)
	g := &gosnmp.GoSNMP{Version: gosnmp.Version2c}

	for {
		select {
		case <-srv.stopCh:
			return
		default:
		}

		_ = srv.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, remoteAddr, err := srv.conn.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-srv.stopCh:
				return
			default:
				continue
			}
		}

		// Reload state file if modified externally
		srv.store.ReloadIfChanged()
		state := srv.store.GetState()

		// Offline simulation: drop incoming requests to simulate communication loss
		if state.Offline {
			continue
		}

		packet, err := g.SnmpDecodePacket(buf[:n])
		if err != nil {
			continue
		}

		// Security: verify community string
		if packet.Community != srv.community {
			continue
		}

		if packet.PDUType != gosnmp.GetRequest &&
			packet.PDUType != gosnmp.GetNextRequest &&
			packet.PDUType != gosnmp.GetBulkRequest {
			continue
		}

		activeMIB := state.buildActiveMIB()

		omittedSet := make(map[string]bool)
		for _, o := range state.OmittedOIDs {
			omittedSet[normalizeOID(o)] = true
		}

		resp := &gosnmp.SnmpPacket{
			Version:   packet.Version,
			Community: packet.Community,
			PDUType:   gosnmp.GetResponse,
			RequestID: packet.RequestID,
			Error:     gosnmp.NoError,
			Variables: make([]gosnmp.SnmpPDU, 0, len(packet.Variables)),
		}

		switch packet.PDUType {
		case gosnmp.GetRequest:
			for _, pdu := range packet.Variables {
				normReqOID := normalizeOID(pdu.Name)

				if omittedSet[normReqOID] {
					resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
						Name:  pdu.Name,
						Type:  gosnmp.NoSuchInstance,
						Value: nil,
					})
					continue
				}

				var found *gosnmp.SnmpPDU
				for i := range activeMIB {
					if normalizeOID(activeMIB[i].Name) == normReqOID {
						found = &activeMIB[i]
						break
					}
				}

				if found != nil {
					resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
						Name:  pdu.Name,
						Type:  found.Type,
						Value: found.Value,
					})
				} else {
					resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
						Name:  pdu.Name,
						Type:  gosnmp.NoSuchInstance,
						Value: nil,
					})
				}
			}

		case gosnmp.GetNextRequest:
			for _, pdu := range packet.Variables {
				normReqOID := normalizeOID(pdu.Name)
				var successor *gosnmp.SnmpPDU
				for i := range activeMIB {
					if compareOIDs(normalizeOID(activeMIB[i].Name), normReqOID) > 0 {
						successor = &activeMIB[i]
						break
					}
				}

				if successor != nil {
					resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
						Name:  successor.Name,
						Type:  successor.Type,
						Value: successor.Value,
					})
				} else {
					resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
						Name:  pdu.Name,
						Type:  gosnmp.EndOfMibView,
						Value: nil,
					})
				}
			}

		case gosnmp.GetBulkRequest:
			nonRepeaters := int(packet.NonRepeaters)
			if nonRepeaters < 0 {
				nonRepeaters = 0
			}
			maxRepetitions := int(packet.MaxRepetitions)
			if maxRepetitions < 0 {
				maxRepetitions = 0
			}
			numVars := len(packet.Variables)
			nr := nonRepeaters
			if nr > numVars {
				nr = numVars
			}

			// Handle non-repeaters (1 successor each)
			for i := 0; i < nr; i++ {
				pdu := packet.Variables[i]
				normReqOID := normalizeOID(pdu.Name)
				var successor *gosnmp.SnmpPDU
				for j := range activeMIB {
					if compareOIDs(normalizeOID(activeMIB[j].Name), normReqOID) > 0 {
						successor = &activeMIB[j]
						break
					}
				}
				if successor != nil {
					resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
						Name:  successor.Name,
						Type:  successor.Type,
						Value: successor.Value,
					})
				} else {
					resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
						Name:  pdu.Name,
						Type:  gosnmp.EndOfMibView,
						Value: nil,
					})
				}
			}

			// Handle repeaters (up to maxRepetitions successors for remaining variables)
			repeatersCount := numVars - nr
			if repeatersCount > 0 && maxRepetitions > 0 {
				lastOIDs := make([]string, repeatersCount)
				for i := 0; i < repeatersCount; i++ {
					lastOIDs[i] = normalizeOID(packet.Variables[nr+i].Name)
				}

				for r := 0; r < maxRepetitions; r++ {
					for i := 0; i < repeatersCount; i++ {
						currentOID := lastOIDs[i]
						var successor *gosnmp.SnmpPDU
						for j := range activeMIB {
							if compareOIDs(normalizeOID(activeMIB[j].Name), currentOID) > 0 {
								successor = &activeMIB[j]
								break
							}
						}
						if successor != nil {
							resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
								Name:  successor.Name,
								Type:  successor.Type,
								Value: successor.Value,
							})
							lastOIDs[i] = normalizeOID(successor.Name)
						} else {
							resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
								Name:  packet.Variables[nr+i].Name,
								Type:  gosnmp.EndOfMibView,
								Value: nil,
							})
						}
					}
				}
			}
		}

		outBytes, err := resp.MarshalMsg()
		if err != nil {
			continue
		}
		_, _ = srv.conn.WriteTo(outBytes, remoteAddr)
	}
}

func writePID(pidPath string) error {
	dir := filepath.Dir(pidPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644)
}

// Error flag name parsing
func parseErrorFlag(name string) (byteIndex int, mask byte, ok bool) {
	clean := strings.ToLower(strings.TrimSpace(name))
	clean = strings.ReplaceAll(clean, "-", "_")
	clean = strings.ReplaceAll(clean, " ", "_")

	switch clean {
	case "low_paper", "lowpaper":
		return 0, FlagLowPaper, true
	case "no_paper", "nopaper":
		return 0, FlagNoPaper, true
	case "low_toner", "lowtoner":
		return 0, FlagLowToner, true
	case "no_toner", "notoner":
		return 0, FlagNoToner, true
	case "door_open", "dooropen", "cover_open", "coveropen":
		return 0, FlagDoorOpen, true
	case "jammed", "paper_jam", "paperjam":
		return 0, FlagJammed, true
	case "offline":
		return 0, FlagOffline, true
	case "service_requested", "servicerequested":
		return 0, FlagServiceRequested, true
	case "input_tray_missing", "inputtraymissing":
		return 1, FlagInputTrayMissing, true
	case "output_tray_missing", "outputtraymissing":
		return 1, FlagOutputTrayMissing, true
	case "marker_supply_missing", "markersupplymissing":
		return 1, FlagMarkerSupplyMissing, true
	case "output_near_full", "outputnearfull":
		return 1, FlagOutputNearFull, true
	case "output_full", "outputfull", "output_bin_full":
		return 1, FlagOutputFull, true
	case "input_tray_empty", "inputtrayempty":
		return 1, FlagInputTrayEmpty, true
	case "overdue_prevent_maint", "overduepreventmaint", "maint_overdue":
		return 1, FlagOverduePreventMaint, true
	default:
		return 0, 0, false
	}
}

func parseStatusString(s string) (int, error) {
	if val, err := strconv.Atoi(s); err == nil {
		if val < 1 || val > 7 {
			return 0, fmt.Errorf("hrPrinterStatus integer must be between 1 and 7, got %d", val)
		}
		return val, nil
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "other":
		return 1, nil
	case "unknown":
		return 2, nil
	case "idle":
		return 3, nil
	case "printing":
		return 4, nil
	case "warmup", "warming_up", "warmingup":
		return 5, nil
	case "error":
		return 6, nil
	case "offline":
		return 7, nil
	default:
		return 0, fmt.Errorf("unknown status string '%s' (valid: other, unknown, idle, printing, warmup, error, offline)", s)
	}
}

func formatMarkerUnitName(unit int) string {
	switch unit {
	case MarkerUnitTenThousandthsOfInches:
		return "tenThousandthsOfInches(3)"
	case MarkerUnitMicrometers:
		return "micrometers(4)"
	case MarkerUnitCharacters:
		return "characters(5)"
	case MarkerUnitLines:
		return "lines(6)"
	case MarkerUnitImpressions:
		return "impressions(7)"
	case MarkerUnitSheets:
		return "sheets(8)"
	case MarkerUnitDotRow:
		return "dotRow(9)"
	case MarkerUnitHours:
		return "hours(11)"
	case MarkerUnitFeet:
		return "feet(16)"
	default:
		return fmt.Sprintf("custom(%d)", unit)
	}
}

func printState(state SNMPState, w io.Writer) {
	fmt.Fprintf(w, "SNMP Emulator State (Synthetic Test Fixture - Not Real Hardware Measurement):\n")
	fmt.Fprintf(w, "  sysDescr:              %s\n", state.SysDescr)
	fmt.Fprintf(w, "  sysObjectID:           %s\n", state.SysObjectID)
	fmt.Fprintf(w, "  sysUpTime:             %d (10ms ticks)\n", state.SysUpTime)
	fmt.Fprintf(w, "  prtGeneralPrinterName: %s\n", state.PrinterName)
	fmt.Fprintf(w, "  printerIndex:          %s\n", state.PrinterIndex)

	statusStr := "UNKNOWN"
	switch state.HrPrinterStatus {
	case 1:
		statusStr = "OTHER (1)"
	case 2:
		statusStr = "UNKNOWN (2)"
	case 3:
		statusStr = "IDLE (3)"
	case 4:
		statusStr = "PRINTING (4)"
	case 5:
		statusStr = "WARMING_UP (5)"
	case 6:
		statusStr = "ERROR (6)"
	case 7:
		statusStr = "OFFLINE (7)"
	default:
		statusStr = fmt.Sprintf("CUSTOM (%d)", state.HrPrinterStatus)
	}
	fmt.Fprintf(w, "  hrPrinterStatus:       %s\n", statusStr)
	fmt.Fprintf(w, "  hrPrinterErrorState:   [0x%02X, 0x%02X]\n", state.ErrorByte0, state.ErrorByte1)

	var activeFlags []string
	if state.ErrorByte0&FlagLowPaper != 0 {
		activeFlags = append(activeFlags, "lowPaper")
	}
	if state.ErrorByte0&FlagNoPaper != 0 {
		activeFlags = append(activeFlags, "noPaper")
	}
	if state.ErrorByte0&FlagLowToner != 0 {
		activeFlags = append(activeFlags, "lowToner")
	}
	if state.ErrorByte0&FlagNoToner != 0 {
		activeFlags = append(activeFlags, "noToner")
	}
	if state.ErrorByte0&FlagDoorOpen != 0 {
		activeFlags = append(activeFlags, "doorOpen")
	}
	if state.ErrorByte0&FlagJammed != 0 {
		activeFlags = append(activeFlags, "jammed")
	}
	if state.ErrorByte0&FlagOffline != 0 {
		activeFlags = append(activeFlags, "offline")
	}
	if state.ErrorByte0&FlagServiceRequested != 0 {
		activeFlags = append(activeFlags, "serviceRequested")
	}
	if state.ErrorByte1&FlagInputTrayMissing != 0 {
		activeFlags = append(activeFlags, "inputTrayMissing")
	}
	if state.ErrorByte1&FlagOutputTrayMissing != 0 {
		activeFlags = append(activeFlags, "outputTrayMissing")
	}
	if state.ErrorByte1&FlagMarkerSupplyMissing != 0 {
		activeFlags = append(activeFlags, "markerSupplyMissing")
	}
	if state.ErrorByte1&FlagOutputNearFull != 0 {
		activeFlags = append(activeFlags, "outputNearFull")
	}
	if state.ErrorByte1&FlagOutputFull != 0 {
		activeFlags = append(activeFlags, "outputFull")
	}
	if state.ErrorByte1&FlagInputTrayEmpty != 0 {
		activeFlags = append(activeFlags, "inputTrayEmpty")
	}
	if state.ErrorByte1&FlagOverduePreventMaint != 0 {
		activeFlags = append(activeFlags, "overduePreventMaint")
	}
	if len(activeFlags) == 0 {
		fmt.Fprintf(w, "  Active Error Flags:    none (healthy)\n")
	} else {
		fmt.Fprintf(w, "  Active Error Flags:    %s\n", strings.Join(activeFlags, ", "))
	}

	fmt.Fprintf(w, "  Markers (%d configured):\n", len(state.Markers))
	for _, m := range state.Markers {
		lifeStr := fmt.Sprintf("%d", m.LifeCount)
		if m.OmitLifeCount {
			lifeStr = "OMITTED (OID not served)"
		}
		fmt.Fprintf(w, "    - Index %s: unit=%s, lifeCount=%s, powerOn=%d\n",
			m.Index, formatMarkerUnitName(m.CounterUnit), lifeStr, m.PowerOnCount)
	}

	fmt.Fprintf(w, "  Supplies (%d configured):\n", len(state.Supplies))
	if len(state.Supplies) == 0 {
		if state.SuppliesCleared {
			fmt.Fprintf(w, "    (Supplies cleared - simulating missing supplies)\n")
		} else {
			fmt.Fprintf(w, "    (No supplies configured)\n")
		}
	}
	for _, sup := range state.Supplies {
		pct := 0
		if sup.MaxCapacity > 0 && sup.Level >= 0 {
			pct = (sup.Level * 100) / sup.MaxCapacity
		}
		fmt.Fprintf(w, "    - Index %s (%s): %d / %d (%d%%) [type=%d, unit=%d]\n",
			sup.Index, sup.Description, sup.Level, sup.MaxCapacity, pct, sup.Type, sup.Unit)
	}

	fmt.Fprintf(w, "  Covers (%d configured):\n", len(state.Covers))
	for _, cov := range state.Covers {
		statusStr := "closed(4)"
		if cov.Status == 3 {
			statusStr = "open(3)"
		}
		fmt.Fprintf(w, "    - Index %s (%s): %s\n", cov.Index, cov.Description, statusStr)
	}

	if len(state.OmittedOIDs) > 0 {
		fmt.Fprintf(w, "  Omitted OIDs:          %s\n", strings.Join(state.OmittedOIDs, ", "))
	}
	if state.Offline {
		fmt.Fprintf(w, "  Communication:         OFFLINE (packets dropped)\n")
	} else {
		fmt.Fprintf(w, "  Communication:         ONLINE\n")
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <serve|get|set|reset> [options]\n", os.Args[0])
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "serve":
		cmdServe(os.Args[2:])
	case "get":
		cmdGet(os.Args[2:])
	case "set":
		cmdSet(os.Args[2:])
	case "reset":
		cmdReset(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command '%s'. Valid commands: serve, get, set, reset\n", cmd)
		os.Exit(1)
	}
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "Host/interface to bind (must be loopback)")
	port := fs.Int("port", 1161, "UDP port to listen on (0 for dynamic port)")
	community := fs.String("community", "public", "SNMPv2c community string")
	stateFile := fs.String("state", "snmp_state.json", "Path to JSON state file")
	pidFile := fs.String("pid", "snmp_responder.pid", "Path to PID file")
	portFile := fs.String("port-file", "", "Optional path to write assigned port number")
	_ = fs.Parse(args)

	if !isLoopbackHost(*host) {
		log.Fatalf("Safety violation: refusing to bind to non-loopback host '%s'", *host)
	}

	store, err := NewStateStore(*stateFile)
	if err != nil {
		log.Fatalf("Failed to initialize state store: %v", err)
	}

	srv, err := NewServer(*host, *port, *community, store, *pidFile)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	if err := srv.Start(); err != nil {
		log.Fatalf("Failed to start SNMP responder: %v", err)
	}
	if *portFile != "" {
		_ = os.WriteFile(*portFile, []byte(fmt.Sprintf("%d\n", srv.Port())), 0644)
	}
	log.Printf("SNMPv2c printer emulator running on %s:%d (community: %s)", *host, srv.Port(), *community)

	sigCh := make(chan os.Signal, 1)
	signal.Ignore(syscall.SIGHUP)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("Received signal %s, shutting down SNMP responder...", sig)
	srv.Stop()
	if *portFile != "" {
		_ = os.Remove(*portFile)
	}
	log.Printf("SNMP responder stopped.")
}

func cmdGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	stateFile := fs.String("state", "snmp_state.json", "Path to JSON state file")
	jsonOutput := fs.Bool("json", false, "Output raw JSON instead of formatted text")
	_ = fs.Parse(args)

	store, err := NewStateStore(*stateFile)
	if err != nil {
		log.Fatalf("Failed to read state: %v", err)
	}
	state := store.GetState()

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(state)
		return
	}
	printState(state, os.Stdout)
}

func cmdSet(args []string) {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	stateFile := fs.String("state", "snmp_state.json", "Path to JSON state file")
	status := fs.String("status", "", "Printer status (idle, printing, warmup, error, offline, unknown or 1-7)")
	setError := fs.String("set-error", "", "Comma-separated error flag(s) to activate")
	clearError := fs.String("clear-error", "", "Comma-separated error flag(s) to clear")
	clearAllErrors := fs.Bool("clear-all-errors", false, "Clear all active error flags")
	errorByte0 := fs.String("error-byte0", "", "Raw byte 0 value (hex 0x.. or int)")
	errorByte1 := fs.String("error-byte1", "", "Raw byte 1 value (hex 0x.. or int)")
	counter := fs.Int64("counter", -1, "Set marker life counter (starting odometer)")
	counterInc := fs.Int64("counter-inc", 0, "Deterministically increment marker life counter")
	tonerLevel := fs.Int("toner-level", -999, "Toner level")
	tonerMax := fs.Int("toner-max", -999, "Toner max capacity")
	drumLevel := fs.Int("drum-level", -999, "Drum level")
	drumMax := fs.Int("drum-max", -999, "Drum max capacity")
	omitOID := fs.String("omit-oid", "", "OID to omit (simulate missing OID)")
	unomitOID := fs.String("unomit-oid", "", "OID to restore if previously omitted")
	clearOmitted := fs.Bool("clear-omitted", false, "Clear all omitted OIDs")
	sysDescr := fs.String("sys-descr", "", "Set sysDescr value")
	sysObjectID := fs.String("sys-object-id", "", "Set sysObjectID value")
	printerName := fs.String("printer-name", "", "Set prtGeneralPrinterName value")
	printerIndex := fs.String("printer-index", "", "Set printer / hrDeviceIndex (e.g. 1 or 2)")
	markerIndex := fs.String("marker-index", "", "Set primary marker index (e.g. 1.1 or 2.1)")
	markerUnit := fs.Int("marker-unit", -1, "Set primary marker counter unit (8=sheets, 7=impressions, 11=hours, 16=feet)")
	markerOmitLife := fs.String("marker-omit-life", "", "Omit marker life count OID (true/false)")
	supplyIndex := fs.String("supply-index", "", "Set primary toner supply index (e.g. 1.1 or 2.1)")
	drumIndex := fs.String("drum-index", "", "Set secondary drum supply index (e.g. 1.2 or 2.2)")
	clearSupplies := fs.Bool("clear-supplies", false, "Clear all supplies (simulate missing supplies)")
	restoreSupplies := fs.Bool("restore-supplies", false, "Restore default supplies")
	coverIndex := fs.String("cover-index", "", "Set cover index (e.g. 1.1)")
	coverStatus := fs.Int("cover-status", -1, "Set cover status (3=coverOpen, 4=coverClosed)")
	uptime := fs.Int64("uptime", -1, "Set sysUpTime in 10ms ticks")
	restart := fs.Bool("restart", false, "Simulate printer restart (resets uptime to 100 ticks)")
	offline := fs.String("offline", "", "Simulate communication loss (true/false)")
	_ = fs.Parse(args)

	store, err := NewStateStore(*stateFile)
	if err != nil {
		log.Fatalf("Failed to load state: %v", err)
	}

	err = store.UpdateState(func(s *SNMPState) {
		if *status != "" {
			st, err := parseStatusString(*status)
			if err != nil {
				log.Fatalf("Invalid status: %v", err)
			}
			s.HrPrinterStatus = st
		}
		if *clearAllErrors {
			s.ErrorByte0 = 0x00
			s.ErrorByte1 = 0x00
		}
		if *setError != "" {
			for _, item := range strings.Split(*setError, ",") {
				if bIdx, mask, ok := parseErrorFlag(item); ok {
					if bIdx == 0 {
						s.ErrorByte0 |= mask
					} else {
						s.ErrorByte1 |= mask
					}
				} else {
					log.Fatalf("Unknown error flag '%s'", item)
				}
			}
		}
		if *clearError != "" {
			for _, item := range strings.Split(*clearError, ",") {
				if bIdx, mask, ok := parseErrorFlag(item); ok {
					if bIdx == 0 {
						s.ErrorByte0 &^= mask
					} else {
						s.ErrorByte1 &^= mask
					}
				} else {
					log.Fatalf("Unknown error flag '%s'", item)
				}
			}
		}
		if *errorByte0 != "" {
			val, err := strconv.ParseUint(strings.TrimPrefix(*errorByte0, "0x"), 16, 8)
			if err != nil {
				val64, err2 := strconv.ParseUint(*errorByte0, 10, 8)
				if err2 != nil {
					log.Fatalf("Invalid error-byte0 value: %v", err)
				}
				val = val64
			}
			s.ErrorByte0 = byte(val)
		}
		if *errorByte1 != "" {
			val, err := strconv.ParseUint(strings.TrimPrefix(*errorByte1, "0x"), 16, 8)
			if err != nil {
				val64, err2 := strconv.ParseUint(*errorByte1, 10, 8)
				if err2 != nil {
					log.Fatalf("Invalid error-byte1 value: %v", err)
				}
				val = val64
			}
			s.ErrorByte1 = byte(val)
		}
		if *counter >= 0 {
			s.MarkerLifeCount = uint32(*counter)
			if len(s.Markers) > 0 {
				s.Markers[0].LifeCount = uint32(*counter)
			}
		}
		if *counterInc > 0 {
			s.MarkerLifeCount += uint32(*counterInc)
			if len(s.Markers) > 0 {
				s.Markers[0].LifeCount = s.MarkerLifeCount
			}
		}
		if *tonerLevel != -999 {
			s.TonerLevel = *tonerLevel
		}
		if *tonerMax != -999 {
			s.TonerMax = *tonerMax
		}
		if *drumLevel != -999 {
			s.DrumLevel = *drumLevel
		}
		if *drumMax != -999 {
			s.DrumMax = *drumMax
		}
		if *clearOmitted {
			s.OmittedOIDs = nil
		}
		if *omitOID != "" {
			norm := normalizeOID(*omitOID)
			found := false
			for _, o := range s.OmittedOIDs {
				if normalizeOID(o) == norm {
					found = true
					break
				}
			}
			if !found {
				s.OmittedOIDs = append(s.OmittedOIDs, norm)
			}
		}
		if *unomitOID != "" {
			norm := normalizeOID(*unomitOID)
			var updated []string
			for _, o := range s.OmittedOIDs {
				if normalizeOID(o) != norm {
					updated = append(updated, o)
				}
			}
			s.OmittedOIDs = updated
		}
		if *sysDescr != "" {
			s.SysDescr = *sysDescr
		}
		if *sysObjectID != "" {
			s.SysObjectID = *sysObjectID
		}
		if *printerName != "" {
			s.PrinterName = *printerName
		}
		if *printerIndex != "" {
			s.PrinterIndex = *printerIndex
		}
		if *markerIndex != "" {
			if len(s.Markers) == 0 {
				s.Markers = append(s.Markers, MarkerState{Index: *markerIndex, CounterUnit: MarkerUnitSheets, LifeCount: s.MarkerLifeCount})
			} else {
				s.Markers[0].Index = *markerIndex
			}
		}
		if *markerUnit >= 0 {
			if len(s.Markers) == 0 {
				s.Markers = append(s.Markers, MarkerState{Index: s.PrinterIndex + ".1", CounterUnit: *markerUnit, LifeCount: s.MarkerLifeCount})
			} else {
				s.Markers[0].CounterUnit = *markerUnit
			}
		}
		if *markerOmitLife != "" {
			omit := strings.EqualFold(*markerOmitLife, "true") || *markerOmitLife == "1"
			if len(s.Markers) == 0 {
				s.Markers = append(s.Markers, MarkerState{Index: s.PrinterIndex + ".1", CounterUnit: MarkerUnitSheets, LifeCount: s.MarkerLifeCount, OmitLifeCount: omit})
			} else {
				s.Markers[0].OmitLifeCount = omit
			}
		}
		if *clearSupplies {
			s.SuppliesCleared = true
			s.Supplies = nil
		}
		if *restoreSupplies {
			s.SuppliesCleared = false
			s.Supplies = nil
		}
		if *supplyIndex != "" {
			if len(s.Supplies) == 0 {
				s.Supplies = append(s.Supplies, SupplyState{Index: *supplyIndex, Description: "Black Toner Cartridge (Synthetic Test Fixture)", Type: 3, Class: 3, Unit: 15, MaxCapacity: s.TonerMax, Level: s.TonerLevel})
			} else {
				s.Supplies[0].Index = *supplyIndex
			}
		}
		if *drumIndex != "" {
			if len(s.Supplies) > 1 {
				s.Supplies[1].Index = *drumIndex
			}
		}
		if *coverIndex != "" {
			if len(s.Covers) == 0 {
				s.Covers = append(s.Covers, CoverState{Index: *coverIndex, Description: "Front Cover", Status: 4})
			} else {
				s.Covers[0].Index = *coverIndex
			}
		}
		if *coverStatus >= 0 {
			if len(s.Covers) == 0 {
				s.Covers = append(s.Covers, CoverState{Index: s.PrinterIndex + ".1", Description: "Front Cover", Status: *coverStatus})
			} else {
				s.Covers[0].Status = *coverStatus
			}
		}
		if *uptime >= 0 {
			s.SysUpTime = uint32(*uptime)
		}
		if *restart {
			s.SysUpTime = 100 // 1 second
		}
		if *offline != "" {
			s.Offline = strings.EqualFold(*offline, "true") || *offline == "1"
		}
	})

	if err != nil {
		log.Fatalf("Failed to update state: %v", err)
	}
	printState(store.GetState(), os.Stdout)
}

func cmdReset(args []string) {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	stateFile := fs.String("state", "snmp_state.json", "Path to JSON state file")
	_ = fs.Parse(args)

	store, err := NewStateStore(*stateFile)
	if err != nil {
		log.Fatalf("Failed to open state store: %v", err)
	}
	err = store.UpdateState(func(s *SNMPState) {
		*s = DefaultState()
	})
	if err != nil {
		log.Fatalf("Failed to reset state: %v", err)
	}
	fmt.Printf("State reset to default healthy/idle.\n")
	printState(store.GetState(), os.Stdout)
}
