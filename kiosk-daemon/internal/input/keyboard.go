package input

import (
	"bufio"
	"io"
	"sync"
	"time"
)

// ReaderSource reads user input from an io.Reader (such as os.Stdin or a terminal buffer)
// and maps keystrokes to standard PIN keypad actions.
type ReaderSource struct {
	reader io.Reader
	events chan Event
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

// NewReaderSource creates and starts a ReaderSource that reads from r.
func NewReaderSource(r io.Reader) *ReaderSource {
	s := &ReaderSource{
		reader: r,
		events: make(chan Event, 64),
		done:   make(chan struct{}),
	}
	go s.loop()
	return s
}

// Events returns the read-only event channel.
func (s *ReaderSource) Events() <-chan Event {
	return s.events
}

func (s *ReaderSource) loop() {
	defer close(s.events)
	br := bufio.NewReader(s.reader)

	for {
		r, _, err := br.ReadRune()
		if err != nil {
			return
		}

		select {
		case <-s.done:
			return
		default:
		}

		now := time.Now().UTC()
		var ev *Event

		switch {
		case r >= '0' && r <= '9':
			ev = &Event{Type: ActionDigit, Digit: r, Timestamp: now}
		case r == '\b' || r == 0x7f: // Backspace or DEL
			ev = &Event{Type: ActionBackspace, Timestamp: now}
		case r == 'c' || r == 'C': // Clear
			ev = &Event{Type: ActionClear, Timestamp: now}
		case r == '\r' || r == '\n': // Enter/Return -> Submit
			ev = &Event{Type: ActionSubmit, Timestamp: now}
		case r == 0x1b || r == 'x' || r == 'X' || r == 'q' || r == 'Q': // ESC or X/Q -> Cancel
			ev = &Event{Type: ActionCancel, Timestamp: now}
		default:
			// Ignore unrecognized keystrokes
			continue
		}

		if ev != nil {
			select {
			case s.events <- *ev:
			case <-s.done:
				return
			}
		}
	}
}

// Close terminates reading and closes channels.
func (s *ReaderSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

