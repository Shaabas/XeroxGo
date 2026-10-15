package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultHTTPAddr is the fallback HTTP server address when HTTP_ADDR is unset.
const DefaultHTTPAddr = "127.0.0.1:8080"

const defaultHTTPAddr = DefaultHTTPAddr

// Config contains the cloud backend runtime configuration.
type Config struct {
	HTTPAddr string
}

// LookupEnv has the same signature as os.LookupEnv.
type LookupEnv func(string) (string, bool)

// Load builds configuration from the environment.
func Load(lookupEnv LookupEnv) (Config, error) {
	if lookupEnv == nil {
		return Config{}, fmt.Errorf("environment lookup function must not be nil")
	}

	addr, supplied := lookupEnv("HTTP_ADDR")
	if !supplied {
		return Config{HTTPAddr: defaultHTTPAddr}, nil
	}

	if err := validateHTTPAddr(addr); err != nil {
		return Config{}, fmt.Errorf("invalid HTTP_ADDR: %w", err)
	}

	return Config{HTTPAddr: addr}, nil
}

func validateHTTPAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("value must not be empty")
	}
	if strings.TrimSpace(addr) != addr {
		return fmt.Errorf("leading or trailing whitespace is not allowed")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("expected a host:port address: %w", err)
	}

	if strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("host must not contain whitespace")
	}
	if port == "" {
		return fmt.Errorf("port must not be empty")
	}
	for _, char := range port {
		if char < '0' || char > '9' {
			return fmt.Errorf("port must contain decimal digits only")
		}
	}

	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	return nil
}
