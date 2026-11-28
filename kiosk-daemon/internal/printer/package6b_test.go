package printer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCancelAndVerifyCUPSJob_RequiresAuthoritativeStopAndEmptyQueue(t *testing.T) {
	ctx := context.Background()

	t.Run("confirmed canceled and empty queue", func(t *testing.T) {
		runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "cancel":
				return []byte(""), nil
			case "ipptool":
				return []byte("job-state (enum) = canceled\n"), nil
			case "lpstat":
				if len(args) >= 2 && args[0] == "-W" && args[1] == "not-completed" {
					return []byte(""), nil
				}
				return []byte(""), nil
			default:
				return nil, errors.New("unexpected command")
			}
		}

		res := CancelAndVerifyCUPSJob(ctx, runner, "XeroxQueue", "XeroxQueue-101")
		if !res.ConfirmedStopped || res.StillActive || res.AlreadyCompleted || res.IsUnknown {
			t.Fatalf("unexpected CancelResult: %+v", res)
		}
	})

	t.Run("disappearance alone is insufficient", func(t *testing.T) {
		runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "cancel":
				return []byte(""), nil
			case "ipptool":
				return []byte("client-error-not-found"), errors.New("exit status 1")
			case "lpstat":
				return []byte(""), nil
			default:
				return nil, errors.New("unexpected command")
			}
		}

		res := CancelAndVerifyCUPSJob(ctx, runner, "XeroxQueue", "XeroxQueue-102")
		if res.ConfirmedStopped || !res.IsUnknown {
			t.Fatalf("disappearance alone must not confirm stop: %+v", res)
		}
	})

	t.Run("unrelated active job on queue fails verification", func(t *testing.T) {
		runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "cancel":
				return []byte(""), nil
			case "ipptool":
				return []byte("job-state (enum) = canceled\n"), nil
			case "lpstat":
				return []byte("XeroxQueue-999 user 1024 Mon\n"), nil
			default:
				return nil, errors.New("unexpected command")
			}
		}

		res := CancelAndVerifyCUPSJob(ctx, runner, "XeroxQueue", "XeroxQueue-103")
		if res.ConfirmedStopped || !res.StillActive || res.QueueCheckErr == nil {
			t.Fatalf("expected active queue check failure to block ConfirmedStopped: %+v", res)
		}
	})

	t.Run("still processing after cancel", func(t *testing.T) {
		runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "cancel":
				return []byte("cancel failed"), errors.New("exit status 1")
			case "ipptool":
				return []byte("job-state (enum) = processing\n"), nil
			case "lpstat":
				return []byte("XeroxQueue-104 user 1024 Mon\n"), nil
			default:
				return nil, errors.New("unexpected command")
			}
		}

		res := CancelAndVerifyCUPSJob(ctx, runner, "XeroxQueue", "XeroxQueue-104")
		if res.ConfirmedStopped || !res.StillActive {
			t.Fatalf("expected StillActive=true, ConfirmedStopped=false: %+v", res)
		}
	})

	t.Run("already completed before cancel", func(t *testing.T) {
		runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "cancel":
				return []byte("already completed"), errors.New("exit status 1")
			case "ipptool":
				return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\n"), nil
			case "lpstat":
				return []byte(""), nil
			default:
				return nil, errors.New("unexpected command")
			}
		}

		res := CancelAndVerifyCUPSJob(ctx, runner, "XeroxQueue", "XeroxQueue-105")
		if res.ConfirmedStopped || !res.AlreadyCompleted {
			t.Fatalf("expected AlreadyCompleted=true, ConfirmedStopped=false: %+v", res)
		}
	})

	_ = strings.TrimSpace("")
}
