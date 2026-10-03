package resource

import (
	"bufio"
	"bytes"
	"fmt"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func deliverMessage(t *testing.T, env *sieveStoreTestEnv, mailBytes []byte, envelopeFrom string) error {
	t.Helper()
	tx := env.db.MustBegin()
	info := InboundMessageInfo{
		Recipient:       "rcpt@example.test",
		ListenInterface: "test-listener",
		ReturnPath:      envelopeFrom,
	}
	err := env.root.DeliverInboundWithSieve(env.mailAccountId, env.sessionUser,
		mailBytes, info, envelopeFrom, 0, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func activateTestScript(t *testing.T, env *sieveStoreTestEnv, content string) {
	t.Helper()
	tx := env.db.MustBegin()
	if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef,
		env.sessionUser, "active", content, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("create script: %v", err)
	}
	if err := env.root.SetActiveSieveScript(env.mailAccountId, "active", env.sessionUser, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("activate script: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func deliverDefault(t *testing.T, env *sieveStoreTestEnv, mailBytes []byte, envelopeFrom string, spamScore int) error {
	t.Helper()
	tx := env.db.MustBegin()
	info := InboundMessageInfo{
		Recipient:       "rcpt@example.test",
		ListenInterface: "test-listener",
		ReturnPath:      envelopeFrom,
	}
	err := env.root.DeliverInboundDefault(env.mailAccountId, env.sessionUser, mailBytes, info, spamScore, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func countRows(t *testing.T, db *sqlx.DB, table string, where string, args ...interface{}) int {
	t.Helper()
	query := "select count(*) from " + table
	if where != "" {
		query += " where " + where
	}
	var count int
	if err := db.QueryRowx(query, args...).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func mailboxNames(t *testing.T, env *sieveStoreTestEnv) []string {
	t.Helper()
	boxRows, err := env.db.Queryx("select name from mail_box")
	if err != nil {
		t.Fatal(err)
	}
	defer boxRows.Close()
	names := []string{}
	for boxRows.Next() {
		var name string
		if err := boxRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func mailBoxNameOfMail(t *testing.T, env *sieveStoreTestEnv, index int) string {
	t.Helper()
	// mail_box_id is conformed to the internal mailbox id at write time.
	row := env.db.QueryRowx(`select mb.name from mail m join mail_box mb
		on m.mail_box_id = mb.id order by m.id limit 1 offset ?`, index)
	var name string
	if err := row.Scan(&name); err != nil {
		t.Fatalf("mailbox of mail %d: %v", index, err)
	}
	return name
}

func outboxRows(t *testing.T, env *sieveStoreTestEnv) []map[string]interface{} {
	t.Helper()
	tx := env.db.MustBegin()
	defer tx.Rollback()
	rows, _, err := env.root.GetRowsByWhereClauseWithTransaction("outbox", map[string]bool{"mail": true}, tx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// makeMail builds a minimal RFC822 message with the given headers.
func makeMail(headers ...[2]string) []byte {
	var builder bytes.Buffer
	builder.WriteString("Date: Mon, 1 Jan 2024 00:00:00 +0000\r\n")
	if !hasHeader(headers, "Message-Id") {
		builder.WriteString("Message-Id: <msg-1@example.test>\r\n")
	}
	for _, header := range headers {
		builder.WriteString(header[0] + ": " + header[1] + "\r\n")
	}
	if !hasHeader(headers, "From") {
		builder.WriteString("From: sender@example.test\r\n")
	}
	if !hasHeader(headers, "To") {
		builder.WriteString("To: rcpt@example.test\r\n")
	}
	if !hasHeader(headers, "Subject") {
		builder.WriteString("Subject: test\r\n")
	}
	builder.WriteString("\r\nHello body\r\n")
	return builder.Bytes()
}

func hasHeader(headers [][2]string, name string) bool {
	for _, header := range headers {
		if strings.EqualFold(header[0], name) {
			return true
		}
	}
	return false
}

func parseReplyHeader(t *testing.T, raw []byte) textproto.MIMEHeader {
	t.Helper()
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	header, err := reader.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("parse reply: %v", err)
	}
	return header
}

// ---------------------------------------------------------------------------
// Default path regression
// ---------------------------------------------------------------------------

func TestDeliverDefaultNoScriptKeepsExistingBehavior(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	mail := makeMail()

	if err := deliverDefault(t, env, mail, "sender@example.test", 0); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if countRows(t, env.db, "mail", "") != 1 {
		t.Fatal("mail count != 1")
	}
	if mailBoxNameOfMail(t, env, 0) != "INBOX" {
		t.Fatal("default mail not in INBOX")
	}
}

func TestDeliverDefaultSpamRoutingUnchanged(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	mail := makeMail()

	if err := deliverDefault(t, env, mail, "sender@example.test", 350); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if mailBoxNameOfMail(t, env, 0) != "Spam" {
		t.Fatal("spam mail not in Spam mailbox")
	}
}

// ---------------------------------------------------------------------------
// Conditions
// ---------------------------------------------------------------------------

func TestSieveHeaderCondition(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `require ["fileinto"];
		if header :contains "X-Newsletter" ["yes"] { fileinto "Newsletter"; }`)

	mail := makeMail([2]string{"X-Newsletter", "yes"})
	if err := deliverMessage(t, env, mail, "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if mailBoxNameOfMail(t, env, 0) != "Newsletter" {
		t.Fatal("header match not filed into Newsletter")
	}

	env2 := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env2, `require ["fileinto"];
		if header :is "Subject" "test" { fileinto "Exact"; }`)
	if err := deliverMessage(t, env2, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if mailBoxNameOfMail(t, env2, 0) != "Exact" {
		t.Fatal("header :is match failed")
	}
}

func TestSieveHeaderNoMatchImplicitKeep(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `require ["fileinto"];
		if header :contains "X-Nope" "yes" { fileinto "Other"; }`)

	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if countRows(t, env.db, "mail", "") != 1 || mailBoxNameOfMail(t, env, 0) != "INBOX" {
		t.Fatal("implicit keep to INBOX failed")
	}
}

func TestSieveAddressCondition(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `require ["fileinto"];
		if address :domain :is "from" "example.test" { fileinto "Domain"; }`)
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if mailBoxNameOfMail(t, env, 0) != "Domain" {
		t.Fatal("address domain test failed")
	}
}

func TestSieveSizeCondition(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `require ["fileinto"];
		if size :over 10 { fileinto "Big"; }`)
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if mailBoxNameOfMail(t, env, 0) != "Big" {
		t.Fatal("size over test failed")
	}
}

func TestSieveBodyCondition(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `require ["body", "fileinto"];
		if body :contains "Hello body" { fileinto "BodyMatch"; }`)
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if mailBoxNameOfMail(t, env, 0) != "BodyMatch" {
		t.Fatal("body test failed")
	}
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

func TestSieveDiscardAndStop(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `discard;`)
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if countRows(t, env.db, "mail", "") != 0 {
		t.Fatal("discard left a mail row")
	}

	env2 := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env2, `require ["fileinto"];
		if true { stop; fileinto "Unreached"; }`)
	if err := deliverMessage(t, env2, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if countRows(t, env2.db, "mail_box", "name = 'Unreached'") != 0 {
		t.Fatal("commands after stop executed")
	}
}

func TestSieveFileintoCreatesMailbox(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `require ["fileinto"]; fileinto "AutomaticallyCreated";`)
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	found := false
	for _, name := range mailboxNames(t, env) {
		if name == "AutomaticallyCreated" {
			found = true
		}
	}
	if !found {
		t.Fatal("fileinto did not create target mailbox")
	}
}

func TestSieveRedirectEntersOutboxAndPreventsLoops(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `redirect "other@external.test";`)
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	rows := outboxRows(t, env)
	if len(rows) != 1 || rows[0]["to_address"] != "other@external.test" {
		t.Fatalf("redirect outbox rows: %#v", rows)
	}

	// Duplicate redirect: only one row.
	env2 := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env2, `redirect "dup@external.test"; redirect "dup@external.test";`)
	if err := deliverMessage(t, env2, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if rows := outboxRows(t, env2); len(rows) != 1 {
		t.Fatalf("duplicate redirect produced %d rows", len(rows))
	}

	// Redirect back to the original sender: no rows.
	env3 := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env3, `redirect "sender@example.test";`)
	if err := deliverMessage(t, env3, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if rows := outboxRows(t, env3); len(rows) != 0 {
		t.Fatalf("redirect to sender produced %d rows", len(rows))
	}

	// Redirect to the recipient's own address: no rows.
	env4 := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env4, `redirect "rcpt@example.test";`)
	if err := deliverMessage(t, env4, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if rows := outboxRows(t, env4); len(rows) != 0 {
		t.Fatalf("redirect to self produced %d rows", len(rows))
	}
}

// ---------------------------------------------------------------------------
// Atomicity
// ---------------------------------------------------------------------------

func TestDeliveryFailureRollsBackCompletely(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `redirect "this is not an address";`)

	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err == nil {
		t.Fatal("invalid redirect address accepted")
	}
	if countRows(t, env.db, "mail", "") != 0 ||
		countRows(t, env.db, "outbox", "") != 0 ||
		countRows(t, env.db, "mail_box", "") != 0 {
		t.Fatal("failed delivery left partial rows")
	}
}

// ---------------------------------------------------------------------------
// Vacation
// ---------------------------------------------------------------------------

func vacationScript(tags string) string {
	return `require ["vacation"]; vacation ` + tags + ` "I am on vacation";`
}

func TestVacationSkipsListsAutomatedAndNullSender(t *testing.T) {
	cases := []struct {
		name     string
		from     string
		headers  [][2]string
	}{
		{"list", "sender@example.test", [][2]string{{"List-Id", "<list.example.test>"}}},
		{"precedence", "sender@example.test", [][2]string{{"Precedence", "bulk"}}},
		{"auto-submitted", "sender@example.test", [][2]string{{"Auto-Submitted", "auto-generated"}}},
		{"x-autoreply", "sender@example.test", [][2]string{{"X-Autoreply", "yes"}}},
		{"null-sender", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSieveStoreTestEnv(t, false)
			activateTestScript(t, env, vacationScript(`:days 7`))
			if err := deliverMessage(t, env, makeMail(tc.headers...), tc.from); err != nil {
				t.Fatalf("deliver: %v", err)
			}
			if rows := outboxRows(t, env); len(rows) != 0 {
				t.Fatalf("%s mail produced a vacation reply: %#v", tc.name, rows)
			}
		})
	}
}

func TestVacationRepliesToPersonalMailWithCorrectFormat(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, vacationScript(`:days 7`))
	mail := makeMail([2]string{"Message-Id", "<original@example.test>"})
	if err := deliverMessage(t, env, mail, "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	rows := outboxRows(t, env)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d", len(rows))
	}
	if rows[0]["from_address"] != "" {
		t.Fatal("vacation MAIL FROM must be empty")
	}
	raw, err := env.root.MailColumnBytes("outbox", "mail", rows[0]["mail"])
	if err != nil {
		t.Fatal(err)
	}
	header := parseReplyHeader(t, raw)
	if header.Get("Auto-Submitted") != "auto-replied" {
		t.Fatalf("auto-submitted = %q", header.Get("Auto-Submitted"))
	}
	if header.Get("In-Reply-To") != "<original@example.test>" {
		t.Fatalf("in-reply-to = %q", header.Get("In-Reply-To"))
	}
	if header.Get("References") != "<original@example.test>" {
		t.Fatalf("references = %q", header.Get("References"))
	}
	if !strings.HasPrefix(header.Get("Subject"), "Re:") {
		t.Fatalf("subject = %q", header.Get("Subject"))
	}
	if !strings.Contains(string(raw), "I am on vacation") {
		t.Fatal("reply body missing reason")
	}
}

