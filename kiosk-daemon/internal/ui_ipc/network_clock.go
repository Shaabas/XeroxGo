package ui_ipc

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

var (
	clockMu     sync.RWMutex
	clockOffset time.Duration
	clockSynced bool

	// Default NTP servers (UDP port 123)
	defaultNTPServers = []string{
		"pool.ntp.org:123",
		"time.google.com:123",
		"time.cloudflare.com:123",
	}
)

// fetchNTP queries an NTP server via UDP and returns the network time.
func fetchNTP(server string) (time.Time, error) {
	conn, err := net.DialTimeout("udp", server, 3*time.Second)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return time.Time{}, err
	}

	// Standard 48-byte NTP request packet with LI=0, VN=3, Mode=3 (client): 0x1B
	req := make([]byte, 48)
	req[0] = 0x1B

	if _, err := conn.Write(req); err != nil {
		return time.Time{}, err
	}

	resp := make([]byte, 48)
	if _, err := conn.Read(resp); err != nil {
		return time.Time{}, err
	}

	// Transmit timestamp seconds are in bytes 40..43
	secs := binary.BigEndian.Uint32(resp[40:44])
	if secs == 0 {
		return time.Time{}, fmt.Errorf("empty NTP timestamp returned")
	}

	// Convert NTP epoch (1900-01-01) to Unix epoch (1970-01-01): 2,208,988,800 seconds
	unixSecs := int64(secs) - 2208988800
	return time.Unix(unixSecs, 0), nil
}

// SyncNetworkClock attempts to synchronize the network clock against the NTP pool.
func SyncNetworkClock() bool {
	for _, srv := range defaultNTPServers {
		t, err := fetchNTP(srv)
		if err == nil {
			clockMu.Lock()
			clockOffset = t.Sub(time.Now())
			clockSynced = true
			clockMu.Unlock()
			log.Printf("[CLOCK] Synced network time from %s: %s (offset: %v)", srv, t.Format(time.RFC3339), clockOffset)
			return true
		}
	}
	log.Printf("[CLOCK] Network time sync failed across all NTP servers; will retry")
	return false
}

// StartNetworkClockLoop runs a background worker that keeps the clock synced.
func StartNetworkClockLoop(ctx context.Context) {
	go func() {
		synced := SyncNetworkClock()
		retryInterval := 10 * time.Second
		if synced {
			retryInterval = 30 * time.Minute
		}

		ticker := time.NewTicker(retryInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if SyncNetworkClock() {
					ticker.Reset(30 * time.Minute)
				} else {
					ticker.Reset(15 * time.Second)
				}
			}
		}
	}()
}

// NetworkNow returns the current local time corrected by the network offset.
func NetworkNow() time.Time {
	clockMu.RLock()
	defer clockMu.RUnlock()

	now := time.Now()
	if clockSynced {
		return now.Add(clockOffset)
	}
	return now
}

// SetNetworkTimeOffset overrides the network clock offset (useful for deterministic tests).
func SetNetworkTimeOffset(offset time.Duration) {
	clockMu.Lock()
	defer clockMu.Unlock()
	clockOffset = offset
	clockSynced = true
}

// SetNetworkTime sets a specific network time anchor.
func SetNetworkTime(t time.Time) {
	SetNetworkTimeOffset(t.Sub(time.Now()))
}
