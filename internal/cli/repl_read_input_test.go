package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lineReadResult struct {
	line string
	err  error
}

type scriptedLineEditor struct {
	results []lineReadResult
}

func (e *scriptedLineEditor) ReadLine(context.Context, string) (string, error) {
	if len(e.results) == 0 {
		return "", io.EOF
	}
	result := e.results[0]
	e.results = e.results[1:]
	return result.line, result.err
}

func (*scriptedLineEditor) SetHistory([]string)    {}
func (*scriptedLineEditor) History() []string      { return nil }
func (*scriptedLineEditor) SetCompleter(Completer) {}

func TestREPLReadInputPropagatesLineEditorError(t *testing.T) {
	readErr := errTTYReaderCancellationUnsupported
	session := &REPLSession{
		lineEditor: &scriptedLineEditor{results: []lineReadResult{{err: readErr}}},
		spanCtx:    context.Background(),
		out:        &bytes.Buffer{},
	}

	require.ErrorIs(t, session.readInput(), readErr)
}

func TestREPLReadInputKeepsContextCancellationAsNormalExit(t *testing.T) {
	session := &REPLSession{
		lineEditor: &scriptedLineEditor{results: []lineReadResult{{err: context.Canceled}}},
		spanCtx:    context.Background(),
		out:        &bytes.Buffer{},
	}

	require.NoError(t, session.readInput())
}

func TestREPLReadInputKeepsInterruptRetryAndEOFBehavior(t *testing.T) {
	var out bytes.Buffer
	session := &REPLSession{
		lineEditor: &scriptedLineEditor{results: []lineReadResult{
			{err: &interruptedError{}},
			{err: io.EOF},
		}},
		spanCtx: context.Background(),
		out:     &out,
	}

	require.NoError(t, session.readInput())
	assert.Contains(t, out.String(), "Ctrl+C again to exit")
}
