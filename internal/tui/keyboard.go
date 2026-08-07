package tui

import (
	"bufio"
	"context"
	"os"
)

// keyMsg carries a parsed keyboard action through the msgCh into the render
// loop. Decoding happens in keyboardLoop so the main select stays simple.
type keyMsg int

const (
	keySelectUp keyMsg = iota
	keySelectDown
	keyToggle
	keyExpandAll
	keyCollapseAll
	keyQuit
	// Steer input mode keys.
	keySteerEnter       // TAB - enter steer input mode
	keySteerSubmit      // Enter - submit steer text
	keySteerCancel      // Esc - cancel steer input
	keySteerBackspace   // Backspace - delete char before cursor
	keySteerCursorStart // Ctrl+A - move cursor to start
	keySteerCursorEnd   // Ctrl+E - move cursor to end
	keySteerDeleteWord  // Ctrl+W - delete word before cursor
	// Pause/resume.
	keyPause // Space - toggle pause
)

// steerCharMsg carries a single typed character in steer input mode.
type steerCharMsg struct {
	char byte
}

// keyboardLoop runs in its own goroutine while the agent turn is active. It
// reads raw bytes from stdin and translates single-key presses into keyMsg
// values posted to msgCh. Arrow keys arrive as the three-byte CSI sequence
// ESC [ A/B/C/D; we only need up and down for list navigation.
//
// When the app is in steer input mode (steerInputMode atomic flag), printable
// characters are forwarded as steerCharMsg and special keys are translated to
// steer-specific keyMsg values.
//
// The loop exits when ctx is canceled or stdin reports an error (e.g. the
// terminal was closed). It never calls back into the app directly to avoid
// lock ordering issues - everything goes through msgCh.
func (a *BubbleteaApp) keyboardLoop(ctx context.Context) {
	src := a.inputReader
	if src == nil {
		src = os.Stdin
	}
	reader := bufio.NewReader(src)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		b, err := reader.ReadByte()
		if err != nil {
			return
		}

		if a.steerInputMode.Load() {
			a.handleSteerKey(ctx, reader, b)
			continue
		}

		var msg keyMsg
		switch b {
		case 0x1b: // ESC - could be a CSI sequence or a lone Escape
			b2, err := reader.ReadByte()
			if err != nil {
				msg = keyQuit
				break
			}
			if b2 == '[' {
				b3, err := reader.ReadByte()
				if err != nil {
					continue
				}
				switch b3 {
				case 'A':
					msg = keySelectUp
				case 'B':
					msg = keySelectDown
				default:
					continue
				}
			} else {
				continue
			}
		case '\t':
			msg = keySteerEnter
		case '\r', '\n':
			msg = keyToggle
		case ' ':
			msg = keyPause
		case 'e':
			msg = keyExpandAll
		case 'c':
			msg = keyCollapseAll
		case 'q':
			msg = keyQuit
		default:
			continue
		}
		select {
		case a.msgCh <- msg:
		case <-ctx.Done():
			return
		}
	}
}

// handleSteerKey processes a byte received while in steer input mode. It
// translates the byte into the appropriate steerCharMsg or keyMsg and posts it
// to msgCh.
func (a *BubbleteaApp) handleSteerKey(ctx context.Context, reader *bufio.Reader, b byte) {
	var msg any
	switch b {
	case 0x1b: // ESC - cancel steer
		// Check if this is a CSI sequence (arrow key); if so, ignore.
		b2, err := reader.ReadByte()
		if err != nil {
			msg = keySteerCancel
			break
		}
		if b2 == '[' {
			// Read and discard the third byte of the CSI sequence.
			_, _ = reader.ReadByte() //nolint:errcheck // best-effort discard
			return
		}
		msg = keySteerCancel
	case '\r', '\n': // Enter - submit
		msg = keySteerSubmit
	case 0x7f, 0x08: // Backspace / Ctrl+H
		msg = keySteerBackspace
	case 0x01: // Ctrl+A - cursor to start
		msg = keySteerCursorStart
	case 0x05: // Ctrl+E - cursor to end
		msg = keySteerCursorEnd
	case 0x17: // Ctrl+W - delete word
		msg = keySteerDeleteWord
	case 0x15: // Ctrl+U - clear line
		msg = keySteerCancel // reuse cancel to reset
	default:
		if b >= 0x20 && b < 0x7f {
			msg = steerCharMsg{char: b}
		} else {
			return
		}
	}
	select {
	case a.msgCh <- msg:
	case <-ctx.Done():
	}
}

