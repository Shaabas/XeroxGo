package input

import (
	"bufio"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
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

// MakeRaw configures a file descriptor (TTY) to raw mode so keystrokes arrive instantly
// without line buffering and without echoing to the screen.
func MakeRaw(fd int) (*syscall.Termios, error) {
	var old syscall.Termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&old))); err != 0 {
		return nil, err
	}
	raw := old
	raw.Lflag &^= (syscall.ICANON | syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL | syscall.ISIG | syscall.IEXTEN)
	raw.Iflag &^= (syscall.IXON | syscall.ICRNL | syscall.BRKINT | syscall.INPCK | syscall.ISTRIP)
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&raw))); err != 0 {
		return nil, err
	}
	return &old, nil
}

// Restore resets the terminal configuration saved by MakeRaw.
func Restore(fd int, old *syscall.Termios) {
	if old != nil {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(old)))
	}
}

// DeviceSource manages reading from a specific device or TTY path.
type DeviceSource struct {
	*ReaderSource
	file    *os.File
	oldTerm *syscall.Termios
	rawFd   int
}

// OpenTTYSource opens the TTY at path, puts it in raw mode, and starts a ReaderSource.
func OpenTTYSource(path string) (*DeviceSource, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		f, err = os.OpenFile(path, os.O_RDONLY, 0)
	}
	if err != nil {
		return nil, err
	}

	fd := int(f.Fd())
	oldTerm, _ := MakeRaw(fd)

	rs := NewReaderSource(f)
	return &DeviceSource{
		ReaderSource: rs,
		file:         f,
		oldTerm:      oldTerm,
		rawFd:        fd,
	}, nil
}

// Close terminates reading, restores terminal attributes, and closes the device file.
func (d *DeviceSource) Close() error {
	_ = d.ReaderSource.Close()
	if d.oldTerm != nil {
		Restore(d.rawFd, d.oldTerm)
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	return nil
}

