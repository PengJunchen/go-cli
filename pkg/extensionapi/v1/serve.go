package extensionapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Serve runs the bounded v1 JSONL worker. Calls are serialized; each deadline is
// passed to the handler. The host terminates an uncooperative worker on timeout.
// stdout must be reserved for protocol frames; diagnostics belong on stderr.
func Serve(ctx context.Context, input io.Reader, output io.Writer, manifest Manifest, handler Handler) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if handler == nil {
		return errors.New("extensionapi: nil handler")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	initialized := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var request Request
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return fmt.Errorf("extensionapi: invalid request: %w", err)
		}
		if request.ID == 0 {
			return errors.New("extensionapi: request requires nonzero id")
		}
		response := Response{ID: request.ID}
		if request.Method == "initialize" {
			if initialized {
				return errors.New("extensionapi: duplicate handshake")
			}
			var version struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(request.Params, &version) != nil || version.Version != Version {
				return errors.New("extensionapi: incompatible handshake")
			}
			response.Result, _ = json.Marshal(manifest)
			initialized = true
		} else {
			if !initialized {
				return errors.New("extensionapi: handshake required")
			}
			callCtx, cancel := context.WithCancel(ctx)
			if request.DeadlineUnixNano > 0 {
				cancel()
				callCtx, cancel = context.WithDeadline(ctx, time.Unix(0, request.DeadlineUnixNano))
			}
			result, err := handleSafely(callCtx, handler, request)
			cancel()
			if err != nil {
				code := "callback_failed"
				var panicked callbackPanicError
				var unknown interface{ OutcomeUnknown() bool }
				if errors.As(err, &panicked) {
					code = "callback_panicked"
				} else if errors.As(err, &unknown) && unknown.OutcomeUnknown() {
					code = "outcome_unknown"
				}
				response.Error = &Diagnostic{Code: code, Message: err.Error()}
			} else {
				response.Result = result
			}
		}
		frame, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("extensionapi: encode response: %w", err)
		}
		if len(frame)+1 > MaxFrameBytes {
			return errors.New("extensionapi: response exceeds frame budget")
		}
		frame = append(frame, '\n')
		if _, err = output.Write(frame); err != nil {
			return fmt.Errorf("extensionapi: write response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("extensionapi: read frame: %w", err)
	}
	return nil
}

func handleSafely(ctx context.Context, h Handler, r Request) (result json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			err = callbackPanicError{}
		}
	}()
	return h.Handle(ctx, r)
}

type callbackPanicError struct{}

func (callbackPanicError) Error() string        { return "extensionapi: callback panicked" }
func (callbackPanicError) OutcomeUnknown() bool { return true }
