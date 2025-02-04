package logger

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeferredLogger(t *testing.T) {
	out1 := &bytes.Buffer{}
	out2 := &bytes.Buffer{}

	logger1 := NewLogger(DebugLvl, out1)
	ctx := WithLogger(context.Background(), logger1)
	deferLogger := NewDeferredLogger(ctx)
	logger2 := NewLogger(DebugLvl, out2)

	deferLogger.Infof("Hello %s", "world")
	deferLogger.SetOutput(logger2)
	deferLogger.Infof("Goodbye %s", "world")

	assert.Equal(t, "", out1.String())
	assert.Equal(t, "Hello world\nGoodbye world\n", out2.String())
}

func TestDeferredLoggerOriginal(t *testing.T) {
	out1 := &bytes.Buffer{}

	logger1 := NewLogger(DebugLvl, out1)
	ctx := WithLogger(context.Background(), logger1)
	deferLogger := NewDeferredLogger(ctx)

	deferLogger.Infof("Hello %s", "world")
	deferLogger.SetOutput(deferLogger.Original())
	deferLogger.Infof("Goodbye %s", "world")

	assert.Equal(t, "Hello world\nGoodbye world\n", out1.String())
}
