package imap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrace_WritesLineWhenEnabled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "imap-trace.log")
	SetTracePath(p)
	defer SetTracePath("")
	trace("FetchHeaders INBOX n=200", time.Now().Add(-3*time.Millisecond))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("trace file not written: %v", err)
	}
	line := strings.TrimSpace(string(b))
	if !strings.Contains(line, " FetchHeaders INBOX n=200 ") || !strings.HasSuffix(line, "ms") {
		t.Errorf("unexpected trace line %q", line)
	}
	if _, err := time.Parse(time.RFC3339Nano, strings.Fields(line)[0]); err != nil {
		t.Errorf("first field is not RFC3339Nano: %q", line)
	}
}

func TestTrace_NoopWhenDisabled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "imap-trace.log")
	SetTracePath("")
	trace("Ping", time.Now())
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("trace file must not exist when disabled")
	}
}
