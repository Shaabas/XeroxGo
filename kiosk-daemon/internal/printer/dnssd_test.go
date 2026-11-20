package printer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseDNSSDURI(t *testing.T) {
	tests := []struct {
		name        string
		uri         string
		wantService string
		wantRegType string
		wantDomain  string
		wantUUID    string
		wantErr     bool
	}{
		{
			name:        "standard HP LaserJet Tank 1020w driverless URI",
			uri:         "dnssd://HP%20LaserJet%20Tank%201020w._ipp._tcp.local/?uuid=381v6a-12345",
			wantService: "HP LaserJet Tank 1020w",
			wantRegType: "_ipp._tcp",
			wantDomain:  "local",
			wantUUID:    "381v6a-12345",
			wantErr:     false,
		},
		{
			name:        "secure IPPS scheme with complex name",
			uri:         "dnssd://Lab%20Printer%20(Room%20101)._ipps._tcp.local/",
			wantService: "Lab Printer (Room 101)",
			wantRegType: "_ipps._tcp",
			wantDomain:  "local",
			wantUUID:    "",
			wantErr:     false,
		},
		{
			name:    "non-dnssd scheme rejected",
			uri:     "ipp://192.168.1.50:631/ipp/print",
			wantErr: true,
		},
		{
			name:    "empty URI rejected",
			uri:     "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, err := ParseDNSSDURI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for URI %q, got nil", tc.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info.ServiceName != tc.wantService {
				t.Errorf("ServiceName = %q, want %q", info.ServiceName, tc.wantService)
			}
			if info.RegType != tc.wantRegType {
				t.Errorf("RegType = %q, want %q", info.RegType, tc.wantRegType)
			}
			if info.Domain != tc.wantDomain {
				t.Errorf("Domain = %q, want %q", info.Domain, tc.wantDomain)
			}
			if info.UUID != tc.wantUUID {
				t.Errorf("UUID = %q, want %q", info.UUID, tc.wantUUID)
			}
		})
	}
}

func TestResolveDNSSDTarget_SuccessViaIPPFind(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ippfind" {
			return []byte("ipp://192.168.1.50:631/ipp/print\n"), nil
		}
		return nil, errors.New("command not found")
	}

	rawURI := "dnssd://HP%20LaserJet%20Tank%201020w._ipp._tcp.local/?uuid=381v6a"
	host, port, path, err := ResolveDNSSDTarget(context.Background(), runner, rawURI)
	if err != nil {
		t.Fatalf("unexpected error resolving DNS-SD target: %v", err)
	}

	if host != "192.168.1.50" {
		t.Errorf("expected host 192.168.1.50, got %q", host)
	}
	if port != 631 {
		t.Errorf("expected port 631, got %d", port)
	}
	if path != "/ipp/print" {
		t.Errorf("expected path /ipp/print, got %q", path)
	}
}

func TestResolveDNSSDTarget_SuccessViaAvahi(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ippfind" {
			return nil, errors.New("ippfind: no matching service")
		}
		if name == "avahi-resolve" {
			return []byte("HPLaserJet.local   192.168.1.65\n"), nil
		}
		return nil, errors.New("command not found")
	}

	rawURI := "dnssd://HPLaserJet._ipp._tcp.local/"
	host, port, _, err := ResolveDNSSDTarget(context.Background(), runner, rawURI)
	if err != nil {
		t.Fatalf("unexpected error resolving DNS-SD target via avahi: %v", err)
	}

	if host != "192.168.1.65" {
		t.Errorf("expected host 192.168.1.65, got %q", host)
	}
	if port != 631 {
		t.Errorf("expected port 631, got %d", port)
	}
}

func TestResolveDNSSDTarget_FailureNeverGuessesIP(t *testing.T) {
	// Both ippfind and avahi fail
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("service resolution failed")
	}

	rawURI := "dnssd://HP%20LaserJet%20Tank%201020w._ipp._tcp.local/"
	host, port, path, err := ResolveDNSSDTarget(context.Background(), runner, rawURI)
	if err == nil {
		t.Fatalf("expected error when DNS-SD resolution fails, got nil (host=%q)", host)
	}

	// Must never invent or guess an IP address
	if host != "" || port != 0 || path != "" {
		t.Errorf("host, port, path must be empty on failure, got host=%q, port=%d, path=%q", host, port, path)
	}

	if !strings.Contains(err.Error(), "could not be resolved automatically") {
		t.Errorf("unexpected error message: %v", err)
	}
}
