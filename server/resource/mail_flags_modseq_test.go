package resource

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"testing"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/google/uuid"
)

func patchMailRequest(sessionUser *auth.SessionUser) api2go.Request {
	return api2go.Request{
		PlainRequest: (&http.Request{
			Method: http.MethodPatch,
			URL:    &url.URL{Path: "/api/mail"},
		}).WithContext(context.WithValue(context.Background(), "user", sessionUser)),
	}
}

// createTestMailboxForMove creates a second mailbox through the standard
// resource creation path; its reference_id is returned in the result.
func createTestMailboxForMove(t *testing.T, env sentMailTestEnv) map[string]interface{} {
	t.Helper()
	var accountRefBytes []byte
	if err := env.db.Get(&accountRefBytes, `select reference_id from mail_account where id=1`); err != nil {
		t.Fatalf("load mail account reference: %v", err)
	}
	accountRef, err := uuid.FromBytes(accountRefBytes)
	if err != nil {
		t.Fatalf("parse mail account reference: %v", err)
	}
	model := api2go.NewApi2GoModelWithData("mail_box", nil, 16256, nil, map[string]interface{}{
		"name":            "Destination",
		"mail_account_id": accountRef.String(),
		"uidvalidity":     int64(2),
		"nextuid":         int64(1),
	})
	tx := env.db.MustBegin()
	result, err := env.root.Cruds["mail_box"].createWithoutFilter(model, createMailModSeqRequest(env.sessionUser), tx, alwaysCanRefer)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("create destination box: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit destination: %v", err)
	}
	if _, ok := result["reference_id"].(daptinid.DaptinReferenceId); !ok {
		t.Fatalf("destination box result missing reference_id: %#v", result["reference_id"])
	}
	return result
}

func readMailModSeq(t *testing.T, env sentMailTestEnv, ref daptinid.DaptinReferenceId) (int64, string) {
	t.Helper()
	var modseq int64
	var flags sql.NullString
	if err := env.db.QueryRowx(
		`select modseq, flags from mail where reference_id = ?`, ref[:]).Scan(&modseq, &flags); err != nil {
		t.Fatalf("read mail: %v", err)
	}
	return modseq, flags.String
}

func TestUpdateMailFlagsBumpsModSeq(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)
	r1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	ref := daptinid.InterfaceToDIR(r1["reference_id"])

	before, _ := readMailModSeq(t, env, ref)

	tx := env.db.MustBegin()
	err := env.root.UpdateMailFlags(7, ref, []string{`\Answered`}, env.sessionUser, tx)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("UpdateMailFlags: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, flags := readMailModSeq(t, env, ref)
	if after <= before {
		t.Fatalf("modseq must advance: %d → %d", before, after)
	}
	if !FlagsEqual(FlagsFromStoredString(flags), []string{`\Answered`}) {
		t.Fatalf("flags = %q, want \\Answered", flags)
	}
}

func TestJSONPatchChangedFlagsBumpsModSeq(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)
	r1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	ref := daptinid.InterfaceToDIR(r1["reference_id"])
	before, _ := readMailModSeq(t, env, ref)

	tx := env.db.MustBegin()
	model := api2go.NewApi2GoModelWithData("mail", nil, 0, nil, map[string]interface{}{
		"flags": `\Seen`,
	})
	model.SetID(ref.String())
	if _, err := env.root.updateWithoutFilters(model, patchMailRequest(env.sessionUser), tx, alwaysCanRefer); err != nil {
		_ = tx.Rollback()
		t.Fatalf("updateWithoutFilters: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, flags := readMailModSeq(t, env, ref)
	if after <= before {
		t.Fatalf("modseq must advance on external flag change: %d → %d", before, after)
	}
	if !FlagsEqual(FlagsFromStoredString(flags), []string{`\Seen`}) {
		t.Fatalf("flags = %q, want \\Seen", flags)
	}
}

func TestJSONPatchUnchangedFlagsKeepsModSeq(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)
	r1 := createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef,
		"flags":       `\Seen`,
	})
	ref := daptinid.InterfaceToDIR(r1["reference_id"])
	before, storedFlags := readMailModSeq(t, env, ref)

	tx := env.db.MustBegin()
	model := api2go.NewApi2GoModelWithData("mail", nil, 0, nil, map[string]interface{}{
		"flags": storedFlags, // same set, different ordering/casing allowed
	})
	model.SetID(ref.String())
	if _, err := env.root.updateWithoutFilters(model, patchMailRequest(env.sessionUser), tx, alwaysCanRefer); err != nil {
		_ = tx.Rollback()
		t.Fatalf("updateWithoutFilters: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, _ := readMailModSeq(t, env, ref)
	if after != before {
		t.Fatalf("modseq must not change when flags are equal: %d → %d", before, after)
	}
}

func TestJSONPatchMailboxMoveRecordsSourceHistoryAndBumpsDest(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)

	// Destination mailbox (fresh watermark 0).
	destBox := createTestMailboxForMove(t, env)
	destRef := destBox["reference_id"].(daptinid.DaptinReferenceId)

	r1 := createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef,
		"uid":         int64(42),
	})
	ref := daptinid.InterfaceToDIR(r1["reference_id"])

	tx := env.db.MustBegin()

	model := api2go.NewApi2GoModelWithData("mail", nil, 0, nil, map[string]interface{}{
		"mail_box_id": destRef.String(),
	})
	model.SetID(ref.String())
	if _, err := env.root.updateWithoutFilters(model, patchMailRequest(env.sessionUser), tx, alwaysCanRefer); err != nil {
		_ = tx.Rollback()
		t.Fatalf("updateWithoutFilters move: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// UID unchanged; the mail carries the destination box's new modseq.
	after, _ := readMailModSeq(t, env, ref)
	var destId int64
	if err := env.db.Get(&destId, `select id from mail_box where reference_id = ?`, destRef[:]); err != nil {
		t.Fatalf("resolve destination id: %v", err)
	}
	destHighest, err := env.root.Cruds["mail_box"].GetMailBoxHighestModSeq(destId, nil)
	if err != nil {
		t.Fatalf("read destination highestmodseq: %v", err)
	}
	if after != destHighest || destHighest != 1 {
		t.Fatalf("mail modseq %d must equal destination highestmodseq %d", after, destHighest)
	}

	var uid int64
	if err := env.db.Get(&uid, `select uid from mail where reference_id = ?`, ref[:]); err != nil {
		t.Fatalf("read uid: %v", err)
	}
	if uid != 42 {
		t.Fatalf("uid = %d, must remain 42 on API move", uid)
	}

	// Source box deletion history contains the moved UID.
	tx2 := env.db.MustBegin()
	vanished, err := env.root.DeletionUIDsSince(7, 0, tx2)
	if err != nil {
		t.Fatalf("deletionUIDsSince source: %v", err)
	}
	if !vanished.Contains(42) {
		t.Fatalf("source deletion history %q must contain uid 42", vanished.String())
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
