package resource

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Client helpers
// ---------------------------------------------------------------------------

type managesieveClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

func dialManageSieve(t *testing.T, address string) *managesieveClient {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	return &managesieveClient{conn: conn, reader: bufio.NewReader(conn)}
}

func (c *managesieveClient) send(command string) error {
	_, err := fmt.Fprintf(c.conn, "%s\r\n", command)
	return err
}

func (c *managesieveClient) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// readResponse collects all lines until OK/NO and returns the final status.
func (c *managesieveClient) readResponse() (string, []string, error) {
	lines := []string{}
	for {
		line, err := c.readLine()
		if err != nil {
			return "", lines, err
		}
		if strings.HasPrefix(line, "OK") || strings.HasPrefix(line, "NO") {
			return line[:2], append(lines, line), nil
		}
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			var length int
			fmt.Sscanf(line, "{%d}", &length)
			buffer := make([]byte, length)
			if _, err := readFullClient(c.reader, buffer); err != nil {
				return "", lines, err
			}
			// consume trailing CRLF
			if _, _, err := c.reader.ReadLine(); err != nil {
				return "", lines, err
			}
			lines = append(lines, string(buffer))
			continue
		}
		lines = append(lines, line)
	}
}

func readFullClient(reader *bufio.Reader, buffer []byte) (int, error) {
	read := 0
	for read < len(buffer) {
		count, err := reader.Read(buffer[read:])
		read += count
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func mustBeOK(t *testing.T, status string) {
	t.Helper()
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
}

// ---------------------------------------------------------------------------
// Self-signed certificate
// ---------------------------------------------------------------------------

func selfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}}
}

// ---------------------------------------------------------------------------
// End-to-end test
// ---------------------------------------------------------------------------