func TestVacationDedupBySenderAndHandle(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, vacationScript(`:days 7`))

	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}
	// Same sender within the period: no second reply.
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, env); len(rows) != 1 {
		t.Fatalf("expected 1 reply within period, got %d", len(rows))
	}
	// Different sender: another reply.
	if err := deliverMessage(t, env, makeMail(), "other@example.test"); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, env); len(rows) != 2 {
		t.Fatalf("expected 2 replies for different senders, got %d", len(rows))
	}

	// Explicit different handle: replies independently.
	env2 := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env2, `require ["vacation"];
		vacation :handle "trip" :days 7 "away";
		vacation :handle "other-trip" :days 7 "away other";`)
	if err := deliverMessage(t, env2, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, env2); len(rows) != 2 {
		t.Fatalf("different handles produced %d replies", len(rows))
	}
}

func TestVacationRepliesAgainAfterPeriod(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, vacationScript(`:days 1`))

	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}

	// Backdate the tracking record beyond the period.
	if _, err := env.db.Exec(
		"update vacation_track set last_sent_at = ?", time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, env); len(rows) != 2 {
		t.Fatalf("expected reply after period, got %d rows", len(rows))
	}
}

func TestVacationDedupSurvivesRestart(t *testing.T) {
	env := newSieveStoreTestEnv(t, true)
	activateTestScript(t, env, vacationScript(`:days 7`))
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}

	path := env.dbPath
	_ = env.db.Close()
	reopened, err := sqlx.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	env.db = reopened
	env.root = sieveCrudGraphFromDB(t, reopened)

	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, env); len(rows) != 1 {
		t.Fatalf("replies after restart = %d, want 1", len(rows))
	}
}

