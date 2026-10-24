//go:build !headless

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/pengjunchen/go-cli/internal/tui"
)

type replApp = tui.BubbleteaApp

func newREPLThemeManager() ThemeController { return tui.NewThemeManager() }

func interactiveApprovalsEnabled() bool { return tui.IsTerminal() }

// renderTUI builds the TUI options, creates the BubbleteaApp, starts the
// render goroutine, and wires HITL through the bubbletea program.
func (s *REPLSession) renderTUI(initialTranscript *tui.CanonicalTranscriptSnapshot) error {
	// Bridge core events to TUI events and render. Bubbletea owns terminal
	// rendering in TTY mode (raw mode, diff-based repaint on stderr), so
	// the legacy onUpdate manual redraw and ANSI cursor logic has been
	// removed.
	var eventSource *tui.OwnedAgentEventStream
	if s.rc != nil && s.rc.TUI.Mode == "remote" && s.rc.TUI.RemoteURL != "" {
		eventSource = tui.NewACPStreamAdapter(s.rc.TUI.RemoteURL).StreamOwned(s.turnCtx)
	} else {
		eventSource = tui.BridgeEventsOwned(s.turnCtx, s.stream)
	}
	s.eventSource = eventSource
	s.turnCounter++
	s.isTTY = tui.IsTerminal()
	tsp := tui.NewDefaultTerminalSizeProvider()
	appOpts := []tui.AppOption{
		tui.WithWidth(tsp.Width()),
		tui.WithModelInfo(s.slashCtx.ModelName()),
		tui.WithTurnCount(s.turnCounter),
		tui.WithSessionInfo(s.assembly.SessionID),
	}
	if snapshot, ok := s.assembly.todoSnapshot(); ok {
		appOpts = append(appOpts, tui.WithTodoSnapshot(todoUpdateMsg(snapshot)))
	}
	// Mode label: show "plan" when plan mode is active, otherwise "chat".
	if s.assembly.PlanCtrl != nil && s.assembly.PlanCtrl.IsActive() {
		appOpts = append(appOpts, tui.WithModeLabel("plan"))
	} else {
		appOpts = append(appOpts, tui.WithModeLabel("chat"))
	}
	// TUIConfig: word wrap, diff style. The shared theme manager is
	// passed separately so runtime /theme switches persist across turns.
	if s.rc != nil {
		appOpts = append(appOpts,
			tui.WithWordWrap(s.rc.TUI.WordWrap),
			tui.WithDiffStyle(s.rc.TUI.DiffStyle),
		)
	}
	if manager, ok := s.sharedThemeMgr.(*tui.ThemeManager); ok {
		appOpts = append(appOpts, tui.WithThemeManager(manager))
	}
	if s.assembly.ApprovalChannel != nil {
		appOpts = append(appOpts, tui.WithApprovalChannel(s.assembly.ApprovalChannel))
	}
	if s.slashCtx.thinkingVisibility != "" {
		appOpts = append(appOpts, tui.WithThinkingVisibility(s.slashCtx.thinkingVisibility))
	}
	if s.assembly.durableSession != nil {
		appOpts = append(appOpts, tui.WithDeferQuitUntilTranscriptRefresh())
	}
	appOpts = append(appOpts, s.buildCallbackOptions()...)
	s.uiCtx, s.cancelUI = context.WithCancel(s.spanCtx)
	if initialTranscript != nil {
		app, err := tui.NewBubbleteaAppWithTranscript(eventSource.Events(), *initialTranscript, appOpts...)
		if err != nil {
			s.cancelUI()
			return fmt.Errorf("interactive: create canonical transcript TUI: %w", err)
		}
		s.app = app
	} else {
		s.app = tui.NewBubbleteaApp(eventSource.Events(), appOpts...)
	}

	s.appStarted = true
	go func() {
		if runErr := s.app.Run(s.uiCtx); runErr != nil && runErr != context.Canceled {
			s.logger.Debug("cli_interactive_tui_error", "err", runErr)
		}
	}()

	s.wireHITL()
	return nil
}

// buildCallbackOptions constructs the steer, follow-up, cancel, pause, and
// resume callback options for the BubbleteaApp.
func (s *REPLSession) buildCallbackOptions() []tui.AppOption {
	return []tui.AppOption{
		tui.WithSteerCallback(func(input string) {
			turnID := s.assembly.TurnRunner.RunningTurnID()
			if turnID != "" {
				if steerErr := s.assembly.TurnRunner.Steer(s.turnCtx, turnID, input); steerErr != nil {
					s.logger.Warn("cli_interactive_steer_callback_failed", "err", steerErr)
				}
			} else if s.assembly.SteerChannel != nil {
				select {
				case s.assembly.SteerChannel <- input:
				default:
				}
			}
		}),
		tui.WithFollowUpCallback(func(input string) {
			turnID := s.assembly.TurnRunner.RunningTurnID()
			if turnID != "" {
				if err := s.assembly.TurnRunner.FollowUp(s.turnCtx, turnID, input); err != nil {
					s.logger.Warn("cli_interactive_followup_callback_failed", "err", err)
				}
			} else if s.assembly.FollowUpChannel != nil {
				select {
				case s.assembly.FollowUpChannel <- input:
				default:
					s.logger.Warn("cli_interactive_followup_channel_full")
				}
			}
		}),
		tui.WithCancelCallback(func() {
			if s.turnCtx != nil && s.turnCtx.Err() == nil && s.spanCtx != nil && s.spanCtx.Err() == nil {
				s.uiCancelRequested.Store(true)
			}
			if s.cancelTurn != nil {
				s.cancelTurn()
			}
		}),
		tui.WithPauseCallback(func() {
			if s.assembly.LoopAgent != nil {
				s.assembly.LoopAgent.Pause()
			}
		}),
		tui.WithResumeCallback(func() {
			if s.assembly.LoopAgent != nil {
				s.assembly.LoopAgent.Resume()
			}
		}),
	}
}

// wireHITL waits for the bubbletea program to be initialized so HITL can
// route through it.
func (s *REPLSession) wireHITL() {
	// Wait for the tea.Program to be initialized so HITL can route
	// through it. The program is created inside Run (which runs in the
	// goroutine above); instead of polling, we wait on the
	// ProgramReady channel which closes once the program is stored.
	// In TTY mode this routes HITL questions through bubbletea instead
	// of corrupting stdout.
	if s.assembly.HITLEmitter != nil {
		select {
		case <-s.app.ProgramReady():
			if prog := s.app.Program(); prog != nil {
				s.assembly.HITLEmitter.SetProgram(prog)
			}
		case <-time.After(5 * time.Second):
			s.logger.Warn("cli_interactive_hitl_program_timeout",
				"op", "cli.interactive.hitl.program_timeout",
			)
		case <-s.turnCtx.Done():
		}
	}
}