// handleMsg processes a single message posted via Send or keyboardLoop. Key
// messages mutate the accordion model and re-render the view; the onUpdate
// callback (if registered) receives the new view for terminal repainting.
//
// Steer input messages are handled here: the steerInput buffer and cursor are
// updated, and the steerCallback is invoked on submit. The cancelCallback is
// invoked on keyQuit. The pause/resume callbacks are invoked on keyPause.
func (a *BubbleteaApp) handleMsg(msg Msg) bool {
	var view string
	notify := a.onUpdate != nil
	quit := false

	func() {
		a.mu.Lock()
		defer a.mu.Unlock()

		switch m := msg.(type) {
		case keyMsg:
			switch m {
			case keySelectUp:
				a.accordion.Select(-1)
			case keySelectDown:
				a.accordion.Select(1)
			case keyToggle:
				a.accordion.Toggle()
			case keyExpandAll:
				a.accordion.ExpandAll()
			case keyCollapseAll:
				a.accordion.CollapseAll()
			case keyQuit:
				// Call cancel callback before quitting.
				if a.cancelCallback != nil {
					a.cancelCallback()
				}
				quit = true
			case keySteerEnter:
				a.steerInputMode.Store(true)
				a.steerInput = ""
				a.steerCursor = 0
			case keySteerSubmit:
				input := a.steerInput
				a.steerInputMode.Store(false)
				a.steerInput = ""
				a.steerCursor = 0
				if a.steerCallback != nil && input != "" {
					cb := a.steerCallback
					go cb(input)
				}
			case keySteerCancel:
				a.steerInputMode.Store(false)
				a.steerInput = ""
				a.steerCursor = 0
			case keySteerBackspace:
				if a.steerCursor > 0 {
					a.steerInput = a.steerInput[:a.steerCursor-1] + a.steerInput[a.steerCursor:]
					a.steerCursor--
				}
			case keySteerCursorStart:
				a.steerCursor = 0
			case keySteerCursorEnd:
				a.steerCursor = len(a.steerInput)
			case keySteerDeleteWord:
				a.deleteWord()
			case keyPause:
				if a.paused {
					a.paused = false
					if a.resumeCallback != nil {
						a.resumeCallback()
					}
				} else {
					a.paused = true
					if a.pauseCallback != nil {
						a.pauseCallback()
					}
				}
			}
		case steerCharMsg:
			if a.steerInputMode.Load() {
				a.steerInput = a.steerInput[:a.steerCursor] + string(m.char) + a.steerInput[a.steerCursor:]
				a.steerCursor++
			}
		}

		if notify && !quit {
			view = a.renderView()
		}
	}()

	if notify && !quit {
		a.onUpdate(view)
	}
	return quit
}

// deleteWord deletes the word before the cursor (Ctrl+W behavior).
func (a *BubbleteaApp) deleteWord() {
	if a.steerCursor == 0 {
		return
	}
	// Skip trailing spaces.
	end := a.steerCursor
	for end > 0 && a.steerInput[end-1] == ' ' {
		end--
	}
	// Skip word characters.
	for end > 0 && a.steerInput[end-1] != ' ' {
		end--
	}
	a.steerInput = a.steerInput[:end] + a.steerInput[a.steerCursor:]
	a.steerCursor = end
}