func TestVacationConcurrentDeliveriesProduceOneReply(t *testing.T) {
	env := newSieveStoreTestEnv(t, true)
	env.db.SetMaxOpenConns(4)
	activateTestScript(t, env, vacationScript(`:days 7`))

	var waitGroup sync.WaitGroup
	deliveryErrors := make(chan error, 2)
	for index := 0; index < 2; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			deliveryErrors <- deliverMessage(t, env, makeMail(), "sender@example.test")
		}()
	}
	waitGroup.Wait()
	close(deliveryErrors)
	for deliveryErr := range deliveryErrors {
		if deliveryErr != nil {
			// The loser of the unique-key race fails its whole transaction; the
			// caller retries and then finds the active dedup window.
			t.Logf("concurrent delivery error (allowed): %v", deliveryErr)
		}
	}
	if rows := outboxRows(t, env); len(rows) != 1 {
		t.Fatalf("concurrent replies = %d, want exactly 1", len(rows))
	}
}

func TestVacationMimeReasonQueuedVerbatim(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	// The MIME entity is supplied with the multi-line text: form; the lexer
	// normalizes line endings to CRLF.
	rawEntity := "From: rcpt@example.test\r\nContent-Type: text/plain\r\n\r\ncustom mime body\r\n"
	program := "require [\"vacation\"]; vacation :mime :days 7 text:\r\n" +
		strings.ReplaceAll(rawEntity, "\r\n", "\n") + ".\r\n;"
	activateTestScript(t, env, program)

	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	rows := outboxRows(t, env)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d", len(rows))
	}
	raw, err := env.root.MailColumnBytes("outbox", "mail", rows[0]["mail"])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != rawEntity {
		t.Fatalf("mime entity = %q, want %q", raw, rawEntity)
	}
}

