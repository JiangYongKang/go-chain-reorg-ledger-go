package ledger

import (
	"log"
	"strings"
	"testing"
)

// testWriter 把标准日志转发到 t.Log，保证单测日志里能看到关键判定依据。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func newLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(testWriter{t}, "", 0)
}
