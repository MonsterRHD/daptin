package resource

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	log "github.com/sirupsen/logrus"
)

// ManageSieveServer serves the RFC 5804 protocol against Daptin resources.
type ManageSieveServer struct {
	Listener  net.Listener
	TLSConfig *tls.Config
	Root      *DbResource

	closeOnce   sync.Once
	connections  sync.WaitGroup
	connLock     sync.Mutex
	conns        map[net.Conn]struct{}
	closed       bool
}

// trackConn registers an accepted connection.
func (s *ManageSieveServer) trackConn(conn net.Conn) {
	s.connLock.Lock()
	defer s.connLock.Unlock()
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	if s.closed {
		_ = conn.Close()
		return
	}
	s.conns[conn] = struct{}{}
}

// untrackConn removes a connection whose session has ended.
func (s *ManageSieveServer) untrackConn(conn net.Conn) {
	s.connLock.Lock()
	delete(s.conns, conn)
	s.connLock.Unlock()
}

type managesieveSession struct {
	server        *ManageSieveServer
	conn          net.Conn
	reader        *managesieveReader
	tlsActive     bool
	authenticated bool
	mailAccountId int64
	accountRef    string
	sessionUser   *auth.SessionUser
}

// Serve accepts connections until the listener is closed.
func (s *ManageSieveServer) Serve(runtimeErrors chan<- error) {
	for {
		conn, err := s.Listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case runtimeErrors <- fmt.Errorf("ManageSieve accept: %w", err):
			default:
			}
			continue
		}
		s.connections.Add(1)
		go s.handle(conn)
	}
}

// Close stops accepting connections, forcibly closes active sessions so the
// shutdown is bounded, and waits for their goroutines to finish.
func (s *ManageSieveServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.Listener.Close()

		s.connLock.Lock()
		s.closed = true
		conns := make([]net.Conn, 0, len(s.conns))
		for conn := range s.conns {
			conns = append(conns, conn)
		}
		s.connLock.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	s.connections.Wait()
	return err
}

func (s *ManageSieveServer) handle(conn net.Conn) {
	s.trackConn(conn)
	defer s.untrackConn(conn)
	defer s.connections.Done()

	session := &managesieveSession{server: s, conn: conn}
	session.reader = &managesieveReader{
		reader:    bufio.NewReader(conn),
		writeLine: session.writeLine,
	}

	if err := session.writeLine(session.capabilityLine() + " " +
		quoteManageSieve("Daptin ManageSieve ready")); err != nil {
		return
	}

	for {
		// Bound idle connections so Close cannot block forever on a stalled
		// client.
		_ = conn.SetReadDeadline(time.Now().Add(manageSieveIdleTimeout))

		verb, arguments, err := session.reader.ReadCommand()
		if err != nil {
			switch {
			case errors.Is(err, errManageSieveLiteralTooLarge):
				// The oversized body is unread, so the session is closed.
				_ = session.writeLine("NO (QUOTA) literal too large")
			case errors.Is(err, errManageSieveCommandTooLarge):
				_ = session.writeLine("NO command line too long")
			default:
				var netErr net.Error
				if errors.As(err, &netErr) || !strings.Contains(err.Error(), "EOF") {
					log.Debugf("ManageSieve connection ended: %v", err)
				}
			}
			return
		}
		if done := session.dispatch(verb, arguments); done {
			return
		}
	}
}

// manageSieveIdleTimeout bounds reads on an idle session.
const manageSieveIdleTimeout = 10 * time.Minute

// ---------------------------------------------------------------------------
// Protocol responses
// ---------------------------------------------------------------------------

func (s *managesieveSession) writeLine(line string) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(manageSieveWriteTimeout))
	_, err := fmt.Fprintf(s.conn, "%s\r\n", line)
	return err
}

const manageSieveWriteTimeout = 30 * time.Second

func (s *managesieveSession) writeRaw(value string) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(manageSieveWriteTimeout))
	_, err := fmt.Fprintf(s.conn, "%s\r\n", value)
	return err
}

func (s *managesieveSession) sendOK(text string) error {
	return s.writeLine("OK " + quoteManageSieve(text))
}

func (s *managesieveSession) sendNO(code, text string) error {
	line := "NO"
	if strings.TrimSpace(code) != "" {
		line += " (" + code + ")"
	}
	return s.writeLine(line + " " + quoteManageSieve(text))
}

