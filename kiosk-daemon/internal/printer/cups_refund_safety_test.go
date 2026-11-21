package printer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCancelAndVerifyCUPSJob_RequiresTerminalState(t *testing.T) {
	cases := []struct{ name, status string; failCancel, expectOK bool }{
		{"canceled","canceled",false,true},
		{"aborted","aborted",false,true},
		{"processing","processing",false,false},
		{"held","pending-held",false,false},
		{"unknown","unknown",false,false},
		{"cancel-command-failed","canceled",true,false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "cancel" {
					if len(args) != 1 || args[0] != "queue-45" { t.Errorf("wrong cancel arguments: %+v", args) }
					if tc.failCancel { return []byte("CUPS unreachable"), errors.New("failed") }
					return nil, nil
				}
				if name == "ipptool" {
					return []byte("job-id (integer) = 45\njob-state (enum) = " + tc.status + "\n"), nil
				}
				if name == "lpstat" { return []byte(""), errors.New("unknown") }
				t.Errorf("unexpected command %q (%s)", name, strings.Join(args, " "))
				return nil, errors.New("unexpected command")
			}
			err := CancelAndVerifyCUPSJob(ctx, runner, "queue", "queue-45")
			if (err == nil) != tc.expectOK {
				t.Fatalf("status %s expected ok %v; got %v", tc.status, tc.expectOK, err)
			}
		})
	}
}