func TestVacationDaysZeroAlwaysReplies(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, vacationScript(`:days 0`))
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, env); len(rows) != 2 {
		t.Fatalf("days 0 produced %d replies, want 2", len(rows))
	}
}

// Too many Received hops suppress redirects.
func TestSieveRedirectSuppressedAfterHopLimit(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	activateTestScript(t, env, `redirect "other@external.test";`)
	headers := make([][2]string, 0, sieveMaxMailHops)
	for index := 0; index < sieveMaxMailHops; index++ {
		headers = append(headers, [2]string{"Received", "from upstream.example.test"})
	}
	if err := deliverMessage(t, env, makeMail(headers...), "sender@example.test"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if rows := outboxRows(t, env); len(rows) != 0 {
		t.Fatalf("redirect after hop limit produced %d rows", len(rows))
	}
}

// More than the maximum number of redirects fails execution and rolls back.
func TestSieveRedirectOverMaximumFails(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	var builder strings.Builder
	for index := 0; index <= 5; index++ {
		builder.WriteString(fmt.Sprintf(`redirect "target%d@external.test";`+"\n", index+1))
	}
	activateTestScript(t, env, builder.String())

	if err := deliverMessage(t, env, makeMail(), "sender@example.test"); err == nil {
		t.Fatal("script over maximum redirect count accepted")
	}
	if countRows(t, env.db, "outbox", "") != 0 || countRows(t, env.db, "mail", "") != 0 {
		t.Fatal("failed redirect batch left rows")
	}
}

// Writes made inside a transaction are fully discarded when a later action
// fails and the transaction rolls back, including mailbox creation and UID
// allocation.
func TestRollbackAfterWritesDiscardsEverything(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	tx := env.db.MustBegin()

	box, err := env.root.ResolveOrCreateMailBox(env.mailAccountId, "INBOX", env.sessionUser, tx)
	if err != nil {
		t.Fatal(err)
	}
	info := InboundMessageInfo{
		Recipient: "rcpt@example.test",
		Hash:      "hash123",
	}
	if err := env.root.appendInboundMail(box, env.sessionUser, makeMail(), info, 0, tx); err != nil {
		t.Fatal(err)
	}

	// Trigger a deterministic failure: no mail server matches the listener.
	if err := env.root.EnqueueOutboxMail("", "a@external.test", "missing-listener", []byte("x"), tx); err == nil {
		t.Fatal("expected outbox enqueue failure")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	if countRows(t, env.db, "mail", "") != 0 ||
		countRows(t, env.db, "mail_box", "") != 0 ||
		countRows(t, env.db, "outbox", "") != 0 {
		t.Fatal("rollback left persisted rows")
	}
}