func quoteManageSieve(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func (s *managesieveSession) capabilityLine() string {
	line := `IMPLEMENTATION "Daptin"`
	line += ` SIEVE "` + strings.Join(SupportedSieveCapabilities(), " ") + `"`
	line += " STARTTLS"
	if s.tlsActive {
		line += ` AUTH "PLAIN" SASL "PLAIN"`
	}
	line += ` VERSION "1.0"`
	return line
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func (s *managesieveSession) dispatch(verb string, arguments []string) bool {
	upper := strings.ToUpper(verb)

	// Unknown commands are rejected with UNKNOWN-COMMAND regardless of the
	// authentication state.
	if !isKnownManageSieveCommand(upper) {
		return s.failIfErr(s.sendNO("UNKNOWN-COMMAND", "unknown command: "+verb))
	}

	switch upper {
	case "CAPABILITY":
		if err := s.writeLine(s.capabilityLine()); err != nil {
			return true
		}
		return s.failIfErr(s.sendOK("capabilities listed"))
	case "STARTTLS":
		return s.failIfErr(s.startTLS())
	case "AUTHENTICATE":
		return s.failIfErr(s.authenticate(arguments))
	case "LOGOUT":
		_ = s.sendOK("logout")
		return true
	case "NOOP":
		return s.failIfErr(s.sendOK("noop completed"))
	}

	if !s.authenticated {
		return s.failIfErr(s.sendNO("", "authentication required"))
	}

	var err error
	switch upper {
	case "LISTSCRIPTS":
		err = s.listScripts()
	case "GETSCRIPT":
		err = s.getScript(arguments)
	case "PUTSCRIPT":
		err = s.putScript(arguments)
	case "CHECKSCRIPT":
		err = s.checkScript(arguments)
	case "SETACTIVE":
		err = s.setActive(arguments)
	case "DELETESCRIPT":
		err = s.deleteScript(arguments)
	case "RENAMESCRIPT":
		err = s.renameScript(arguments)
	}
	return s.failIfErr(err)
}

var knownManageSieveCommands = map[string]struct{}{
	"CAPABILITY": {}, "STARTTLS": {}, "AUTHENTICATE": {}, "LOGOUT": {}, "NOOP": {},
	"LISTSCRIPTS": {}, "GETSCRIPT": {}, "PUTSCRIPT": {}, "CHECKSCRIPT": {},
	"SETACTIVE": {}, "DELETESCRIPT": {}, "RENAMESCRIPT": {},
}

func isKnownManageSieveCommand(upper string) bool {
	_, ok := knownManageSieveCommands[upper]
	return ok
}

func (s *managesieveSession) failIfErr(err error) bool {
	if err != nil {
		var protocolError *manageSieveProtocolError
		if errors.As(err, &protocolError) {
			_ = s.sendNO(protocolError.code, protocolError.Error())
			return false
		}
		log.Debugf("ManageSieve session error: %v", err)
		return true
	}
	return false
}

// manageSieveProtocolError carries an RFC 5804 response code for expected
// command failures.
type manageSieveProtocolError struct {
	code string
	text string
}

func (e *manageSieveProtocolError) Error() string { return e.text }

func protocolFailure(code, text string) error {
	return &manageSieveProtocolError{code: code, text: text}
}

// ---------------------------------------------------------------------------
// TLS and authentication
// ---------------------------------------------------------------------------

func (s *managesieveSession) startTLS() error {
	if s.tlsActive {
		return protocolFailure("ALREADYACTIVE", "TLS is already active")
	}
	if s.authenticated {
		return protocolFailure("ALREADYAUTHENTICATED", "already authenticated")
	}
	if s.server.TLSConfig == nil {
		return protocolFailure("", "TLS is not supported")
	}
	if err := s.sendOK("Begin TLS negotiation now"); err != nil {
		return err
	}
	tlsConn := tls.Server(s.conn, s.server.TLSConfig)
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	s.conn = tlsConn
	s.tlsActive = true
	s.reader.reader = bufio.NewReader(tlsConn)
	return nil
}

func (s *managesieveSession) authenticate(arguments []string) error {
	if !s.tlsActive {
		return protocolFailure("ENCRYPT-NEEDED", "TLS is required before authentication")
	}
	if s.authenticated {
		return protocolFailure("ALREADYAUTHENTICATED", "already authenticated")
	}
	if len(arguments) < 1 {
		return protocolFailure("SASL", "AUTHENTICATE requires a mechanism name")
	}
	if strings.ToUpper(arguments[0]) != "PLAIN" {
		return protocolFailure("SASL", "unsupported SASL mechanism")
	}

	payload := ""
	if len(arguments) >= 2 {
		payload = arguments[1]
	} else {
		if err := s.writeLine(`""`); err != nil {
			return err
		}
		continuation, continuationArgs, err := s.reader.ReadCommand()
		if err != nil {
			return err
		}
		_ = continuationArgs
		payload = continuation
	}
	if payload == "*" {
		return protocolFailure("", "authentication cancelled")
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return protocolFailure("SASL", "invalid base64 response")
	}
	parts := strings.SplitN(string(decoded), "\x00", 3)
	if len(parts) != 3 {
		return protocolFailure("SASL", "invalid PLAIN response")
	}
	username := strings.TrimSpace(parts[1])
	password := parts[2]

	tx, err := s.server.Root.Connection().Beginx()
	if err != nil {
		return err
	}
	account, lookupErr := s.server.Root.GetUserMailAccountRowByEmail(username, tx)
	if lookupErr == nil {
		s.sessionUser, err = s.server.Root.MailAccountSessionUser(account, tx)
	}
	_ = tx.Rollback()
	if lookupErr != nil || account == nil {
		return protocolFailure("", "authentication failed")
	}
	hash, _ := account["password"].(string)
	if !BcryptCheckStringHash(password, hash) {
		return protocolFailure("", "authentication failed")
	}

	s.authenticated = true
	s.mailAccountId, _ = account["id"].(int64)
	s.accountRef = referenceIdString(account["reference_id"])
	return s.sendOK("authentication successful")
}

func referenceIdString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case daptinid.DaptinReferenceId:
		return typed.String()
	case []byte:
		var reference daptinid.DaptinReferenceId
		copy(reference[:], typed)
		return reference.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

// ---------------------------------------------------------------------------
// Script commands
// ---------------------------------------------------------------------------

func (s *managesieveSession) listScripts() error {
	tx, err := s.server.Root.Connection().Beginx()
	if err != nil {
		return err
	}
	scripts, err := s.server.Root.ListSieveScripts(s.mailAccountId, tx)
	_ = tx.Rollback()
	if err != nil {
		return err
	}
	for _, script := range scripts {
		name, _ := script["name"].(string)
		line := quoteManageSieve(name)
		if truthy(script["is_active"]) {
			line += " ACTIVE"
		}
		if err := s.writeLine(line); err != nil {
			return err
		}
	}
	return s.sendOK("scripts listed")
}

func (s *managesieveSession) getScript(arguments []string) error {
	if len(arguments) < 1 {
		return protocolFailure("SYNTAX", "GETSCRIPT requires a script name")
	}
	tx, err := s.server.Root.Connection().Beginx()
	if err != nil {
		return err
	}
	script, lookupErr := s.server.Root.GetSieveScriptByName(s.mailAccountId, arguments[0], tx)
	_ = tx.Rollback()
	if lookupErr != nil {
		return protocolFailure("NONEXISTENT", "no such script")
	}
	content, _ := script["script"].(string)
	if err := s.writeLine(fmt.Sprintf("{%d+}", len(content))); err != nil {
		return err
	}
	if err := s.writeRaw(content); err != nil {
		return err
	}
	return s.sendOK("script retrieved")
}

func (s *managesieveSession) putScript(arguments []string) error {
	if len(arguments) < 2 {
		return protocolFailure("SYNTAX", "PUTSCRIPT requires a name and a script")
	}
	name, content := arguments[0], arguments[1]
	if err := ValidateSieveScript(content); err != nil {
		return protocolFailure("", err.Error())
	}

	tx, err := s.server.Root.Connection().Beginx()
	if err != nil {
		return err
	}
	err = s.server.Root.CreateSieveScript(s.mailAccountId, s.accountRef,
		s.sessionUser, name, content, tx)
	if err != nil {
		_ = tx.Rollback()
		if strings.Contains(err.Error(), "already exists") {
			return protocolFailure("SCRIPTEXISTS", "a script with this name already exists")
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.sendOK("script stored")
}

func (s *managesieveSession) checkScript(arguments []string) error {
	if len(arguments) < 1 {
		return protocolFailure("SYNTAX", "CHECKSCRIPT requires a script")
	}
	if err := ValidateSieveScript(arguments[0]); err != nil {
		return protocolFailure("", err.Error())
	}
	return s.sendOK("script is valid")
}

func (s *managesieveSession) setActive(arguments []string) error {
	if len(arguments) < 1 {
		return protocolFailure("SYNTAX", "SETACTIVE requires a script name")
	}
	tx, err := s.server.Root.Connection().Beginx()
	if err != nil {
		return err
	}
	err = s.server.Root.SetActiveSieveScript(s.mailAccountId, arguments[0], s.sessionUser, tx)
	if err != nil {
		_ = tx.Rollback()
		if strings.Contains(err.Error(), "no such sieve script") {
			return protocolFailure("NONEXISTENT", "no such script")
		}
		return protocolFailure("", err.Error())
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.sendOK("active script updated")
}

func (s *managesieveSession) deleteScript(arguments []string) error {
	if len(arguments) < 1 {
		return protocolFailure("SYNTAX", "DELETESCRIPT requires a script name")
	}
	tx, err := s.server.Root.Connection().Beginx()
	if err != nil {
		return err
	}
	err = s.server.Root.DeleteSieveScript(s.mailAccountId, arguments[0], s.sessionUser, tx)
	if err != nil {
		_ = tx.Rollback()
		return protocolFailure("NONEXISTENT", "no such script")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.sendOK("script deleted")
}

func (s *managesieveSession) renameScript(arguments []string) error {
	if len(arguments) < 2 {
		return protocolFailure("SYNTAX", "RENAMESCRIPT requires old and new names")
	}
	tx, err := s.server.Root.Connection().Beginx()
	if err != nil {
		return err
	}
	err = s.server.Root.RenameSieveScript(s.mailAccountId, arguments[0], arguments[1], s.sessionUser, tx)
	if err != nil {
		_ = tx.Rollback()
		if strings.Contains(err.Error(), "already exists") {
			return protocolFailure("SCRIPTEXISTS", "target script name already exists")
		}
		return protocolFailure("NONEXISTENT", "no such script")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.sendOK("script renamed")
}

func truthy(value interface{}) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int64:
		return typed != 0
	case int:
		return typed != 0
	}
	return false
}