func TestManageSieveEndToEnd(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &ManageSieveServer{
		Listener:  listener,
		TLSConfig: selfSignedTLSConfig(t),
		Root:      env.root,
	}
	runtimeErrors := make(chan error, 5)
	go server.Serve(runtimeErrors)
	t.Cleanup(func() { _ = server.Close() })

	client := dialManageSieve(t, listener.Addr().String())
	t.Cleanup(func() { _ = client.conn.Close() })

	// Greeting: capabilities with SIEVE list, STARTTLS, no AUTH before TLS.
	greeting, err := client.readLine()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(greeting, `IMPLEMENTATION "Daptin"`) ||
		!strings.Contains(greeting, "vacation") ||
		!strings.Contains(greeting, "STARTTLS") {
		t.Fatalf("greeting missing capabilities: %q", greeting)
	}
	if strings.Contains(greeting, "AUTH") {
		t.Fatalf("AUTH advertised before TLS: %q", greeting)
	}

	// AUTH is refused before TLS.
	if err := client.send(`AUTHENTICATE PLAIN`); err != nil {
		t.Fatal(err)
	}
	status, _, err := client.readResponse()
	if err != nil {
		t.Fatal(err)
	}
	if status != "NO" {
		t.Fatal("AUTH permitted before TLS")
	}

	// Unauthenticated management commands are refused.
	if err := client.send("LISTSCRIPTS"); err != nil {
		t.Fatal(err)
	}
	status, _, _ = client.readResponse()
	if status != "NO" {
		t.Fatal("LISTSCRIPTS permitted before authentication")
	}

	// STARTTLS.
	if err := client.send("STARTTLS"); err != nil {
		t.Fatal(err)
	}
	if status, startLines, err := client.readResponse(); err != nil || status != "OK" {
		t.Fatalf("STARTTLS: status=%q err=%v lines=%#v", status, err, startLines)
	}
	tlsConn := tls.Client(client.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	client.conn = tlsConn
	client.reader = bufio.NewReader(tlsConn)

	// Post-TLS capability advertises AUTH.
	if err := client.send("CAPABILITY"); err != nil {
		t.Fatal(err)
	}
	status, capabilityLines, err := client.readResponse()
	if err != nil || status != "OK" {
		t.Fatalf("CAPABILITY: %v", err)
	}
	if !strings.Contains(strings.Join(capabilityLines, "\n"), `AUTH "PLAIN"`) {
		t.Fatal("AUTH not advertised after TLS")
	}

	// Wrong password.
	wrong := base64.StdEncoding.EncodeToString([]byte("\x00rcpt@example.test\x00wrong"))
	if err := client.send("AUTHENTICATE PLAIN " + wrong); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil || status != "NO" {
		t.Fatal("AUTH accepted wrong password")
	}

	// Correct password.
	correct := base64.StdEncoding.EncodeToString([]byte("\x00rcpt@example.test\x00secret"))
	if err := client.send("AUTHENTICATE PLAIN " + correct); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil {
		t.Fatal(err)
	}
	mustBeOK(t, status)

	// LISTSCRIPTS initially empty.
	if err := client.send("LISTSCRIPTS"); err != nil {
		t.Fatal(err)
	}
	status, lines, err := client.readResponse()
	if err != nil || status != "OK" || len(lines) != 1 {
		t.Fatalf("empty LISTSCRIPTS: %#v", lines)
	}

	// PUTSCRIPT rejects invalid scripts.
	invalidScript := "not valid sieve"
	if err := client.send(fmt.Sprintf(`PUTSCRIPT "bad" {%d+}`, len(invalidScript))); err != nil {
		t.Fatal(err)
	}
	if err := client.send(invalidScript); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil || status != "NO" {
		t.Fatal("PUTSCRIPT accepted invalid script")
	}

	// PUTSCRIPT valid script using non-synchronized literal.
	script := `require ["fileinto", "vacation"];
if header :contains "Subject" "newsletter" { fileinto "Newsletter"; stop; }
vacation "I am away";`
	if err := client.send(fmt.Sprintf(`PUTSCRIPT "main" {%d+}`, len(script))); err != nil {
		t.Fatal(err)
	}
	if err := client.send(script); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil {
		t.Fatal(err)
	}
	mustBeOK(t, status)

	// Duplicate PUTSCRIPT is rejected.
	if err := client.send(fmt.Sprintf(`PUTSCRIPT "main" {%d+}`, len(script))); err != nil {
		t.Fatal(err)
	}
	if err := client.send(script); err != nil {
		t.Fatal(err)
	}
	if status, lines, err = client.readResponse(); err != nil || status != "NO" {
		t.Fatalf("duplicate PUTSCRIPT: status=%q lines=%#v", status, lines)
	}
	if !strings.Contains(lines[len(lines)-1], "SCRIPTEXISTS") {
		t.Fatalf("missing SCRIPTEXISTS code: %#v", lines)
	}

	// CHECKSCRIPT.
	if err := client.send(fmt.Sprintf(`CHECKSCRIPT {%d+}`, len(script))); err != nil {
		t.Fatal(err)
	}
	if err := client.send(script); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil || status != "OK" {
		t.Fatal("CHECKSCRIPT rejected valid script")
	}

	// SETACTIVE then LISTSCRIPTS shows ACTIVE.
	if err := client.send(`SETACTIVE "main"`); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil {
		t.Fatal(err)
	}
	mustBeOK(t, status)
	if err := client.send("LISTSCRIPTS"); err != nil {
		t.Fatal(err)
	}
	status, lines, err = client.readResponse()
	if err != nil || status != "OK" {
		t.Fatal(err)
	}
	if !strings.Contains(lines[0], `"main" ACTIVE`) {
		t.Fatalf("LISTSCRIPTS missing ACTIVE: %#v", lines)
	}

	// GETSCRIPT returns content verbatim.
	if err := client.send(`GETSCRIPT "main"`); err != nil {
		t.Fatal(err)
	}
	status, lines, err = client.readResponse()
	if err != nil || status != "OK" {
		t.Fatal(err)
	}
	if lines[0] != script {
		t.Fatal("GETSCRIPT content mismatch")
	}

	// GETSCRIPT missing script.
	if err := client.send(`GETSCRIPT "missing"`); err != nil {
		t.Fatal(err)
	}
	if status, lines, err = client.readResponse(); err != nil || status != "NO" {
		t.Fatalf("GETSCRIPT missing: status=%q %#v", status, lines)
	}

	// RENAMESCRIPT.
	if err := client.send(`RENAMESCRIPT "main" "renamed"`); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil {
		t.Fatal(err)
	}
	mustBeOK(t, status)

	// RENAMESCRIPT onto an existing name fails.
	if err := client.send(fmt.Sprintf(`PUTSCRIPT "other" {%d+}`, len(script))); err != nil {
		t.Fatal(err)
	}
	if err := client.send(script); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil {
		t.Fatal(err)
	}
	mustBeOK(t, status)
	if err := client.send(`RENAMESCRIPT "renamed" "other"`); err != nil {
		t.Fatal(err)
	}
	if status, lines, err = client.readResponse(); err != nil || status != "NO" {
		t.Fatalf("rename to existing: status=%q %#v", status, lines)
	}

	// DELETESCRIPT.
	if err := client.send(`DELETESCRIPT "renamed"`); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil {
		t.Fatal(err)
	}
	mustBeOK(t, status)
	if err := client.send(`GETSCRIPT "renamed"`); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil || status != "NO" {
		t.Fatal("deleted script still retrievable")
	}

	// SETACTIVE empty clears.
	if err := client.send(`SETACTIVE ""`); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil {
		t.Fatal(err)
	}
	mustBeOK(t, status)

	// LOGOUT.
	if err := client.send("LOGOUT"); err != nil {
		t.Fatal(err)
	}
	if status, _, err = client.readResponse(); err != nil || status != "OK" {
		t.Fatalf("LOGOUT: %v", err)
	}
}

func TestManageSieveCloseStopsListener(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &ManageSieveServer{Listener: listener, Root: env.root}
	runtimeErrors := make(chan error, 2)
	go server.Serve(runtimeErrors)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", listener.Addr().String()); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepting connections after Close")
	}
}

