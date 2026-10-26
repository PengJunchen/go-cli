//go:build darwin || linux

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type approvalParityOutcome struct {
	result *tools.ToolResult
	err    error
}

func TestAssembledAskBaseClassifierMatchesDisplayedPolicy(t *testing.T) {
	for _, channel := range []bool{false, true} {
		cfg := newAssembleTestConfig(t)
		options := []AssembleOption{WithApproveMode(ApproveAsk)}
		if channel {
			options = append(options, WithApprovalChannel(make(chan approval.InteractiveRequest, 1)))
		}
		assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, options...)
		require.NoError(t, err)
		t.Cleanup(assembly.Cleanup)
		require.Equal(t, core.ClassificationRequireApproval, assembly.Registry.ApprovalClassifier().Classify(context.Background(), "bash"))
	}
}

func TestAssembledAskBashUsesHumanChoiceAcrossDisplays(t *testing.T) {
	for _, display := range []string{"stdio", "channel"} {
		for _, choice := range []string{"yes", "no", "input-ended", "cancel-late-always"} {
			t.Run(display+"/"+choice, func(t *testing.T) {
				cfg := newAssembleTestConfig(t)
				cfg.Tools.Builtin = []string{"bash"}
				cwd, err := os.Getwd()
				require.NoError(t, err)
				cfg.Sandbox.AllowedPaths = []string{cfg.Git.WorkDir, cwd}
				marker := filepath.Join(cfg.Git.WorkDir, "bash-approved.txt")
				prompt, err := os.CreateTemp(cfg.Git.WorkDir, "approval-prompt-")
				require.NoError(t, err)
				previousStdin, previousStderr := os.Stdin, os.Stderr
				os.Stderr = prompt
				var master *os.File
				closeInput := func() {}
				if display == "stdio" {
					slave, ownedMaster, cleanup := openPTYWithMaster(t)
					master, os.Stdin = ownedMaster, slave
					var once sync.Once
					closeInput = func() { once.Do(cleanup) }
				}
				t.Cleanup(func() {
					os.Stdin, os.Stderr = previousStdin, previousStderr
					closeInput()
					require.NoError(t, prompt.Close())
				})
				channel := make(chan approval.InteractiveRequest, 1)
				options := []AssembleOption{WithApproveMode(ApproveAsk)}
				if display == "channel" {
					options = append(options, WithApprovalChannel(channel))
				}
				assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, options...)
				require.NoError(t, err)
				t.Cleanup(assembly.Cleanup)
				preparer := assembly.ToolRegistry.(tools.ToolCallPreparer)
				lifetime, stop := context.WithTimeout(context.Background(), 8*time.Second)
				ctx, cancel := context.WithCancel(lifetime)
				var workers sync.WaitGroup
				t.Cleanup(func() { cancel(); stop(); closeInput(); workers.Wait() })
				call := tools.ToolCall{ID: "approval-parity", Name: "bash", Args: map[string]any{"command": "printf ran >> " + shellSingleQuote(marker)}}
				run := func(callCtx context.Context) <-chan approvalParityOutcome {
					done := make(chan approvalParityOutcome, 1)
					workers.Add(1)
					go func() {
						defer workers.Done()
						invocation, err := preparer.PrepareToolCall(callCtx, call)
						if err != nil {
							done <- approvalParityOutcome{err: err}
							return
						}
						result, err := invocation.Dispatch(callCtx)
						done <- approvalParityOutcome{result: result, err: err}
					}()
					return done
				}
				resolve := func(done <-chan approvalParityOutcome, response string, promptCount int) {
					if display == "channel" {
						select {
						case request := <-channel:
							require.Equal(t, "bash", request.ToolName)
							require.NoFileExists(t, marker)
							switch response {
							case "yes":
								request.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveAllow, DecisionSource: "user"}
							case "input-ended":
								request.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveDeny, DecisionSource: "canceled"}
							case "cancel-late-always":
								cancel()
								request.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveAlwaysAllow, DecisionSource: "user"}
							default:
								request.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveDeny, DecisionSource: "user"}
							}
						case outcome := <-done:
							t.Fatalf("Bash finished without requesting human approval: %v", outcome.err)
						case <-lifetime.Done():
							t.Fatal(lifetime.Err())
						}
						return
					}
					waitApprovalParityPrompt(t, prompt.Name(), promptCount, done)
					require.NoFileExists(t, marker)
					input := "n\n"
					switch response {
					case "yes":
						input = "y\n"
					case "input-ended":
						input = "\x04"
					case "cancel-late-always":
						cancel()
						input = "a\n" // Release the owned reader after cancellation; no goroutine is abandoned.
					}
					_, err := io.WriteString(master, input)
					require.NoError(t, err)
				}
				await := func(done <-chan approvalParityOutcome) approvalParityOutcome {
					select {
					case outcome := <-done:
						return outcome
					case <-time.After(2 * time.Second):
						t.Fatal("prepared Bash did not join after the approval response")
						return approvalParityOutcome{}
					}
				}
				done := run(ctx)
				resolve(done, choice, 1)
				outcome := await(done)
				if choice == "yes" {
					require.NoError(t, outcome.err)
					body, err := os.ReadFile(marker)
					require.NoError(t, err)
					require.Equal(t, "ran", string(body))
				} else {
					if choice == "cancel-late-always" {
						require.ErrorIs(t, outcome.err, context.Canceled)
					} else {
						require.ErrorIs(t, outcome.err, approval.ErrToolDenied)
					}
					require.NoFileExists(t, marker)
				}
				entries, err := approval.NewAuditTrail(filepath.Join(cfg.Git.WorkDir, ".go-cli", "audit")).ReadEntries(time.Now())
				require.NoError(t, err)
				var resolutions []approval.AuditEntry
				for _, entry := range entries {
					if entry.Stage == "classifier_proposal" {
						require.Equal(t, "ask", entry.Decision)
						require.Equal(t, "safety_policy", entry.Classifier)
						require.Equal(t, "default", entry.PermissionMode)
					}
					if entry.Stage == "approval_resolution" {
						resolutions = append(resolutions, entry)
					}
				}
				require.Len(t, resolutions, 1)
				if choice == "yes" || choice == "no" {
					require.Equal(t, "user", resolutions[0].Resolution)
					wantChoice := "deny"
					if choice == "yes" {
						wantChoice = "allow_once"
					}
					require.Equal(t, wantChoice, resolutions[0].UserChoice)
				} else {
					require.Empty(t, resolutions[0].UserChoice, "missing or cancelled input cannot grant remembered approval")
					require.NotEqual(t, "user", resolutions[0].Resolution)
				}
				if choice != "yes" {
					// The exact same call in a fresh context must ask again, rather
					// than dispatch through a grant left by EOF or a late always key.
					retry := run(lifetime)
					resolve(retry, "no", 2)
					require.ErrorIs(t, await(retry).err, approval.ErrToolDenied)
					require.NoFileExists(t, marker)
				}
			})
		}
	}
}

