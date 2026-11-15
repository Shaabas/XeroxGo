# XeroxGo Local IPP Printer Emulator

An isolated Linux laptop test harness using CUPS `ippeveprinter` to emulate an IPP Everywhere printer for local development, capability discovery, and shadow-health testing.

---

## 1. Overview & Architecture

The emulator uses the system-installed `ippeveprinter` tool provided by CUPS / OpenPrinting. It conforms to the **PWG IPP Everywhere (PWG 5100.14)** standard, enabling full driverless IPP printer emulation without requiring physical printer hardware or external network connections.

### Key Principles & Safety Guarantees
- **Local Isolation:** Binds strictly to `localhost` (`127.0.0.1` / `::1`) by default on an unprivileged port (`8631`). Never exposes the emulator publicly.
- **Port Availability & IPP Readiness Verification:** Startup validates that the port is not in use before launching, and actively tests the endpoint with `ipptool` before declaring success. A running PID alone is never treated as success.
- **Concurrency Protection:** Uses atomic locking to prevent concurrent or repeated startup from corrupting ownership metadata or spawning multiple listeners.
- **Hardware Safety:** Uses a dedicated spool directory (`tools/printer-emulator/spool`). Print jobs are preserved as local test artifacts and are **never** forwarded to any physical printer.
- **Spool Path Safety & Ownership Verification:** Validates all spool paths against root (`/`), system directories (`/etc`, `/usr`, `/var`, etc.), and symlink escapes. Unowned directories must be completely empty before the emulator will claim ownership and initialize `.xeroxgo_emulator_spool`; pre-existing directories containing unrelated files are strictly refused. Existing owned directories may only be reused if ownership is verified and all files are valid emulator artifacts.
- **Authoritative Artifact Tracking & Restart Safety:** Filename patterns alone never authorize artifact enrollment or file deletion. The manifest (`.xeroxgo_emulator_manifest`) and execution logs are authoritative. On restart, pre-existing untracked files are never registered as emulator-owned. Unrelated files matching artifact patterns (e.g. `2-notes.pdf`) survive full `start` → `stop` → `restart` → `cleanup` lifecycles completely untouched. Refuses destructive operations when ownership is uncertain.
- **PID Validation:** Validates the PID file against the running `ippeveprinter` process name, port, and printer name before signaling it. Rejects stale, reused, malformed, or unrelated PIDs. Eliminates unconditional `kill -9`.
- **Queue Ownership Tracking & Non-Destructive Management:** `setup-cups` never overwrites existing queues; it creates queues only when the name is unused. Ownership metadata (`cups_queue.meta`) tracks the queue and its expected IPP URI. `remove-cups` and `cleanup` verify both metadata ownership and actual device URI (`lpstat -v`) before deletion, refusing deletion if ownership cannot be verified. Pre-existing physical or production queues are completely protected.
- **Zero Daemon Dependencies:** Adds no runtime or binary dependencies to the production kiosk daemon. Production code remains untouched.

---

## 2. Installation Requirements

`ippeveprinter` and CUPS client utilities are packaged across major Linux distributions as follows:

| Distribution | Package Installation Command | Service Startup |
|---|---|---|
| **Fedora / RHEL / CentOS Stream** | `sudo dnf install -y cups-printerapp cups-ipptool cups-client cups` | `sudo systemctl enable --now cups` |
| **Ubuntu / Debian / Linux Mint** | `sudo apt-get update && sudo apt-get install -y cups-ipp-utils cups-client cups` | `sudo systemctl enable --now cups` |
| **Arch Linux / Manjaro** | `sudo pacman -S --needed cups cups-filters` | `sudo systemctl enable --now cups` |
| **Alpine Linux** | `sudo apk add cups cups-client` | `sudo rc-service cupsd start` |
| **openSUSE Tumbleweed / Leap** | `sudo zypper install -y cups cups-client` | `sudo systemctl enable --now cups` |

---

## 3. Quick Start & Basic Commands

All commands can be executed via `./emulator.sh <command>` or the convenience scripts:

