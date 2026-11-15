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
	oidHrPrinterStatus             = "1.3.6.1.2.1.25.3.5.1.1.1"
	oidHrPrinterDetectedErrorState = "1.3.6.1.2.1.25.3.5.1.2.1"
	oidPrtMarkerLifeCount          = "1.3.6.1.2.1.43.10.2.1.4.1.1"
	oidPrtMarkerSuppliesLevel1     = "1.3.6.1.2.1.43.11.1.1.9.1.1"
	oidPrtMarkerSuppliesMax1       = "1.3.6.1.2.1.43.11.1.1.8.1.1"
	oidPrtMarkerSuppliesLevel2     = "1.3.6.1.2.1.43.11.1.1.9.1.2"
	oidPrtMarkerSuppliesMax2       = "1.3.6.1.2.1.43.11.1.1.8.1.2"
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

// SNMPState encapsulates the simulated printer telemetry state.
type SNMPState struct {
	SysDescr        string   `json:"sys_descr"`
	HrPrinterStatus int      `json:"hr_printer_status"`
	ErrorByte0      byte     `json:"error_byte_0"`
	ErrorByte1      byte     `json:"error_byte_1"`
	MarkerLifeCount uint32   `json:"marker_life_count"`
	TonerLevel      int      `json:"toner_level"`
	TonerMax        int      `json:"toner_max"`
	DrumLevel       int      `json:"drum_level"`
	DrumMax         int      `json:"drum_max"`
	OmittedOIDs     []string `json:"omitted_oids,omitempty"`
}

// DefaultState returns healthy/idle default state.
func DefaultState() SNMPState {
	return SNMPState{
		SysDescr:        "Xerox WorkCentre 3025 (Local Emulator)",
		HrPrinterStatus: 3, // idle
		ErrorByte0:      0x00,
		ErrorByte1:      0x00,
		MarkerLifeCount: 1000,
		TonerLevel:      85,
		TonerMax:        100,
		DrumLevel:       95,
		DrumMax:         100,
		OmittedOIDs:     nil,
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
	if port <= 0 || port > 65535 {
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

		packet, err := g.SnmpDecodePacket(buf[:n])
		if err != nil {
			continue
		}

		// Security: verify community string
		if packet.Community != srv.community {
			continue
		}

		if packet.PDUType != gosnmp.GetRequest {
			// SNMPv2c GetNext / Bulk not queried by XeroxGo, ignore
			continue
		}

		resp := &gosnmp.SnmpPacket{
			Version:   packet.Version,
			Community: packet.Community,
			PDUType:   gosnmp.GetResponse,
			RequestID: packet.RequestID,
			Error:     gosnmp.NoError,
			Variables: make([]gosnmp.SnmpPDU, 0, len(packet.Variables)),
		}

		for _, pdu := range packet.Variables {
			normOID := normalizeOID(pdu.Name)

			// Check if OID is explicitly omitted
			omitted := false
			for _, o := range state.OmittedOIDs {
				if normalizeOID(o) == normOID {
					omitted = true
					break
				}
			}

			if omitted {
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.NoSuchInstance,
					Value: nil,
				})
				continue
			}

			switch normOID {
			case normalizeOID(oidSysDescr):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.OctetString,
					Value: []byte(state.SysDescr),
				})
			case normalizeOID(oidHrPrinterStatus):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.HrPrinterStatus,
				})
			case normalizeOID(oidHrPrinterDetectedErrorState):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.OctetString,
					Value: []byte{state.ErrorByte0, state.ErrorByte1},
				})
			case normalizeOID(oidPrtMarkerLifeCount):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Counter32,
					Value: state.MarkerLifeCount,
				})
			case normalizeOID(oidPrtMarkerSuppliesLevel1):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.TonerLevel,
				})
			case normalizeOID(oidPrtMarkerSuppliesMax1):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.TonerMax,
				})
			case normalizeOID(oidPrtMarkerSuppliesLevel2):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.DrumLevel,
				})
			case normalizeOID(oidPrtMarkerSuppliesMax2):
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.Integer,
					Value: state.DrumMax,
				})
			default:
				resp.Variables = append(resp.Variables, gosnmp.SnmpPDU{
					Name:  pdu.Name,
					Type:  gosnmp.NoSuchInstance,
					Value: nil,
				})
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

func printState(state SNMPState, w io.Writer) {
	fmt.Fprintf(w, "SNMP Emulator State:\n")
	fmt.Fprintf(w, "  sysDescr:              %s\n", state.SysDescr)
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

	fmt.Fprintf(w, "  prtMarkerLifeCount:    %d\n", state.MarkerLifeCount)
	tonerPct := 0
	if state.TonerMax > 0 && state.TonerLevel >= 0 {
		tonerPct = (state.TonerLevel * 100) / state.TonerMax
	}
	fmt.Fprintf(w, "  Toner (Supply 1):      %d / %d (%d%%)\n", state.TonerLevel, state.TonerMax, tonerPct)
	drumPct := 0
	if state.DrumMax > 0 && state.DrumLevel >= 0 {
		drumPct = (state.DrumLevel * 100) / state.DrumMax
	}
	fmt.Fprintf(w, "  Drum (Supply 2):       %d / %d (%d%%)\n", state.DrumLevel, state.DrumMax, drumPct)
	if len(state.OmittedOIDs) > 0 {
		fmt.Fprintf(w, "  Omitted OIDs:          %s\n", strings.Join(state.OmittedOIDs, ", "))
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
	port := fs.Int("port", 1161, "UDP port to listen on")
	community := fs.String("community", "public", "SNMPv2c community string")
	stateFile := fs.String("state", "snmp_state.json", "Path to JSON state file")
	pidFile := fs.String("pid", "snmp_responder.pid", "Path to PID file")
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
	log.Printf("SNMPv2c printer emulator running on %s:%d (community: %s)", *host, srv.Port(), *community)

	sigCh := make(chan os.Signal, 1)
	signal.Ignore(syscall.SIGHUP)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("Received signal %s, shutting down SNMP responder...", sig)
	srv.Stop()
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
		}
		if *counterInc > 0 {
			s.MarkerLifeCount += uint32(*counterInc)
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
