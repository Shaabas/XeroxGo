package audio

import (
	"bytes"
	"testing"
)

func TestGenerateChimeWAV(t *testing.T) {
	data := generateChimeWAV()
	if len(data) < 44 {
		t.Fatalf("WAV data too short: %d bytes", len(data))
	}

	// Verify RIFF header
	if !bytes.HasPrefix(data, []byte("RIFF")) {
		t.Errorf("Missing RIFF header")
	}
	if string(data[8:12]) != "WAVE" {
		t.Errorf("Missing WAVE identifier, got %q", string(data[8:12]))
	}
	if string(data[12:16]) != "fmt " {
		t.Errorf("Missing fmt chunk, got %q", string(data[12:16]))
	}

	// Check data chunk exists
	if !bytes.Contains(data, []byte("data")) {
		t.Errorf("Missing data chunk")
	}
}

func TestSynthPlayer_Disabled(t *testing.T) {
	p := NewPlayer(false)
	// Should not panic or block
	p.PlayCompletion()

	data := p.WAVBytes()
	if len(data) == 0 {
		t.Errorf("Expected WAV bytes to still be generated for HTTP serving even when local playback is disabled")
	}
}

func TestSynthPlayer_GracefulDegrade(t *testing.T) {
	p := NewPlayer(true)
	// Even if sound card is busy or missing, PlayCompletion must not block or panic
	p.PlayCompletion()
}

func TestNoopPlayer(t *testing.T) {
	np := NoopPlayer{}
	np.PlayCompletion()
	if np.WAVBytes() != nil {
		t.Errorf("expected nil from NoopPlayer.WAVBytes")
	}
}

