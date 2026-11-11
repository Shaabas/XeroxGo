package input

import (
	"sync"
	"time"
)

// SimulatedSource allows programmatic injection of input events for automated tests and simulators.
type SimulatedSource struct {
	events chan Event
	mu     sync.Mutex
	closed bool
}

// NewSimulatedSource creates a new SimulatedSource with a buffered channel.
func NewSimulatedSource(bufferSize int) *SimulatedSource {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	return &SimulatedSource{
		events: make(chan Event, bufferSize),
	}
}

// Events returns the read-only event channel.
func (s *SimulatedSource) Events() <-chan Event {
	return s.events
}

// Emit sends an event to the events channel if not closed.
func (s *SimulatedSource) Emit(ev Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	select {
	case s.events <- ev:
		return true
	default:
		return false
	}
}

// EmitDigit emits a single numeric digit event ('0'..'9').
func (s *SimulatedSource) EmitDigit(d rune) bool {
	if d < '0' || d > '9' {
		return false
	}
	return s.Emit(Event{
		Type:      ActionDigit,
		Digit:     d,
		Timestamp: time.Now().UTC(),
	})
}

// EmitDigits emits multiple digit events in sequence.
func (s *SimulatedSource) EmitDigits(digits string) int {
	emitted := 0
	for _, r := range digits {
		if s.EmitDigit(r) {
			emitted++
		}
	}
	return emitted
}

// EmitBackspace emits a backspace action.
func (s *SimulatedSource) EmitBackspace() bool {
	return s.Emit(Event{
		Type:      ActionBackspace,
		Timestamp: time.Now().UTC(),
	})
}

// EmitClear emits a clear-all action.
func (s *SimulatedSource) EmitClear() bool {
	return s.Emit(Event{
		Type:      ActionClear,
		Timestamp: time.Now().UTC(),
	})
}

// EmitSubmit emits a submit action.
func (s *SimulatedSource) EmitSubmit() bool {
	return s.Emit(Event{
		Type:      ActionSubmit,
		Timestamp: time.Now().UTC(),
	})
}

// EmitCancel emits a cancel action.
func (s *SimulatedSource) EmitCancel() bool {
	return s.Emit(Event{
		Type:      ActionCancel,
		Timestamp: time.Now().UTC(),
	})
}

// Close closes the underlying events channel.
func (s *SimulatedSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	return nil
}