```bash
cd tools/printer-emulator

# 1. Start emulator in background (binds to localhost:8631)
./start.sh
# or: ./emulator.sh start

# 2. Check emulator status and spool contents
./status.sh
# or: ./emulator.sh status

# 3. Create temporary CUPS driverless queue (XeroxGoEmu)
./emulator.sh setup-cups

# 4. Verify CUPS queue registration
lpstat -p XeroxGoEmu -l

# 5. Submit a test PDF through CUPS
lp -d XeroxGoEmu testdata/sample.pdf

# 6. Verify received print artifact in spool directory
ls -la spool/

# 7. Remove the temporary CUPS queue
./emulator.sh remove-cups

# 8. Stop the emulator
./stop.sh
# or: ./emulator.sh stop

# 9. Clean up all resources (removes queue, stops daemon, clears spool files)
./cleanup.sh
# or: ./emulator.sh cleanup
```

---

## 4. Configuration Reference

The emulator can be configured using command-line flags or environment variables:

| Parameter | CLI Flag | Environment Variable | Default Value | Description |
|---|---|---|---|---|
| Host | `-h, --host` | `EMU_HOST` | `localhost` | Host interface / hostname to bind |
| Port | `-p, --port` | `EMU_PORT` | `8631` | TCP port number |
| Printer Name | `-n, --name` | `EMU_NAME` | `XeroxGoEmu` | IPP service name |
| CUPS Queue | `-q, --queue` | `EMU_CUPS_QUEUE` | `XeroxGoEmu` | Name for temporary driverless CUPS queue |
| Spool Dir | `-d, --spool-dir` | `EMU_SPOOL_DIR` | `./spool` | Dedicated output directory for received print files |
| Processor | N/A | `EMU_PROCESSOR` | `./processor.sh` | Print command script invoked for each job |
| Formats | N/A | `EMU_FORMATS` | `application/pdf,...` | Comma-separated MIME media types accepted |
| Speed / Color | N/A | `EMU_SPEED` | `20,20` | PPM speed rating; second number > 0 enables color |
| Duplex | N/A | `EMU_DUPLEX` | `true` | When true, passes `-2` to advertise two-sided printing |

Example running on a custom port:
```bash
EMU_PORT=8640 ./emulator.sh start
```

---

## 5. Verification Commands

### Direct IPP Endpoint Verification (`ipptool`)
Verify that the IPP endpoint is reachable and advertising valid IPP Everywhere attributes:
```bash
ipptool -tv ipp://localhost:8631/ipp/print get-printer-attributes.test
```
Expected output highlights:
```text
RECEIVED: 9171 bytes in response
status-code = successful-ok (successful-ok)
printer-state (enum) = idle
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = none
```

### Direct IPP Print Submission (`ipptool`)
Submit a test PDF directly to the IPP endpoint:
```bash
ipptool -tv -f testdata/sample.pdf ipp://localhost:8631/ipp/print print-job.test
```
Check completed jobs:
```bash
ipptool -tv ipp://localhost:8631/ipp/print get-completed-jobs.test
```

### CUPS Queue Verification (`lpstat`)
Once `setup-cups` has run, inspect the queue using standard CUPS commands:
```bash
# General status
lpstat -p XeroxGoEmu

# Detailed queue properties
lpstat -p XeroxGoEmu -l
```

### Submitting Print Jobs via CUPS (`lp`)
Submit a test document through CUPS to verify end-to-end spooling and dispatch:
```bash
lp -d XeroxGoEmu testdata/sample.pdf
```
Inspect CUPS job completion:
```bash
lpstat -W completed -d XeroxGoEmu
```

---

## 6. Recorded IPP Attributes

`ippeveprinter` provides a rich set of standard IPP attributes. The following key attributes were captured and verified during emulator validation:

