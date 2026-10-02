package imap

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// tracePath is the file every public IMAP operation appends a timing line
// to when NEOMD_IMAP_TRACE=1 (set via SetTracePath, normally
// ~/.cache/neomd/imap-trace.log). Empty = disabled, zero cost.
var (
	traceMu   sync.Mutex
	tracePath string
)

// SetTracePath enables (non-empty) or disables ("") the IMAP timing trace.
func SetTracePath(p string) {
	traceMu.Lock()
	defer traceMu.Unlock()
	tracePath = p
}

// trace appends "<RFC3339Nano> <op> <elapsed>ms". Use as
// `defer trace(time.Now(), "FetchHeaders %s n=%d", folder, n)`. Failures are ignored —
// the trace must never block or fail a mail operation. Deferred arguments are evaluated
// immediately, so format and args are only evaluated when tracing is enabled (zero cost when disabled).
func trace(start time.Time, format string, args ...any) {
	traceMu.Lock()
	p := tracePath
	traceMu.Unlock()
	if p == "" {
		return
	}
	op := fmt.Sprintf(format, args...)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s %dms\n", time.Now().Format(time.RFC3339Nano), op, time.Since(start).Milliseconds())
}
