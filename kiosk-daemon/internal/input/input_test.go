package input

import (
	"strings"
	"testing"
	"time"
)

func TestSimulatedSource_Events(t *testing.T) {
	sim := NewSimulatedSource(10)
	defer sim.Close()

	if !sim.EmitDigit('5') {
		t.Fatalf("expected EmitDigit to succeed")
	}
	if sim.EmitDigit('a') {
		t.Fatalf("expected non-digit 'a' to be rejected")
	}

	n := sim.EmitDigits("1234")
	if n != 4 {
		t.Fatalf("expected 4 digits emitted, got %d", n)
	}

	sim.EmitBackspace()
	sim.EmitClear()
	sim.EmitSubmit()
	sim.EmitCancel()

	expectedTypes := []ActionType{
		ActionDigit,
		ActionDigit,
		ActionDigit,
		ActionDigit,
		ActionDigit,
		ActionBackspace,
		ActionClear,
		ActionSubmit,
		ActionCancel,
	}

	for i, expected := range expectedTypes {
		select {
		case ev := <-sim.Events():
			if ev.Type != expected {
				t.Errorf("step %d: expected event type %s, got %s", i, expected, ev.Type)
			}
			if expected == ActionDigit && i == 0 && ev.Digit != '5' {
				t.Errorf("step 0: expected digit '5', got %c", ev.Digit)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("timeout waiting for event %d (%s)", i, expected)
		}
	}
}

func TestReaderSource_Mapping(t *testing.T) {
	// Simulate keyboard stream: "45\b9c\r\x1b\x1bOM"
	// '4' -> Digit
	// '5' -> Digit
	// '\b' -> Backspace
	// '9' -> Digit
	// 'c' -> Clear
	// '\r' -> Submit
	// '\x1b' -> Cancel
	// '\x1bOM' -> Keypad Submit
	inputData := "45\b9c\r\x1b\x1bOM"
	reader := strings.NewReader(inputData)
	src := NewReaderSource(reader)
	defer src.Close()

	expectedActions := []ActionType{
		ActionDigit,
		ActionDigit,
		ActionBackspace,
		ActionDigit,
		ActionClear,
		ActionSubmit,
		ActionCancel,
		ActionSubmit,
	}

	for i, exp := range expectedActions {
		select {
		case ev, ok := <-src.Events():
			if !ok {
				t.Fatalf("step %d: channel closed unexpectedly", i)
			}
			if ev.Type != exp {
				t.Errorf("step %d: expected action %s, got %s", i, exp, ev.Type)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timeout waiting for action %d (%s)", i, exp)
		}
	}
}

