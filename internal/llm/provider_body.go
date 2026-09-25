package llm

import (
	"errors"
	"io"
)

const (
	maxProviderJSONBodyBytes  = 10 << 20
	maxProviderErrorBodyBytes = 4 << 10
)

// ErrProviderResponseTooLarge marks a successful provider response whose full
// JSON body exceeds the bounded decoder limit. It is a fixed diagnostic and
// intentionally does not include response bytes.
var ErrProviderResponseTooLarge = errors.New("llm: provider response body exceeds configured limit")

var (
	errProviderBodyMissing = errors.New("provider response body is missing")
	errProviderBodyLimit   = errors.New("provider response body limit is invalid")
)

// providerBodyIOError preserves the underlying I/O cause for errors.Is and
// errors.As while keeping potentially sensitive transport text out of Error.
type providerBodyIOError struct {
	operation string
	cause     error
}

func (e *providerBodyIOError) Error() string {
	if e.operation == "close" {
		return "llm: provider response body close failed"
	}
	return "llm: provider response body read failed"
}

func (e *providerBodyIOError) Unwrap() error { return e.cause }

// readBoundedProviderBody reads no more than limit+1 bytes and always closes
// the acquired body exactly once before returning. The extra byte distinguishes
// a body at the limit from one that must be rejected or marked truncated.
func readBoundedProviderBody(body io.ReadCloser, limit int) (payload []byte, truncated bool, readErr, closeErr error) {
	if body == nil {
		return nil, false, &providerBodyIOError{operation: "read", cause: errProviderBodyMissing}, nil
	}
	maxInt := int(^uint(0) >> 1)
	if limit < 0 || limit == maxInt {
		readErr = &providerBodyIOError{operation: "read", cause: errProviderBodyLimit}
		closeErr = closeProviderBody(body)
		return nil, false, readErr, closeErr
	}

	payload, readErr = readProviderBodyBytes(body, int64(limit)+1)
	closeErr = closeProviderBody(body)
	if readErr != nil {
		return nil, false, readErr, closeErr
	}
	if len(payload) > limit {
		payload = payload[:limit]
		truncated = true
	}
	return payload, truncated, nil, closeErr
}

func readProviderBodyBytes(body io.Reader, readLimit int64) (payload []byte, err error) {
	defer func() {
		if recover() != nil {
			payload = nil
			err = &providerBodyIOError{operation: "read"}
		}
	}()
	payload, err = io.ReadAll(io.LimitReader(body, readLimit))
	if err != nil {
		payload = nil
		err = &providerBodyIOError{operation: "read", cause: err}
	}
	return payload, err
}

func closeProviderBody(body io.Closer) (err error) {
	defer func() {
		if recover() != nil {
			err = &providerBodyIOError{operation: "close"}
		}
	}()
	if err := body.Close(); err != nil {
		return &providerBodyIOError{operation: "close", cause: err}
	}
	return nil
}
