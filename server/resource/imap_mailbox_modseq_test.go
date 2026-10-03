package resource

import (
	"database/sql"
	"testing"

	"github.com/artpar/go-imap"
	daptinid "github.com/daptin/daptin/server/id"
)

var testMailBody = []byte("Subject: test\r\n\r\nhello\r\n")

func mustSeqSet(s string) *imap.SeqSet {
	set, err := imap.ParseSeqSet(s)
	if err != nil {
		panic(err)
	}
	return set
}

func encodedTestMailBody(t *testing.T, env sentMailTestEnv) interface{} {
	t.Helper()
	return env.root.MailColumnValue("mail", "mail", testMailBody, "")
}

func testDaptinMailbox(t *testing.T, env sentMailTestEnv) *DaptinImapMailBox {
	t.Helper()
	boxRef := mailboxReferenceID(t, env, 7)
	return &DaptinImapMailBox{
		name:               "Sent",
		mailBoxId:          7,
		mailAccountId:      1,
		mailBoxReferenceId: boxRef,
		dbResource:         env.root.Cruds,
		sessionUser:        env.sessionUser,
		info:               imap.MailboxInfo{Name: "Sent"},
	}
}

func collectMessages(t *testing.T, mailbox *DaptinImapMailBox, uid bool, set *imap.SeqSet,
	items []imap.FetchItem, since int64) []*imap.Message {
	t.Helper()
	ch := make(chan *imap.Message, 32)
	var err error
	if since > 0 {
		err = mailbox.FetchChanged(uid, set, items, since, ch)
	} else {
		go func() { _ = mailbox.ListMessages(uid, set, items, ch) }()
	}
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var out []*imap.Message
	for msg := range ch {
		out = append(out, msg)
	}
	return out
}

func msgModSeq(msg *imap.Message) int64 {
	v, ok := msg.Items["MODSEQ"].([]interface{})
	if !ok || len(v) != 1 {
		return 0
	}
	n, _ := internalIdFromValue(v[0])
	return n
}

func TestFetchChangedReturnsOnlyChangedMailsWithModSeq(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	mailbox := testDaptinMailbox(t, env)
	boxRef := mailboxReferenceID(t, env, 7)

	createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef, "mail": encodedTestMailBody(t, env),
		"size": int64(len(testMailBody)), "flags": "",
	})
	r2 := createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef, "mail": encodedTestMailBody(t, env),
		"size": int64(len(testMailBody)), "flags": "",
	})
	createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef, "mail": encodedTestMailBody(t, env),
		"size": int64(len(testMailBody)), "flags": "",
	})

	// External flag change on r2: takes modseq 4.
	ref2 := daptinid.InterfaceToDIR(r2["reference_id"])
	tx := env.db.MustBegin()
	if err := env.root.UpdateMailFlags(7, ref2, []string{`\Answered`}, env.sessionUser, tx); err != nil {
		t.Fatalf("external change: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	set, _ := imap.ParseSeqSet("1:*")
	msgs := collectMessages(t, mailbox, false, set, []imap.FetchItem{imap.FetchFlags}, 1)

	if len(msgs) != 2 {
		t.Fatalf("changed messages = %d, want 2", len(msgs))
	}
	// r1 modseq=1 excluded; the changed r2 (4) and r3 (3) remain.
	gotModSeqs := map[int64]bool{}
	for _, msg := range msgs {
		n := msgModSeq(msg)
		if n == 0 {
			t.Fatal("returned message missing MODSEQ")
		}
		gotModSeqs[n] = true
	}
	if !gotModSeqs[3] || !gotModSeqs[4] {
		t.Fatalf("modseqs %v, want 3,4", gotModSeqs)
	}
}

func TestListMessagesIncludesModSeqWhenRequested(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	mailbox := testDaptinMailbox(t, env)
	boxRef := mailboxReferenceID(t, env, 7)
	createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})

	set, _ := imap.ParseSeqSet("1:*")
	ch := make(chan *imap.Message, 4)
	if err := mailbox.ListMessages(false, set, []imap.FetchItem{"MODSEQ"}, ch); err != nil {
		t.Fatalf("list: %v", err)
	}
	for msg := range ch {
		if msgModSeq(msg) != 1 {
			t.Fatalf("explicit MODSEQ item = %d, want 1", msgModSeq(msg))
		}
	}
}

