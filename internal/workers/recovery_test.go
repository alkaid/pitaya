package workers

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type panicFormattingWorkerValue struct{}

func (panicFormattingWorkerValue) Error() string  { panic("worker panic Error called") }
func (panicFormattingWorkerValue) String() string { panic("worker panic String called") }

func TestRecoveredWorkerPanicMessageDoesNotFormatRecoveredValue(t *testing.T) {
	var message string
	require.NotPanics(t, func() {
		message = recoveredWorkerPanicMessage(panicFormattingWorkerValue{})
	})
	require.Contains(t, message, "workers.panicFormattingWorkerValue")

	outcome := &retryPanicOutcome{value: panicFormattingWorkerValue{}}
	require.NotPanics(t, func() {
		message = outcome.Error()
	})
	require.Contains(t, message, "workers.panicFormattingWorkerValue")
}