### Printer Identity & State
- `printer-name (nameWithoutLanguage) = XeroxGoEmu`
- `printer-make-and-model (textWithoutLanguage) = Example Printer`
- `printer-info (textWithoutLanguage) = XeroxGoEmu`
- `printer-state (enum) = idle` (or `processing`, `stopped`)
- `printer-is-accepting-jobs (boolean) = true`
- `printer-state-reasons (keyword) = none`
- `printer-uri-supported = ipp://localhost:8631/ipp/print, ipps://localhost:8631/ipp/print`

### Media & Capabilities
- `document-format-supported = application/pdf, image/pwg-raster, image/urf, application/octet-stream, text/plain`
- `media-supported = na_letter_8.5x11in, na_legal_8.5x14in, iso_a4_210x297mm, na_number-10_4.125x9.5in, iso_dl_110x220mm`
- `sides-supported = one-sided, two-sided-long-edge, two-sided-short-edge` (via `-2`)
- `color-supported (boolean) = true` (via `-s 20,20`)
- `print-color-mode-supported = auto, color, monochrome`
- `copies-supported (rangeOfInteger) = 1-999`
- `page-ranges-supported (boolean) = true`
- `printer-resolution-supported = 600dpi`

### Consumables (Synthetic)
- `printer-supply = index=1;class=receptacleThatIsFilled;type=wasteToner;level=25;, index=2;class=supplyThatIsConsumed;type=toner;level=75;`
- `printer-supply-description = Toner Waste Tank, Black Toner`

---

## 7. Job Lifecycle Behavior

In `ippeveprinter`, the job lifecycle proceeds through standard IPP states:

1. **Submission (`Print-Job` / `Create-Job`):**
   - The client submits the document.
   - `ippeveprinter` validates attributes and document format.
   - Returns `successful-ok`, assigning an integer `job-id` and `job-uri`.
   - Initial state: `pending`.

2. **Processing:**
   - As processing begins, state transitions to `processing` with reason `job-printing`.
   - The file is saved in the dedicated spool directory with `-k` (e.g. `1-sample.pdf`).

3. **Execution & Completion:**
   - `ippeveprinter` invokes the print processor command (`-c processor.sh`) with document data on stdin.
   - `processor.sh` consumes the document and exits with code `0`.
   - Upon successful exit code 0, `ippeveprinter` transitions the job to `completed` with reason `job-completed-successfully`.
   - Both the raw received PDF (`<job-id>-<title>.pdf`) and print output (`<job-id>-<title>.prn`) are retained in the spool directory as test artifacts.

> [!NOTE]
> If no print command (`-c`) is specified, `ippeveprinter` leaves jobs in the `processing` state indefinitely awaiting a command. The provided `processor.sh` script drains the stdin stream, ensuring clean and deterministic job completion.

---

## 8. Limitations of `ippeveprinter`

When using `ippeveprinter` as a development and test harness, note the following architectural boundaries:

1. **No Native SNMP in ippeveprinter:**
   - `ippeveprinter` implements only the IPP protocol over HTTP/HTTPS.
   - It does **not** listen on UDP port 161 or respond to SNMP MIB queries (`HOST-RESOURCES-MIB`, `Printer-MIB`).
   - SNMP emulation is implemented as a dedicated lightweight SNMPv2c responder daemon (`snmp_responder`, see Section 9).

2. **Accurate Hardware Page Counting (Odometers):**
   - Physical page counts and sensor odometers do not exist in software emulation.
   - Impressions and completed pages reflect client-supplied attributes or print processor signals.
   - In the SNMP emulator, odometer counters can be deterministically updated for testing, but they represent synthetic telemetry, never physical sheets.

3. **Hardware Fault Simulation:**
   - `ippeveprinter` starts in a clean `idle` state without physical faults.
   - Simulating faults in IPP requires print processors emitting `STATE: +media-jam-error` on stderr.
   - For SNMP telemetry, full RFC-compatible hardware faults (paper jam, cover open, empty tray, output full, low consumables) are controlled dynamically via the SNMP emulator (`snmp-set`).