func TestVanishedSinceReportsSufficiencyAndSet(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	mailbox := testDaptinMailbox(t, env)
	boxRef := mailboxReferenceID(t, env, 7)

	r1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	ref1 := daptinid.InterfaceToDIR(r1["reference_id"])
	uid1 := mailEffectiveUID(t, env, ref1)
	deleteTestMail(t, env, ref1)

	// Watermark was set to 1 at deletion; deletion event is modseq 2.
	set, sufficient, err := mailbox.VanishedSince(1, nil)
	if err != nil {
		t.Fatalf("vanished: %v", err)
	}
	if !sufficient {
		t.Fatal("history must be sufficient when since >= watermark")
	}
	if !set.Contains(uid1) {
		t.Fatalf("set %q must contain %d", set.String(), uid1)
	}

	_, sufficient, err = mailbox.VanishedSince(0, nil)
	if err != nil {
		t.Fatalf("vanished: %v", err)
	}
	if sufficient {
		t.Fatal("history must be insufficient when since < watermark")
	}

	known := &imap.SeqSet{}
	known.AddNum(uid1 + 1) // a uid that was not deleted
	set2, sufficient, err := mailbox.VanishedSince(1, known)
	if err != nil || !sufficient {
		t.Fatalf("vanished restricted: %v sufficient=%v", err, sufficient)
	}
	if !set2.Empty() {
		t.Fatalf("intersection with unrelated known uid must be empty, got %q", set2.String())
	}
}

