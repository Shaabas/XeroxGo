package printer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// DNSSDServiceInfo encapsulates parsed components of a CUPS dnssd:// URI.
type DNSSDServiceInfo struct {
	RawURI      string `json:"raw_uri"`
	ServiceName string `json:"service_name"`
	RegType     string `json:"reg_type"` // e.g. "_ipp._tcp" or "_ipps._tcp"
	Domain      string `json:"domain"`   // e.g. "local"
	UUID        string `json:"uuid,omitempty"`
}

// ResolvedEndpoint contains discovered network endpoint parameters from DNS-SD.
type ResolvedEndpoint struct {
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Path     string `json:"path,omitempty"` // empty if unverified by discovery query
	ExactURI string `json:"exact_uri,omitempty"`
}

// ParseDNSSDURI parses a CUPS driverless device URI with the dnssd:// scheme.
// Format example: dnssd://HP%20LaserJet%20Tank%201020w._ipp._tcp.local/?uuid=381v6a
func ParseDNSSDURI(rawURI string) (*DNSSDServiceInfo, error) {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return nil, errors.New("empty device URI")
	}

	lower := strings.ToLower(rawURI)
	if !strings.HasPrefix(lower, "dnssd://") {
		return nil, fmt.Errorf("not a dnssd URI scheme: %q", rawURI)
	}

	rest := rawURI[len("dnssd://"):]
	// Strip query string if present
	var queryStr string
	if idx := strings.Index(rest, "?"); idx != -1 {
		queryStr = rest[idx+1:]
		rest = rest[:idx]
	}

	// Strip trailing slashes
	rest = strings.TrimRight(rest, "/")

	// Unescape the service name component (handles percent-encoded spaces and UTF-8)
	unescapedHost, err := url.PathUnescape(rest)
	if err != nil {
		unescapedHost = rest
	}

	info := &DNSSDServiceInfo{
		RawURI: rawURI,
	}

	if queryStr != "" {
		if q, err := url.ParseQuery(queryStr); err == nil {
			info.UUID = q.Get("uuid")
		}
	}

	// Split by dots from the right to extract domain and regtype
	// Standard DNS-SD name: <Instance>.<ServiceType>.<Domain>
	// where ServiceType is typically _ipp._tcp or _ipps._tcp (2 labels)
	labels := strings.Split(unescapedHost, ".")
	if len(labels) < 4 {
		// Not enough labels for standard DNS-SD instance
		info.ServiceName = unescapedHost
		return info, nil
	}

	// Last label is domain (e.g. "local")
	info.Domain = labels[len(labels)-1]

	// Next two labels are transport and application protocol (e.g. "_tcp", "_ipp")
	proto1 := labels[len(labels)-2]
	proto2 := labels[len(labels)-3]
	info.RegType = proto2 + "." + proto1

	// Remaining prefix is the instance/service name
	instanceLabels := labels[:len(labels)-3]
	info.ServiceName = strings.Join(instanceLabels, ".")

	return info, nil
}

// ResolveDNSSDTarget attempts best-effort resolution of a dnssd:// device URI
// to a concrete host/IP, port, and IPP resource path.
// It uses CUPS ippfind or standard mDNS resolution commands without ever guessing an address.
// It preserves secure ipps:// schemes and NEVER manufactures resource paths without evidence.
func ResolveDNSSDTarget(ctx context.Context, runner CommandRunner, rawURI string) (ResolvedEndpoint, error) {
	info, err := ParseDNSSDURI(rawURI)
	if err != nil {
		return ResolvedEndpoint{}, err
	}

	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		ctx = context.Background()
	}

	defaultScheme := "ipp"
	defaultPort := 631
	if strings.Contains(strings.ToLower(info.RegType), "_ipps.") {
		defaultScheme = "ipps"
		defaultPort = 443
	}

	// Attempt 1: Use CUPS ippfind
	// ippfind searches and resolves DNS-SD registered IPP printers directly into ipp:// or ipps:// URIs
	if out, err := runner(ctx, "ippfind", "-T", "2", info.ServiceName); err == nil {
		if resolvedURI := parseResolvedIPPURI(string(out)); resolvedURI != nil {
			scheme := strings.ToLower(resolvedURI.Scheme)
			if scheme == "" {
				scheme = defaultScheme
			}
			return ResolvedEndpoint{
				Scheme:   scheme,
				Host:     resolvedURI.Hostname(),
				Port:     parsePortOrDefault(resolvedURI.Port(), defaultPort),
				Path:     resolvedURI.RequestURI(),
				ExactURI: resolvedURI.String(),
			}, nil
		}
	}

	// Also try ippfind without instance filter in case instance name is slightly formatted
	if out, err := runner(ctx, "ippfind", "-T", "2"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			if resolvedURI := parseResolvedIPPURI(l); resolvedURI != nil {
				if strings.Contains(strings.ToLower(l), strings.ToLower(info.ServiceName)) {
					scheme := strings.ToLower(resolvedURI.Scheme)
					if scheme == "" {
						scheme = defaultScheme
					}
					return ResolvedEndpoint{
						Scheme:   scheme,
						Host:     resolvedURI.Hostname(),
						Port:     parsePortOrDefault(resolvedURI.Port(), defaultPort),
						Path:     resolvedURI.RequestURI(),
						ExactURI: resolvedURI.String(),
					}, nil
				}
			}
		}
	}

	// Attempt 2: Use avahi-resolve for .local hostname resolution if service name has host form
	hostCandidate := info.ServiceName
	if !strings.HasSuffix(strings.ToLower(hostCandidate), ".local") {
		hostCandidate = hostCandidate + ".local"
	}
	if out, err := runner(ctx, "avahi-resolve", "-n", hostCandidate); err == nil {
		parts := strings.Fields(string(out))
		if len(parts) >= 2 {
			ip := parts[1]
			if ip != "" {
				// NOTE: avahi-resolve resolves only the host/IP.
				// Do NOT manufacture a resource path like "/ipp/print" without evidence.
				return ResolvedEndpoint{
					Scheme:   defaultScheme,
					Host:     ip,
					Port:     defaultPort,
					Path:     "", // unverified resource path left empty
					ExactURI: fmt.Sprintf("%s://%s:%d", defaultScheme, ip, defaultPort),
				}, nil
			}
		}
	}

	// Unresolved: do not guess!
	return ResolvedEndpoint{}, fmt.Errorf("DNS-SD target %q (service %q) could not be resolved automatically. Provide explicit --ipp-uri or --snmp-host override", rawURI, info.ServiceName)
}

func parseResolvedIPPURI(raw string) *url.URL {
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ipp://") || strings.HasPrefix(line, "ipps://") || strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			if u, err := url.Parse(line); err == nil && u.Hostname() != "" {
				return u
			}
		}
	}
	return nil
}

func parsePortOrDefault(portStr string, defaultPort int) int {
	if portStr == "" {
		return defaultPort
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return defaultPort
	}
	return p
}