4. **Document Format Advertising:**
   - By default, `ippeveprinter` only advertises raster formats (`image/pwg-raster`, `image/urf`, `application/octet-stream`).
   - To support direct PDF submission from CUPS without rasterization filters, `-f application/pdf,...` must be explicitly specified (configured by default in `emulator.sh`).

5. **Port Binding Restrictions:**
   - Non-root users cannot bind to standard IPP port 631 or SNMP port 161.
   - Unprivileged ports (`> 1024`, e.g. IPP `8631`, SNMP UDP `1161`) must be used.

---

## 9. Local SNMP Printer Emulator (SNMPv2c)

To complement IPP Everywhere emulation with accurate physical hardware telemetry, a local SNMPv2c responder daemon is provided in `snmp_responder.go`.

### Architecture & Safety Guarantees
- **Pure Loopback:** Binds strictly to `127.0.0.1` / `localhost` on an unprivileged UDP port (default `1161`). Safely rejects any non-loopback bind address.
- **Protocol Compliance:** Uses `github.com/gosnmp/gosnmp` to decode and encode standard SNMPv2c packets with strict ASN.1 typing without custom BER parsers.
- **Separate State:** State is stored in an isolated JSON file (`snmp_state.json`), completely separated from production databases and logs.
- **Dynamic Reloading:** State changes made via `snmp-set` take effect immediately without needing server restarts.
- **Safety Protections:** Port collision detection, PID ownership verification before signaling, and safe cleanup prevent interfering with host services or unrelated processes.

### Supported MIB OIDs & ASN.1 Types

| OID | MIB / Object Name | ASN.1 Type | Default Value | Description |
|---|---|---|---|---|
| `1.3.6.1.2.1.1.1.0` | `SNMPv2-MIB::sysDescr.0` | `OctetString` | `"Xerox WorkCentre 3025 (Local Emulator)"` | System hardware description |
| `1.3.6.1.2.1.25.3.5.1.1.1` | `HOST-RESOURCES-MIB::hrPrinterStatus.1` | `Integer` | `3` (`idle`) | RFC 2790 printer state (1=other, 2=unknown, 3=idle, 4=printing, 5=warmup) |
| `1.3.6.1.2.1.25.3.5.1.2.1` | `HOST-RESOURCES-MIB::hrPrinterDetectedErrorState.1` | `OctetString` | `[0x00, 0x00]` | RFC 2790 2-byte error bitmasks |
| `1.3.6.1.2.1.43.10.2.1.4.1.1` | `Printer-MIB::prtMarkerLifeCount.1.1` | `Counter32` | `1000` | Physical odometer count |
| `1.3.6.1.2.1.43.11.1.1.9.1.1` | `Printer-MIB::prtMarkerSuppliesLevel.1.1` | `Integer` | `85` | Supply 1 (toner) level |
| `1.3.6.1.2.1.43.11.1.1.8.1.1` | `Printer-MIB::prtMarkerSuppliesMaxCapacity.1.1` | `Integer` | `100` | Supply 1 (toner) max capacity |
| `1.3.6.1.2.1.43.11.1.1.9.1.2` | `Printer-MIB::prtMarkerSuppliesLevel.1.2` | `Integer` | `95` | Supply 2 (drum) level |
| `1.3.6.1.2.1.43.11.1.1.8.1.2` | `Printer-MIB::prtMarkerSuppliesMaxCapacity.1.2` | `Integer` | `100` | Supply 2 (drum) max capacity |

### RFC 2790 Error Bitmasks

**Byte 0 (bits 0..7):**
- `0x80`: `lowPaper`
- `0x40`: `noPaper`
- `0x20`: `lowToner`
- `0x10`: `noToner`
- `0x08`: `doorOpen`
- `0x04`: `jammed`
- `0x02`: `offline`
- `0x01`: `serviceRequested`

**Byte 1 (bits 8..15):**
- `0x80`: `inputTrayMissing`
- `0x40`: `outputTrayMissing`
- `0x20`: `markerSupplyMissing`
- `0x10`: `outputNearFull`
- `0x08`: `outputFull`
- `0x04`: `inputTrayEmpty`
- `0x02`: `overduePreventMaint`

