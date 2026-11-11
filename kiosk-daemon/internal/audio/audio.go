package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os/exec"
	"sync"
	"time"
)

// Player defines the interface for playing kiosk sound alerts.
type Player interface {
	PlayCompletion()
	WAVBytes() []byte
}

// SynthPlayer generates a clean PCM WAV chime in memory and plays it via system audio tools.
// If audio hardware or utilities are missing, it degrades gracefully without errors or crashes.
type SynthPlayer struct {
	enabled  bool
	wavData  []byte
	initOnce sync.Once
}

// NewPlayer creates a new sound player. If enabled is false, all playback calls are no-ops.
func NewPlayer(enabled bool) *SynthPlayer {
	p := &SynthPlayer{enabled: enabled}
	p.initWAV()
	return p
}

// initWAV generates a 2-tone chime: D5 (587.33 Hz) -> A5 (880.00 Hz) at 44.1 kHz 16-bit mono PCM.
func (p *SynthPlayer) initWAV() {
	p.initOnce.Do(func() {
		p.wavData = generateChimeWAV()
	})
}

// WAVBytes returns the raw synthesized WAV bytes for HTTP serving or local playback.
func (p *SynthPlayer) WAVBytes() []byte {
	p.initWAV()
	return p.wavData
}

// PlayCompletion triggers the completion chime asynchronously.
// If audio hardware is unavailable, it logs and returns without blocking or panicking.
func (p *SynthPlayer) PlayCompletion() {
	if !p.enabled {
		return
	}
	p.initWAV()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		if err := playAudio(ctx, p.wavData); err != nil {
			log.Printf("[AUDIO] Completion sound not played: %v (audio hardware/player unavailable, degrading gracefully)", err)
		}
	}()
}

// playAudio attempts playback using standard Linux/Pi audio utilities: aplay, paplay, ffplay.
func playAudio(ctx context.Context, data []byte) error {
	// 1. Try ALSA aplay (standard on Raspberry Pi OS)
	if aplayPath, err := exec.LookPath("aplay"); err == nil {
		cmd := exec.CommandContext(ctx, aplayPath, "-q", "-")
		cmd.Stdin = bytes.NewReader(data)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}

	// 2. Try PulseAudio paplay
	if paplayPath, err := exec.LookPath("paplay"); err == nil {
		cmd := exec.CommandContext(ctx, paplayPath)
		cmd.Stdin = bytes.NewReader(data)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}

	// 3. Try ffplay
	if ffplayPath, err := exec.LookPath("ffplay"); err == nil {
		cmd := exec.CommandContext(ctx, ffplayPath, "-nodisp", "-autoexit", "-loglevel", "quiet", "-")
		cmd.Stdin = bytes.NewReader(data)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}

	return fmt.Errorf("no functional audio player found (tried aplay, paplay, ffplay)")
}

// generateChimeWAV synthesizes a short, pleasant two-tone chime in standard RIFF WAV format.
func generateChimeWAV() []byte {
	const sampleRate = 44100
	tones := []struct {
		freq     float64
		duration float64
	}{
		{freq: 587.33, duration: 0.12}, // D5
		{freq: 880.00, duration: 0.24}, // A5
	}

	var pcm []int16
	for _, tone := range tones {
		sampleCount := int(float64(sampleRate) * tone.duration)
		for i := 0; i < sampleCount; i++ {
			t := float64(i) / float64(sampleRate)
			// Envelope: rapid attack, smooth exponential decay
			env := math.Exp(-5.0 * (float64(i) / float64(sampleCount)))
			val := int16(env * 0.45 * 32767.0 * math.Sin(2.0*math.Pi*tone.freq*t))
			pcm = append(pcm, val)
		}
	}

	dataSize := len(pcm) * 2
	fileSize := 36 + dataSize

	buf := new(bytes.Buffer)
	// RIFF header
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(fileSize))
	buf.WriteString("WAVE")

	// fmt subchunk
	buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))      // Subchunk1Size (16 for PCM)
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))       // AudioFormat (1 = PCM)
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))       // NumChannels (1 = Mono)
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate)) // SampleRate
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2)) // ByteRate (SampleRate * NumChannels * BitsPerSample/8)
	_ = binary.Write(buf, binary.LittleEndian, uint16(2))       // BlockAlign
	_ = binary.Write(buf, binary.LittleEndian, uint16(16))      // BitsPerSample

	// data subchunk
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	for _, sample := range pcm {
		_ = binary.Write(buf, binary.LittleEndian, sample)
	}

	return buf.Bytes()
}

// NoopPlayer is a silent sound player for testing or when sound is completely disabled.
type NoopPlayer struct{}

func (NoopPlayer) PlayCompletion() {}
func (NoopPlayer) WAVBytes() []byte { return nil }

