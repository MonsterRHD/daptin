package server

import (
	"errors"
	"sync"

	"github.com/artpar/go-imap"
	"github.com/artpar/go-imap/commands"
	"github.com/artpar/go-imap/responses"
	imapserver "github.com/artpar/go-imap/server"
)

const (
	condStoreCapability = "CONDSTORE"
	qresyncCapability   = "QRESYNC"
)

// imapConnState holds per-connection negotiation state. It is released when
// the connection closes (see condStoreConn.Close).
type imapConnState struct {
	enabled        map[string]bool
	condstoreAware bool
}

func (s *imapConnState) enable(capability string) {
	s.enabled[capability] = true
}

func (s *imapConnState) isEnabled(capability string) bool {
	return s.enabled[capability]
}

var connStates sync.Map // server.Conn -> *imapConnState

func getConnState(c imapserver.Conn) *imapConnState {
	if value, ok := connStates.Load(c); ok {
		return value.(*imapConnState)
	}
	state := &imapConnState{enabled: map[string]bool{}}
	actual, _ := connStates.LoadOrStore(c, state)
	return actual.(*imapConnState)
}

func deleteConnState(c imapserver.Conn) {
	connStates.Delete(c)
}

// condStoreConn wraps the connection solely to release its state on close.
type condStoreConn struct {
	imapserver.Conn
}

func (c *condStoreConn) Close() error {
	deleteConnState(c.Conn)
	return c.Conn.Close()
}

// CondStoreExtension provides RFC 7162 CONDSTORE/QRESYNC as a go-imap server
// extension.
type CondStoreExtension struct{}

func NewCondStoreExtension() *CondStoreExtension {
	return &CondStoreExtension{}
}

func (ext *CondStoreExtension) Capabilities(_ imapserver.Conn) []string {
	return []string{condStoreCapability, qresyncCapability}
}

func (ext *CondStoreExtension) NewConn(c imapserver.Conn) imapserver.Conn {
	return &condStoreConn{Conn: c}
}

func (ext *CondStoreExtension) Command(name string) imapserver.HandlerFactory {
	switch name {
	case "ENABLE":
		return func() imapserver.Handler { return &enableHandler{} }
	case "SELECT", "EXAMINE":
		return func() imapserver.Handler { return &condStoreSelect{} }
	case "FETCH":
		return func() imapserver.Handler { return &condStoreFetch{} }
	case "STORE":
		return func() imapserver.Handler { return &condStoreStore{} }
	case "EXPUNGE":
		return func() imapserver.Handler { return &condStoreExpunge{} }
	}
	return nil
}

// enableHandler implements ENABLE (RFC 5161). Only CONDSTORE and QRESYNC are
// accepted; enabling QRESYNC also enables CONDSTORE (RFC 7162).
type enableHandler struct {
	commands.Enable
}

func (h *enableHandler) Handle(c imapserver.Conn) error {
	ctx := c.Context()
	if ctx.State != imap.AuthenticatedState {
		return errors.New("ENABLE is only allowed in the authenticated state, before a mailbox is selected")
	}

	state := getConnState(c)
	accepted := []string{}
	accept := func(capability string) {
		if state.isEnabled(capability) {
			return
		}
		state.enable(capability)
		accepted = append(accepted, capability)
	}

	for _, capability := range h.Caps {
		switch capability {
		case condStoreCapability:
			accept(condStoreCapability)
			state.condstoreAware = true
		case qresyncCapability:
			// QRESYNC implies CONDSTORE.
			if !state.isEnabled(condStoreCapability) {
				state.enable(condStoreCapability)
				accepted = append(accepted, condStoreCapability)
			}
			state.enable(qresyncCapability)
			accepted = append(accepted, qresyncCapability)
			state.condstoreAware = true
		}
	}

	return c.WriteResp(&responses.Enabled{Caps: accepted})
}