### SNMP Quick Start & Commands

```bash
cd tools/printer-emulator

# 1. Start SNMP emulator daemon (default UDP 1161, community public)
./start_snmp.sh
# or: ./emulator.sh start-snmp

# 2. Check SNMP emulator status and current telemetry
./status_snmp.sh
# or: ./emulator.sh status-snmp

# 3. Query telemetry with snmpget CLI
snmpget -v2c -c public 127.0.0.1:1161 \
    1.3.6.1.2.1.1.1.0 \
    1.3.6.1.2.1.25.3.5.1.1.1 \
    1.3.6.1.2.1.25.3.5.1.2.1 \
    1.3.6.1.2.1.43.10.2.1.4.1.1

# 4. Inject simulated hardware faults
./emulator.sh snmp-set -set-error jammed,door_open

# 5. Deterministically increment odometer counter
./emulator.sh snmp-set -counter-inc 50

# 6. Simulate missing / unsupported OIDs
./emulator.sh snmp-set -omit-oid 1.3.6.1.2.1.43.10.2.1.4.1.1

# 7. Reset to default healthy/idle state
./emulator.sh snmp-reset

# 8. Stop SNMP emulator
./stop_snmp.sh
# or: ./emulator.sh stop-snmp

# 9. Clean up all SNMP runtime files
./emulator.sh cleanup-snmp
```

---

## 10. Smoke Test Suites

### IPP Smoke & Safety Test Suite
```bash
./smoke_test.sh
```
Runs 19 comprehensive tests validating IPP Everywhere operations, CUPS driverless printing, spool path safety, PID protection, and artifact persistence.

### SNMP Smoke & Safety Test Suite
```bash
./snmp_smoke_test.sh
```
Runs 15 automated tests validating:
- Startup, UDP port binding, and process lifecycle
- Default healthy state MIB queries via `snmpget`
- Status reporting and state inspection
- Controlled Byte 0 and Byte 1 error bit updates
- Consumables and status modifications
- Deterministic counter updates
- Missing OID simulation (`NoSuchInstance`)
- Stop → Restart state persistence
- Reset to healthy state
- Unauthorized community string rejection
- Occupied port detection and non-loopback safety enforcement
- Unrelated PID protection
- Safe cleanup

### Go Integration Test Suite
```bash
go test -v -race ./kiosk-daemon/internal/printer -run TestSNMPEmulator
```
Exercises XeroxGo's Go SNMP polling implementation against the local emulator, verifying:
- Healthy state polling (`PollHealthConfig`, `PollHardwareTelemetry`, `PollMarkerLifeCount`)
- Error bit decoding across all RFC 2790 flags
- Toner and drum percentage calculations
- Missing OIDs returning `UNKNOWN` state
- Timeouts and context cancellation
- Restart persistence across daemon restarts
- Deterministic counter updates

---

## 11. Coordinated IPP + SNMP Fault Injection (Task D3)

Simulates a single physical printer with coherent, coordinated telemetry across both the D1 IPP (`ippeveprinter`) and D2 SNMP (`snmp_responder`) emulators.

### Coordinated Fault Controller Architecture

The fault controller (`tools/printer-emulator/fault.sh` or `./emulator.sh fault`) orchestrates both emulators through a shared state model (`fault_state.json`), ensuring that simulated physical printer conditions update both IPP attributes and SNMP MIB observations consistently.

```
                  ┌──────────────────────────────────────────────┐
                  │   fault.sh / emulator.sh fault controller     │
                  └───────┬──────────────────────────────┬───────┘
                          │                              │
             (CUPS filter protocol & SIGSTOP)     (Local state & packet drop)
                          │                              │
                          ▼                              ▼
              ┌──────────────────────┐       ┌──────────────────────┐
              │    ippeveprinter     │       │    snmp_responder    │
              │   (IPP Everywhere)   │       │   (RFC 2790 SNMPv2c) │
              └──────────────────────┘       └──────────────────────┘
```

