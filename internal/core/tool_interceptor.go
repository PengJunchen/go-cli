package core

// SetToolInterceptor registers a single ToolInterceptor on this LoopAgent,
// replacing any previously registered loop-scoped interceptors. The
// interceptor is called synchronously before each tool execution (inside
// PreToolCallEvent.IsCancelled); if it returns a non-nil error, the tool call
// is skipped. Global interceptors registered with RegisterToolInterceptor
// remain independent.
//
// Pass nil to remove all interceptors.
func (l *LoopAgent) SetToolInterceptor(f ToolInterceptor) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f == nil {
		l.toolInterceptors = nil
	} else {
		l.toolInterceptors = []ToolInterceptor{f}
	}
}

// SetContextToolInterceptor registers a single run-context-aware interceptor
// on this LoopAgent, replacing any previously registered context-aware
// interceptors. It remains separate from SetToolInterceptor so existing
// context-free callers retain their behavior.
func (l *LoopAgent) SetContextToolInterceptor(f ContextToolInterceptor) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f == nil {
		l.contextToolInterceptors = nil
	} else {
		l.contextToolInterceptors = []ContextToolInterceptor{f}
	}
}
