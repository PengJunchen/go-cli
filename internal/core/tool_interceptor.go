package core

// SetToolInterceptor registers a single ToolInterceptor on the LoopAgent,
// replacing any previously registered interceptors. The interceptor is called
// synchronously before each tool execution (inside PreToolCallEvent.IsCancelled);
// if it returns a non-nil error, the tool call is skipped.
//
// Pass nil to remove all interceptors.
func (l *LoopAgent) SetToolInterceptor(f ToolInterceptor) {
	globalInterceptorsMu.Lock()
	if f == nil {
		globalInterceptors = nil
	} else {
		globalInterceptors = []ToolInterceptor{f}
	}
	globalInterceptorsMu.Unlock()
}
