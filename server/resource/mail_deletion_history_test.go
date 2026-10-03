package resource

import (
	"database/sql"
	"testing"

	"github.com/artpar/api2go/v2"
	"github.com/artpar/go-imap"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/doug-martin/goqu/v9"
)

func deleteTestMail(t *testing.T, env sentMailTestEnv, referenceID daptinid.DaptinReferenceId) {
	t.Helper()
	tx := env.db.MustBegin()
	_, err := env.root.deleteAfterAuthorizationWithTransaction(referenceID, createMailModSeqRequest(env.sessionUser), tx)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("delete mail: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// mailEffectiveUID resolves the IMAP-visible UID of a mail from the database:
// the create result intentionally omits the internal id.
func mailEffectiveUID(t *testing.T, env sentMailTestEnv, referenceID daptinid.DaptinReferenceId) uint32 {
	t.Helper()
	var raw struct {
		ID  int64         `db:"id"`
		UID sql.NullInt64 `db:"uid"`
	}
	if err := env.db.Get(&raw, `select id, uid from mail where reference_id = ?`, referenceID[:]); err != nil {
		t.Fatalf("load mail id/uid: %v", err)
	}
	if raw.UID.Valid && raw.UID.Int64 > 0 {
		return uint32(raw.UID.Int64)
	}
	return uint32(raw.ID)
}

func allDeletionRows(t *testing.T, env sentMailTestEnv) []map[string]interface{} {
	t.Helper()
	tx := env.db.MustBegin()
	rows, _, err := env.root.Cruds["mail_deletion"].GetRowsByWhereClauseWithTransaction(
		"mail_deletion", nil, tx, goqu.Ex{"mail_box_id": 7})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("load deletion rows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rows
}

func TestDeleteWritesDeletionHistoryPerMail(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)

	result1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	result2 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})

	ref1 := daptinid.InterfaceToDIR(result1["reference_id"])
	ref2 := daptinid.InterfaceToDIR(result2["reference_id"])
	uid1 := mailEffectiveUID(t, env, ref1)
	uid2 := mailEffectiveUID(t, env, ref2)
	deleteTestMail(t, env, ref1)
	deleteTestMail(t, env, ref2)

	rows := allDeletionRows(t, env)
	if len(rows) != 2 {
		t.Fatalf("deletion history rows = %d, want 2", len(rows))
	}
	// Deletion modseqs follow the box sequence: creates took 1,2 → deletes 3,4.
	gotModseqs := map[int64]bool{}
	for _, row := range rows {
		modseq, _ := internalIdFromValue(row["modseq"])
		gotModseqs[modseq] = true
		if uid, _ := internalIdFromValue(row["uid"]); uid == 0 {
			t.Fatal("deletion history uid must be positive")
		}
	}
	if !gotModseqs[3] || !gotModseqs[4] {
		t.Fatalf("deletion modseqs = %v, want 3,4", gotModseqs)
	}
	_ = uid1
	_ = uid2
}

func TestDeletionUIDsSinceAggregatesRanges(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)

	r1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	r2 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	ref1 := daptinid.InterfaceToDIR(r1["reference_id"])
	uid1 := mailEffectiveUID(t, env, ref1)
	uid2 := mailEffectiveUID(t, env, daptinid.InterfaceToDIR(r2["reference_id"]))
	deleteTestMail(t, env, ref1)
	deleteTestMail(t, env, daptinid.InterfaceToDIR(r2["reference_id"]))

	tx := env.db.MustBegin()
	set, err := env.root.DeletionUIDsSince(7, 0, tx)
	if err != nil {
		t.Fatalf("deletionUIDsSince: %v", err)
	}
	if !set.Contains(uid1) || !set.Contains(uid2) {
		t.Fatalf("set %q missing effective uids %d,%d", set.String(), uid1, uid2)
	}

	empty, err := env.root.DeletionUIDsSince(7, 4, tx)
	if err != nil {
		t.Fatalf("deletionUIDsSince high: %v", err)
	}
	if !empty.Empty() {
		t.Fatalf("set above all deletions must be empty, got %q", empty.String())
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestDeletionUIDsRestrictedToKnownSet(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)

	r1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	r2 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	ref1 := daptinid.InterfaceToDIR(r1["reference_id"])
	uid1 := mailEffectiveUID(t, env, ref1)
	uid2 := mailEffectiveUID(t, env, daptinid.InterfaceToDIR(r2["reference_id"]))
	deleteTestMail(t, env, ref1)
	deleteTestMail(t, env, daptinid.InterfaceToDIR(r2["reference_id"]))

	known := &imap.SeqSet{}
	known.AddNum(uid1)

	tx := env.db.MustBegin()
	set, err := env.root.DeletionUIDsInSet(7, 0, known, tx)
	if err != nil {
		t.Fatalf("deletionUIDsInSet: %v", err)
	}
	if !set.Contains(uid1) {
		t.Fatalf("set %q must contain known uid %d", set.String(), uid1)
	}
	if set.Contains(uid2) {
		t.Fatalf("set %q must not contain uid %d outside known set", set.String(), uid2)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestWatermarkRecordsBoundaryForLegacyMailbox(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)

	tx0 := env.db.MustBegin()
	w, initialized, err := env.root.Cruds["mail_box"].GetDeletionHistoryWatermark(7, tx0)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if initialized {
		t.Fatal("legacy mailbox must start with uninitialized watermark")
	}
	_ = w
	if err := tx0.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// One mail creation takes highestmodseq to 1; first delete then records
	// boundary 1 and the deletion event itself at modseq 2.
	r1 := createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	ref1 := daptinid.InterfaceToDIR(r1["reference_id"])
	uid1 := mailEffectiveUID(t, env, ref1)
	deleteTestMail(t, env, ref1)

	tx := env.db.MustBegin()
	watermark, initialized, err := env.root.Cruds["mail_box"].GetDeletionHistoryWatermark(7, tx)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if !initialized {
		t.Fatal("watermark must be initialized after first deletion")
	}
	if watermark != 1 {
		t.Fatalf("watermark = %d, want 1", watermark)
	}

	set, err := env.root.DeletionUIDsSince(7, watermark, tx)
	if err != nil {
		t.Fatalf("deletionUIDsSince at watermark: %v", err)
	}
	if !set.Contains(uid1) {
		t.Fatalf("deletion at modseq %d must be returned when since = watermark", watermark+1)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestNewMailboxCreationStartsWithZeroWatermark(t *testing.T) {
	env := newSentMailTestEnv(t, false)

	model := api2go.NewApi2GoModelWithData("mail_box", nil, 16256, nil, map[string]interface{}{
		"name":            "Fresh",
		"mail_account_id": "1",
		"uidvalidity":     int64(1),
		"nextuid":         int64(1),
	})
	tx := env.db.MustBegin()
	result, err := env.root.createWithoutFilter(model, createMailModSeqRequest(env.sessionUser), tx, alwaysCanRefer)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("create mailbox: %v", err)
	}
	if got, _ := internalIdFromValue(result["deletion_history_modseq"]); got != 0 {
		t.Fatalf("new mailbox watermark = %d, want 0", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