func TestConditionalStoreFlagsSkipsConflictingMails(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	mailbox := testDaptinMailbox(t, env)
	boxRef := mailboxReferenceID(t, env, 7)

	r1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	r2 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})

	// r1 is modified by another session: modseq advances 1 → 3.
	ref1 := daptinid.InterfaceToDIR(r1["reference_id"])
	tx := env.db.MustBegin()
	if err := env.root.UpdateMailFlags(7, ref1, []string{`\Answered`}, env.sessionUser, tx); err != nil {
		t.Fatalf("external change: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	set, _ := imap.ParseSeqSet("1:*")
	modified, _, err := mailbox.ConditionalStoreFlags(false, set, imap.AddFlags,
		[]string{`\Deleted`}, 2)
	if err != nil {
		t.Fatalf("conditional store: %v", err)
	}

	// r1 (modseq 3) fails and is reported by sequence number 1; r2 (modseq 2)
	// is updated.
	if !modified.Contains(1) || len(modified.Set) != 1 {
		t.Fatalf("modified set = %q, want seq 1", modified.String())
	}

	// r1 flags untouched; r2 carries \Deleted.
	r1Flags := flagsOfMail(t, env, ref1)
	if !FlagsEqual(FlagsFromStoredString(r1Flags), []string{`\Answered`}) {
		t.Fatalf("r1 flags = %q, must remain unchanged", r1Flags)
	}
	ref2 := daptinid.InterfaceToDIR(r2["reference_id"])
	r2Flags := flagsOfMail(t, env, ref2)
	if !FlagsEqual(FlagsFromStoredString(r2Flags), []string{`\Deleted`, `\Seen`}) {
		t.Fatalf("r2 flags = %q, want \\Deleted (\\Seen is implied by existing semantics)", r2Flags)
	}

	// UID variant: MODIFIED reports UIDs.
	r3 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	ref3 := daptinid.InterfaceToDIR(r3["reference_id"])
	tx2 := env.db.MustBegin()
	if err := env.root.UpdateMailFlags(7, ref3, []string{`\Answered`}, env.sessionUser, tx2); err != nil {
		t.Fatalf("external change r3: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	uidSet, _ := imap.ParseSeqSet("1:*")
	modifiedUid, _, err := mailbox.ConditionalStoreFlags(true, uidSet, imap.AddFlags,
		[]string{`\Deleted`}, 2)
	if err != nil {
		t.Fatalf("uid conditional store: %v", err)
	}
	uid3 := mailEffectiveUID(t, env, ref3)
	if !modifiedUid.Contains(uid3) {
		t.Fatalf("uid modified set %q must contain %d", modifiedUid.String(), uid3)
	}
}

func flagsOfMail(t *testing.T, env sentMailTestEnv, ref daptinid.DaptinReferenceId) string {
	t.Helper()
	var flags sql.NullString
	if err := env.db.Get(&flags, `select flags from mail where reference_id = ?`, ref[:]); err != nil {
		t.Fatalf("read flags: %v", err)
	}
	return flags.String
}

func TestMoveMessagesCopiesToDestinationAndDeletesSource(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	mailbox := testDaptinMailbox(t, env)
	boxRef := mailboxReferenceID(t, env, 7)

	// Destination box.
	createTestMailboxForMove(t, env)

	r1 := createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef, "mail": encodedTestMailBody(t, env),
		"size": int64(len(testMailBody)), "flags": `\Seen`,
	})
	sourceRef := daptinid.InterfaceToDIR(r1["reference_id"])
	sourceUID := mailEffectiveUID(t, env, sourceRef)

	if err := mailbox.MoveMessages(false, mustSeqSet("1"), "Destination"); err != nil {
		t.Fatalf("move: %v", err)
	}

	// Source empty; source deletion history recorded.
	var sourceCount int
	if err := env.db.Get(&sourceCount, `select count(*) from mail where mail_box_id = 7`); err != nil {
		t.Fatal(err)
	}
	if sourceCount != 0 {
		t.Fatalf("source mail count = %d, want 0", sourceCount)
	}
	tx := env.db.MustBegin()
	vanished, err := env.root.DeletionUIDsSince(7, 0, tx)
	if err != nil {
		t.Fatal(err)
	}
	if !vanished.Contains(sourceUID) {
		t.Fatalf("source deletion history %q must contain original uid %d", vanished.String(), sourceUID)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Destination has the moved mail with a fresh UID, Recent and preserved
	// flags/internal date semantics.
	var destCount int
	if err := env.db.Get(&destCount, `select count(*) from mail where mail_box_id = (
		select id from mail_box where name='Destination')`); err != nil {
		t.Fatal(err)
	}
	if destCount != 1 {
		t.Fatalf("destination count = %d, want 1", destCount)
	}
	var destFlags string
	var destUid int64
	var destModSeq sql.NullInt64
	err = env.db.QueryRowx(`select uid, flags, modseq from mail where mail_box_id = (
		select id from mail_box where name='Destination')`).Scan(&destUid, &destFlags, &destModSeq)
	if err != nil {
		t.Fatal(err)
	}
	if destUid != 1 {
		t.Fatalf("destination uid = %d, want newly allocated 1", destUid)
	}
	if !destModSeq.Valid || destModSeq.Int64 == 0 {
		t.Fatal("destination mail must have a positive modseq")
	}
	if !FlagsEqual(FlagsFromStoredString(destFlags), []string{`\Recent`, `\Seen`}) {
		t.Fatalf("destination flags = %q, want Recent + preserved Seen", destFlags)
	}
}

func TestMoveMessagesUnknownDestinationIsNoOp(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	mailbox := testDaptinMailbox(t, env)
	boxRef := mailboxReferenceID(t, env, 7)

	createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef, "mail": encodedTestMailBody(t, env),
		"size": int64(len(testMailBody)), "flags": "",
	})

	if err := mailbox.MoveMessages(false, mustSeqSet("1"), "NoSuchBox"); err == nil {
		t.Fatal("expected error for unknown destination")
	}

	var count int
	if err := env.db.Get(&count, `select count(*) from mail where mail_box_id = 7`); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("source must be unchanged on failed move, count = %d", count)
	}
}

func TestStatusReturnsHighestModSeq(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	mailbox := testDaptinMailbox(t, env)
	boxRef := mailboxReferenceID(t, env, 7)
	createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})

	status, err := mailbox.Status([]imap.StatusItem{"HIGHESTMODSEQ"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	got, ok := status.Items[imap.StatusItem("HIGHESTMODSEQ")]
	if !ok {
		t.Fatal("HIGHESTMODSEQ missing from status")
	}
	n, _ := internalIdFromValue(got)
	if n != 1 {
		t.Fatalf("HIGHESTMODSEQ = %d, want 1", n)
	}
}
