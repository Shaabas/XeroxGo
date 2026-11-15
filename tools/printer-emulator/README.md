# XeroxGo Local IPP Printer Emulator

An isolated Linux laptop test harness using CUPS `ippeveprinter` to emulate an IPP Everywhere printer for local development, capability discovery, and shadow-health testing.

---

## 1. Overview & Architecture

The emulator uses the system-installed `ippeveprinter` tool provided by CUPS / OpenPrinting. It conforms to the **PWG IPP Everywhere (PWG 5100.14)** standard, enabling full driverless IPP printer emulation without requiring physical printer hardware or external network connections.

### Key Principles & Guarantees
- **Local Isolation:** Binds strictly to `localhost` (`127.0.0.1` / `::1`) by default on an unprivileged port (`8631`). Never exposes the emulator publicly.
- **Hardware Safety:** Uses a dedicated spool directory (`tools/printer-emulator/spool`). Print jobs are preserved as local test artifacts and are **never** forwarded to any physical printer.
- **Queue Isolation:** The CUPS configuration commands (`setup-cups` and `remove-cups`) manage only the emulator's temporary driverless queue. Pre-existing physical or production CUPS queues are never modified or removed.
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

1. **No Native SNMP Responder:**
   - `ippeveprinter` implements only the IPP protocol over HTTP/HTTPS.
   - It does **not** listen on UDP port 161 or respond to SNMP MIB queries (`HOST-RESOURCES-MIB`, `Printer-MIB`).
   - SNMP emulation is intentionally decoupled and reserved for **Task D2**.

2. **Accurate Hardware Page Counting (Odometers):**
   - Physical page counts and sensor odometers do not exist in software emulation.
   - Impressions and completed pages reflect client-supplied attributes or print processor signals.

3. **Hardware Fault Simulation:**
   - `ippeveprinter` starts in a clean `idle` state without physical faults.
   - Simulating faults (e.g. `media-jam`, `cover-open`, `media-empty`) requires either:
     - Configuring custom print processors emitting `STATE: +media-jam-error` on stderr, or
     - Modifying state configurations.
   - Dynamic external fault injection cannot be assumed out of the box.

4. **Document Format Advertising:**
   - By default, `ippeveprinter` only advertises raster formats (`image/pwg-raster`, `image/urf`, `application/octet-stream`).
   - To support direct PDF submission from CUPS without rasterization filters, `-f application/pdf,...` must be explicitly specified (configured by default in `emulator.sh`).

5. **Port Binding Restrictions:**
   - Non-root users cannot bind to standard IPP port 631.
   - Unprivileged ports (`> 1024`, e.g. `8631`) must be used.

---

## 9. Smoke Test Suite

A standalone, automated smoke test script is provided in `smoke_test.sh`:

```bash
./smoke_test.sh
```

### Test Coverage
- **Prerequisite Validation:** Checks `ippeveprinter`, `ipptool`, `lpstat`, `lpadmin`, `lp`, and running `cupsd`. (Reports `[SKIP]` with install instructions if missing).
- **Test 1:** Emulator startup and process lifecycle.
- **Test 2:** Direct IPP attribute discovery (`ipptool`).
- **Test 3:** Direct IPP print submission and lifecycle completion.
- **Test 4:** CUPS driverless queue registration (`lpadmin`).
- **Test 5:** End-to-end CUPS job dispatch and artifact receipt (`lp`).
- **Test 6:** CUPS IPP gateway capabilities query.
- **Test 7:** Safe cleanup and queue isolation (verifying no unrelated queues are modified).