func TestAssembledBashExplicitModesIgnoreDisplay(t *testing.T) {
	for _, mode := range []ApproveMode{ApproveAuto, ApproveDeny} {
		for _, display := range []string{"stdio", "channel"} {
			t.Run(mode.String()+"/"+display, func(t *testing.T) {
				cfg := newAssembleTestConfig(t)
				cfg.Tools.Builtin = []string{"bash"}
				cwd, err := os.Getwd()
				require.NoError(t, err)
				cfg.Sandbox.AllowedPaths = []string{cfg.Git.WorkDir, cwd}
				marker := filepath.Join(cfg.Git.WorkDir, "explicit-mode.txt")
				channel := make(chan approval.InteractiveRequest, 1)
				options := []AssembleOption{WithApproveMode(mode)}
				if display == "channel" {
					options = append(options, WithApprovalChannel(channel))
				}
				assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, options...)
				require.NoError(t, err)
				t.Cleanup(assembly.Cleanup)
				require.Nil(t, assembly.ApprovalChannel)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				invocation, err := assembly.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(ctx, tools.ToolCall{
					ID: "explicit-mode", Name: "bash", Args: map[string]any{"command": "printf ran >> " + shellSingleQuote(marker)},
				})
				if mode == ApproveDeny {
					require.ErrorIs(t, err, approval.ErrToolDenied)
					require.Nil(t, invocation)
					require.NoFileExists(t, marker)
				} else {
					require.NoError(t, err)
					_, err = invocation.Dispatch(ctx)
					require.NoError(t, err)
					body, readErr := os.ReadFile(marker)
					require.NoError(t, readErr)
					require.Equal(t, "ran", string(body))
				}
				require.Empty(t, channel)
			})
		}
	}
}

func TestAssembledAskNonTTYCannotProvideApproval(t *testing.T) {
	cfg := newAssembleTestConfig(t)
	cfg.Tools.Builtin = []string{"bash"}
	input, err := os.CreateTemp(cfg.Git.WorkDir, "non-tty-")
	require.NoError(t, err)
	_, err = io.WriteString(input, "a\n")
	require.NoError(t, err)
	_, err = input.Seek(0, io.SeekStart)
	require.NoError(t, err)
	previousStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previousStdin; require.NoError(t, input.Close()) })
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAsk))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	marker := filepath.Join(cfg.Git.WorkDir, "must-not-run.txt")
	call := tools.ToolCall{ID: "non-tty", Name: "bash", Args: map[string]any{"command": "printf ran >> " + shellSingleQuote(marker)}}
	for range 2 {
		invocation, prepareErr := assembly.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(context.Background(), call)
		require.ErrorIs(t, prepareErr, approval.ErrToolDenied)
		require.Nil(t, invocation)
	}
	require.NoFileExists(t, marker)
	position, err := input.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	require.Zero(t, position, "non-TTY input cannot be consumed as human authorization")
	entries, err := approval.NewAuditTrail(filepath.Join(cfg.Git.WorkDir, ".go-cli", "audit")).ReadEntries(time.Now())
	require.NoError(t, err)
	resolutions := 0
	for _, entry := range entries {
		if entry.Stage == "approval_resolution" {
			resolutions++
			require.Equal(t, "non_tty_deny", entry.Resolution)
			require.Empty(t, entry.UserChoice)
		}
	}
	require.Equal(t, 2, resolutions)
}

func waitApprovalParityPrompt(t *testing.T, path string, count int, done <-chan approvalParityOutcome) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		if strings.Count(string(body), "[approval] Tool 'bash'") >= count {
			return
		}
		select {
		case outcome := <-done:
			t.Fatalf("Bash finished before a stdio approval prompt: %v", outcome.err)
		case <-timer.C:
			t.Fatal("stdio Bash did not request human approval")
		case <-ticker.C:
		}
	}
}