### Supported Scenarios

| Scenario | IPP Attributes (`printer-state-reasons`) | SNMP MIBs (`hrPrinterStatus`, Error Bits) | Description |
|---|---|---|---|
| `healthy` | `printer-state=idle`, `reasons=none` | `hrPrinterStatus=idle(3)`, `00 00` | Normal idle operation; zero error flags |
| `printing` | `printer-state=processing`, active job delay | `hrPrinterStatus=printing(4)`, `00 00` | Simulated active print execution |
| `paper-empty` | `reasons=media-empty-report` | `hrPrinterStatus=other(1)`, `40 00` (`noPaper`) | Paper tray empty |
| `paper-jam` | `reasons=media-jam-report` | `hrPrinterStatus=other(1)`, `04 00` (`jammed`) | Media jam in transport path |
| `cover-open` | `reasons=cover-open-report` | `hrPrinterStatus=other(1)`, `08 00` (`doorOpen`) | Access door or cover open |
| `stopped` | `reasons=paused-report`, `cupsdisable` queue | `hrPrinterStatus=other(1)` | Stopped / paused queue state |
| `offline` | Transport unresponsive (SIGSTOP) | UDP packets dropped silently | Complete communication loss |
| `recovery` | Restored to `idle`, `reasons=none` | `hrPrinterStatus=idle(3)`, `00 00` | Clean recovery from any prior fault |

### Truthful Emulation & Subsystem Characteristics

- **Stopped State:** Standalone `ippeveprinter` only supports internal states 3 (`idle`) and 4 (`processing`) in its binary. State 5 (`stopped`) is truthfully represented in standalone IPP via the `paused` reason (`paused-report`), and when a temporary CUPS queue is registered, `cupsdisable <queue>` disables the queue.
- **Media Empty Wait Loop:** `ippeveprinter` suspends command processing when `media-empty` is set. When transitioning away from `paper-empty`, the controller performs an instantaneous in-memory restart (< 0.1s) of `ippeveprinter` to reset the bitmask cleanly.
- **Controlled Communication Loss:** The `offline` scenario pauses `ippeveprinter` via `SIGSTOP` (leaving TCP sockets unresponsive) and configures `snmp_responder` to silently drop incoming UDP packets. Recovery restores both immediately without modifying host interfaces, routes, or iptables.
- **Deterministic Odometer:** Page counts are deterministic and explicitly controllable via `--counter <N>`. Processing test jobs or synchronization probes does not auto-increment the counter.

### Fault Controller CLI Usage

```bash
cd tools/printer-emulator

# 1. Inspect coordinated status and cross-subsystem telemetry analysis
./fault.sh status
# or: ./emulator.sh fault status

# 2. Inject coordinated paper-jam scenario across both IPP and SNMP
./fault.sh set paper-jam

# 3. Inject coordinated cover-open scenario
./fault.sh set cover-open

# 4. Inject coordinated paper-empty scenario
./fault.sh set paper-empty

# 5. Simulate communication loss (both IPP TCP and SNMP UDP unresponsive)
./fault.sh set offline

# 6. Recover from offline or any active fault
./fault.sh set recovery

# 7. Independent target injection (simulate subsystem contradictions/disagreements)
# IPP reports paper-jam while SNMP reports healthy:
./fault.sh set --target ipp paper-jam

# SNMP reports paper-jam while IPP reports healthy:
./fault.sh set --ipp healthy --snmp paper-jam

# 8. Deterministic counter and consumable configuration
./fault.sh set healthy --counter 12500 --toner 40 --drum 75

# 9. Clear all faults and return to healthy agreement
./fault.sh clear
```

### Coordinated Fault Smoke & Verification Suites

```bash
# Run comprehensive D3 smoke test suite (35 automated assertions)
./fault_smoke_test.sh

# Run XeroxGo Go reconciliation integration test suite
go test -v -race ./kiosk-daemon/internal/printer -run TestFaultReconciliation
```
