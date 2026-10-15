package config

import "testing"

func lookup(value string, present bool) LookupEnv {
	return func(key string) (string, bool) {
		if key != "HTTP_ADDR" {
			return "", false
		}
		return value, present
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(lookup("", false))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTPAddr != DefaultHTTPAddr {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, DefaultHTTPAddr)
	}
}

func TestLoadAcceptsValidAddresses(t *testing.T) {
	tests := []string{
		"127.0.0.1:8080",
		"localhost:8080",
		":8080",
		"[::1]:8080",
		"0.0.0.0:65535",
	}

	for _, addr := range tests {
		t.Run(addr, func(t *testing.T) {
			cfg, err := Load(lookup(addr, true))
			if err != nil {
				t.Fatalf("Load(%q) error = %v", addr, err)
			}

			if cfg.HTTPAddr != addr {
				t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, addr)
			}
		})
	}
}

func TestLoadRejectsInvalidAddresses(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{name: "empty", addr: ""},
		{name: "whitespace only", addr: "   "},
		{name: "missing port", addr: "localhost"},
		{name: "empty port", addr: "localhost:"},
		{name: "non-numeric port", addr: "localhost:http"},
		{name: "port with symbols", addr: "localhost:80a"},
		{name: "port with internal space", addr: "localhost:80 80"},
		{name: "host with internal space", addr: "local host:8080"},
		{name: "zero port", addr: "localhost:0"},
		{name: "port too large", addr: "localhost:65536"},
		{name: "negative port", addr: "localhost:-1"},
		{name: "leading whitespace", addr: " localhost:8080"},
		{name: "trailing whitespace", addr: "localhost:8080 "},
		{name: "IPv6 missing brackets", addr: "::1:8080"},
		{name: "IPv6 missing port", addr: "[::1]"},
		{name: "IPv6 empty port", addr: "[::1]:"},
		{name: "multiple colons without brackets", addr: "127.0.0.1:8080:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(lookup(tt.addr, true))
			if err == nil {
				t.Fatalf("Load(%q) expected an error", tt.addr)
			}
		})
	}
}

func TestLoadRejectsNilLookup(t *testing.T) {
	_, err := Load(nil)
	if err == nil {
		t.Fatal("Load(nil) expected an error")
	}
}
