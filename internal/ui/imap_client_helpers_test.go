package ui

import (
	"testing"

	"github.com/sspaeti/neomd/internal/imap"
)

func TestBgImapCli_FallsBackToPrimaryWhenNil(t *testing.T) {
	primary := imap.New(imap.Config{Host: "h", Port: "993"})
	m := Model{clients: []*imap.Client{primary}, accountI: 0}
	if got := m.bgImapCli(); got != primary {
		t.Errorf("no bgClients: want primary, got %v", got)
	}
	m.bgClients = []*imap.Client{nil}
	if got := m.bgImapCli(); got != primary {
		t.Errorf("nil bg entry: want primary, got %v", got)
	}
	bg := imap.New(imap.Config{Host: "h", Port: "993"})
	m.bgClients = []*imap.Client{bg}
	if got := m.bgImapCli(); got != bg {
		t.Errorf("want bg client, got %v", got)
	}
}

func TestBgImapCli_NilWhenEverythingDisabled(t *testing.T) {
	m := Model{clients: []*imap.Client{nil}, bgClients: []*imap.Client{nil}}
	if got := m.bgImapCli(); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}
