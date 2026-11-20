package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
)

func main() {
	var (
		queueName     string
		remoteIPPURI  string
		snmpHost      string
		snmpPort      int
		snmpCommunity string
		snmpTimeout   time.Duration
		snmpRetries   int
		outputPath    string
		format        string
		redact        bool
		showHelp      bool
	)

	flag.StringVar(&queueName, "queue", "", "CUPS queue name to probe (e.g. 'HPTank')")
	flag.StringVar(&queueName, "q", "", "CUPS queue name (shorthand)")
	flag.StringVar(&remoteIPPURI, "ipp-uri", "", "Direct remote IPP URI (e.g. 'ipp://192.168.1.50/ipp/print')")
	flag.StringVar(&remoteIPPURI, "u", "", "Direct remote IPP URI (shorthand)")
	flag.StringVar(&snmpHost, "snmp-host", "", "SNMP target host/IP (defaults to resolving from queue/IPP if omitted)")
	flag.StringVar(&snmpHost, "s", "", "SNMP target host/IP (shorthand)")
	flag.IntVar(&snmpPort, "snmp-port", 161, "SNMP UDP port")
	flag.IntVar(&snmpPort, "p", 161, "SNMP UDP port (shorthand)")
	flag.StringVar(&snmpCommunity, "snmp-community", "public", "SNMPv2c read community string")
	flag.StringVar(&snmpCommunity, "c", "public", "SNMPv2c read community string (shorthand)")
	flag.DurationVar(&snmpTimeout, "snmp-timeout", 3*time.Second, "SNMP request timeout duration")
	flag.IntVar(&snmpRetries, "snmp-retries", 1, "SNMP retry count")
	flag.StringVar(&outputPath, "output", "", "Path to write structured JSON report")
	flag.StringVar(&outputPath, "o", "", "Path to write structured JSON report (shorthand)")
	flag.StringVar(&format, "format", "human", "Output format to stdout: 'human', 'json', or 'both'")
	flag.BoolVar(&redact, "redact", true, "Redact IP addresses, serial numbers, MAC addresses, and credentials in output")
	flag.BoolVar(&showHelp, "help", false, "Show help and usage information")
	flag.BoolVar(&showHelp, "h", false, "Show help and usage information (shorthand)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "XeroxGo Printer Capability Fingerprint CLI\n")
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  # Probe local CUPS queue and auto-resolve remote IPP/SNMP:\n")
		fmt.Fprintf(os.Stderr, "  %s -q HPTank\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Probe direct remote IPP and SNMP target with JSON output:\n")
		fmt.Fprintf(os.Stderr, "  %s -u ipp://192.168.1.50/ipp/print -s 192.168.1.50 -o report.json\n\n", os.Args[0])
	}

	flag.Parse()

	if showHelp {
		flag.Usage()
		os.Exit(0)
	}

	queueName = strings.TrimSpace(queueName)
	remoteIPPURI = strings.TrimSpace(remoteIPPURI)
	snmpHost = strings.TrimSpace(snmpHost)

	if queueName == "" && remoteIPPURI == "" && snmpHost == "" {
		fmt.Fprintf(os.Stderr, "Error: at least one target must be specified: -queue, -ipp-uri, or -snmp-host.\n\n")
		flag.Usage()
		os.Exit(1)
	}

	cfg := printer.FingerprintConfig{
		QueueName:     queueName,
		RemoteIPPURI:  remoteIPPURI,
		SNMPHost:      snmpHost,
		SNMPPort:      snmpPort,
		SNMPCommunity: snmpCommunity,
		SNMPTimeout:   snmpTimeout,
		SNMPRetries:   snmpRetries,
		Redact:        redact,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	snmpFactory := func(snmpCfg printer.SNMPConfig) (printer.DynamicSNMPSession, error) {
		return printer.NewRealDynamicSNMPSession(ctx, snmpCfg)
	}

	report, err := printer.RunFingerprint(ctx, nil, snmpFactory, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fingerprint execution error: %v\n", err)
	}

	if report == nil {
		fmt.Fprintf(os.Stderr, "Fatal: failed to generate capability report\n")
		os.Exit(1)
	}

	jsonBytes, err := printer.FormatJSON(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error formatting JSON: %v\n", err)
		os.Exit(1)
	}

	// Write JSON to file if requested
	if strings.TrimSpace(outputPath) != "" {
		outPath := strings.TrimSpace(outputPath)
		dir := filepath.Dir(outPath)
		if dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0755)
		}
		if writeErr := os.WriteFile(outPath, jsonBytes, 0644); writeErr != nil {
			fmt.Fprintf(os.Stderr, "Error writing report file %q: %v\n", outPath, writeErr)
		} else {
			fmt.Fprintf(os.Stderr, "Saved structured JSON report to %s\n\n", outPath)
		}
	}

	// Output to stdout
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		fmt.Println(string(jsonBytes))
	case "both":
		fmt.Print(printer.FormatHumanReadable(report))
		fmt.Println("\n--- JSON REPORT ---")
		fmt.Println(string(jsonBytes))
	default: // "human"
		fmt.Print(printer.FormatHumanReadable(report))
	}
}
