package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFingerprintCLI_FlagValidation(t *testing.T) {
	// Build the CLI binary in a temporary directory for testing
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "printer-fingerprint")

	cmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build fingerprint CLI: %v\n%s", err, string(out))
	}

	t.Run("fails when no target flags provided", func(t *testing.T) {
		runCmd := exec.Command(binPath)
		out, err := runCmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected failure when no flags provided, got success\n%s", string(out))
		}
		if !strings.Contains(string(out), "at least one target must be specified") {
			t.Errorf("expected error message about target flags, got:\n%s", string(out))
		}
	})

	t.Run("displays help and exits with 0", func(t *testing.T) {
		runCmd := exec.Command(binPath, "--help")
		out, err := runCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected success with --help, got error: %v\n%s", err, string(out))
		}
		if !strings.Contains(string(out), "XeroxGo Printer Capability Fingerprint CLI") {
			t.Errorf("expected usage title in help output, got:\n%s", string(out))
		}
	})

	t.Run("writes JSON output file when requested", func(t *testing.T) {
		outFile := filepath.Join(tmpDir, "test_report.json")
		// Use loopback non-existent port to test fast failure with file generation
		runCmd := exec.Command(binPath, "-s", "127.0.0.1", "-p", "59999", "-snmp-timeout", "100ms", "-o", outFile, "-format", "json")
		_ = runCmd.Run() // command may fail SNMP probe, but report must still be written

		data, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatalf("failed to read generated report file: %v", err)
		}
		if !strings.Contains(string(data), `"targets"`) {
			t.Errorf("output file does not contain valid report JSON:\n%s", string(data))
		}
	})
}
