//go:build !headless

package cli

import tea "github.com/charmbracelet/bubbletea"

// SetProgram routes questions through the active terminal program.
func (e *cliHITLEmitter) SetProgram(program *tea.Program) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if program == nil {
		e.send = nil
		return
	}
	e.send = func(message HITLMessage) { program.Send(message) }
}
