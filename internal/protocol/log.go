package protocol

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// verboseLevel is the `option verbosity` level from which logging is on; git sends 2 for `-v`.
const verboseLevel = 2

// Logger writes to stderr: debug lines only when verbose, warnings always.
type Logger struct {
	mu        sync.Mutex
	w         io.Writer
	forced    bool
	verbosity int
}

// NewLogger returns a logger writing to w; forced turns debug logging on regardless of `option verbosity`.
func NewLogger(w io.Writer, forced bool) *Logger {
	return &Logger{w: w, forced: forced}
}

// SetVerbosity records the level from `option verbosity`.
func (l *Logger) SetVerbosity(level int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.verbosity = level
}

// Logf prints a debug line when verbose.
func (l *Logger) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.forced || l.verbosity >= verboseLevel {
		l.writef("git-remote-gs: ", format, args...)
	}
}

// Warnf prints a warning git users should see even without -v.
func (l *Logger) Warnf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.writef("warning: ", format, args...)
}

func (l *Logger) writef(prefix, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	_, _ = io.WriteString(l.w, prefix+strings.TrimRight(msg, "\n")+"\n")
}
