package production

import "time"

// options holds shared knobs for the production component constructors. Not
// every component consumes every field; unused ones are simply ignored.
type options struct {
	// name overrides the component identifier returned by Name().
	name string
	// fallback is the callable invoked by the CircuitBreaker while Open.
	fallback func() (any, error)
	// now is an injectable clock for deterministic time-based tests. Leave
	// nil to use time.Now.
	now func() time.Time
}

// Option configures a production component at construction time.
type Option func(*options)

// applyOptions folds the given options into a new options value.
func applyOptions(opts []Option) options {
	o := options{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithName overrides the identifier returned by the component's Name method.
func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

// WithFallback wires a fallback callable for the CircuitBreaker, invoked when
// the breaker is Open. It has no effect on other components.
func WithFallback(fn func() (any, error)) Option {
	return func(o *options) { o.fallback = fn }
}

// WithClock injects a time source for deterministic time-based behavior. It is
// primarily consumed by the CircuitBreaker and ignored elsewhere.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}
