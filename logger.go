package acp

import "sync"

// Logger is an optional debug logger hook for ACP client diagnostics.
type Logger interface {
	Debugf(format string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Debugf(string, ...any) {}

var (
	loggerMu  sync.RWMutex
	pkgLogger Logger = noopLogger{}
)

func SetLogger(logger Logger) {
	loggerMu.Lock()
	defer loggerMu.Unlock()
	if logger == nil {
		pkgLogger = noopLogger{}
		return
	}
	pkgLogger = logger
}

func debugLogf(format string, args ...any) {
	loggerMu.RLock()
	logger := pkgLogger
	loggerMu.RUnlock()
	logger.Debugf(format, args...)
}
