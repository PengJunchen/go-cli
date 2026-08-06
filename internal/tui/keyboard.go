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
)

// keyboardLoop runs in its own goroutine while the agent turn is active. It
// reads raw bytes from stdin and translates single-key presses into keyMsg
// values posted to msgCh. Arrow keys arrive as the three-byte CSI sequence
// ESC [ A/B/C/D; we only need up and down for list navigation.
//
// The loop exits when ctx is canceled or stdin reports an error (e.g. the
// terminal was closed). It never calls back into the app directly to avoid
// lock ordering issues — everything goes through msgCh.
func (a *BubbleteaApp) keyboardLoop(ctx context.Context) {
	reader := bufio.NewReader(os.Stdin)
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
		var msg keyMsg
		switch b {
		case 0x1b: // ESC — could be a CSI sequence or a lone Escape
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
		case '\t', '\r', '\n':
			msg = keyToggle
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

// handleMsg processes a single message posted via Send or keyboardLoop. Key
// messages mutate the accordion model and re-render the view; the onUpdate
// callback (if registered) receives the new view for terminal repainting.
func (a *BubbleteaApp) handleMsg(msg Msg) bool {
	km, ok := msg.(keyMsg)
	if !ok {
		return false
	}
	var view string
	notify := a.onUpdate != nil
	quit := false
	func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch km {
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
			quit = true
		}
		if notify && !quit {
			view = a.accordion.Render()
		}
	}()
	if notify && !quit {
		a.onUpdate(view)
	}
	return quit
}