// Close forcibly ends sessions that are blocked on reads and returns in a
// bounded time even without a read deadline firing.
func TestManageSieveCloseUnblocksStalledSession(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &ManageSieveServer{Listener: listener, Root: env.root}
	runtimeErrors := make(chan error, 2)
	go server.Serve(runtimeErrors)

	client := dialManageSieve(t, listener.Addr().String())
	if _, err := client.readLine(); err != nil { // greeting
		t.Fatal(err)
	}

	start := time.Now()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("close blocked for %v", elapsed)
	}
	if err := client.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.readLine(); err == nil {
		t.Fatal("connection still open after Close")
	}
}

// Unknown commands return UNKNOWN-COMMAND even before authentication.
func TestManageSieveUnknownCommandBeforeAuth(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &ManageSieveServer{Listener: listener, Root: env.root}
	runtimeErrors := make(chan error, 2)
	go server.Serve(runtimeErrors)
	t.Cleanup(func() { _ = server.Close() })

	client := dialManageSieve(t, listener.Addr().String())
	t.Cleanup(func() { _ = client.conn.Close() })
	if _, err := client.readLine(); err != nil {
		t.Fatal(err)
	}
	if err := client.send("DOESNOTEXIST"); err != nil {
		t.Fatal(err)
	}
	line, err := client.readLine()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "NO") || !strings.Contains(line, "UNKNOWN-COMMAND") {
		t.Fatalf("unknown command response = %q", line)
	}
}

// A non-literal command token over the size limit is rejected and the session
// is closed, even before authentication.
func TestManageSieveRejectsOversizedToken(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &ManageSieveServer{Listener: listener, Root: env.root}
	runtimeErrors := make(chan error, 2)
	go server.Serve(runtimeErrors)
	t.Cleanup(func() { _ = server.Close() })

	client := dialManageSieve(t, listener.Addr().String())
	t.Cleanup(func() { _ = client.conn.Close() })
	if _, err := client.readLine(); err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("a", maxManageSieveCommandToken+10)
	if err := client.send("NOOP " + oversized); err != nil {
		t.Fatal(err)
	}
	line, err := client.readLine()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "NO") {
		t.Fatalf("oversized token response = %q", line)
	}
	if err := client.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.readLine(); err == nil {
		t.Fatal("connection still open after oversized token")
	}
}

// A literal over the size limit is rejected before the bytes are allocated or
// read, even before authentication.
func TestManageSieveRejectsOversizedLiteral(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &ManageSieveServer{Listener: listener, Root: env.root}
	runtimeErrors := make(chan error, 2)
	go server.Serve(runtimeErrors)
	t.Cleanup(func() { _ = server.Close() })

	client := dialManageSieve(t, listener.Addr().String())
	t.Cleanup(func() { _ = client.conn.Close() })
	if _, err := client.readLine(); err != nil { // greeting
		t.Fatal(err)
	}
	if err := client.send(`PUTSCRIPT "x" {2000000+}`); err != nil {
		t.Fatal(err)
	}
	line, err := client.readLine()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "NO") || !strings.Contains(line, "QUOTA") {
		t.Fatalf("oversized literal response = %q", line)
	}
}
