package resource

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/google/uuid"
)

func alwaysCanRefer(string, daptinid.DaptinReferenceId) bool { return true }

func createMailModSeqRequest(sessionUser *auth.SessionUser) api2go.Request {
	return api2go.Request{
		PlainRequest: (&http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Path: "/api/mail"},
		}).WithContext(context.WithValue(context.Background(), "user", sessionUser)),
	}
}

func mailboxReferenceID(t *testing.T, env sentMailTestEnv, boxId int64) string {
	t.Helper()
	var raw []byte
	if err := env.db.Get(&raw, `select reference_id from mail_box where id = ?`, boxId); err != nil {
		t.Fatalf("load mailbox reference: %v", err)
	}
	ref, err := uuid.FromBytes(raw)
	if err != nil {
		t.Fatalf("parse mailbox reference: %v", err)
	}
	return ref.String()
}

func createTestMail(t *testing.T, env sentMailTestEnv, boxRef string, attrs map[string]interface{}) map[string]interface{} {
	t.Helper()
	model := api2go.NewApi2GoModelWithData("mail", nil, 768, nil, attrs)
	req := createMailModSeqRequest(env.sessionUser)

	tx := env.db.MustBegin()
	result, err := env.root.createWithoutFilter(model, req, tx, alwaysCanRefer)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("createWithoutFilter: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return result
}

func TestCreateWithoutFilterAssignsPersistentModSeq(t *testing.T) {
	env := newSentMailTestEnv(t, true) // Sent box id=7, starts at highestmodseq 0
	boxRef := mailboxReferenceID(t, env, 7)

	result1 := createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef,
	})
	if got := result1["modseq"]; got != int64(1) {
		t.Fatalf("first mail modseq = %v, want 1", got)
	}
	highest, err := env.root.Cruds["mail_box"].GetMailBoxHighestModSeq(7, nil)
	if err != nil {
		t.Fatalf("read highestmodseq: %v", err)
	}
	if highest != 1 {
		t.Fatalf("highestmodseq = %d, want 1", highest)
	}

	result2 := createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef,
	})
	if got := result2["modseq"]; got != int64(2) {
		t.Fatalf("second mail modseq = %v, want 2", got)
	}
}

func TestCreateWithoutFilterIgnoresClientModSeq(t *testing.T) {
	env := newSentMailTestEnv(t, true)
	boxRef := mailboxReferenceID(t, env, 7)

	// Two prior creates: next server value must be 3 even though the client
	// attempts to supply modseq 999.
	createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})
	createTestMail(t, env, boxRef, map[string]interface{}{"mail_box_id": boxRef})

	result := createTestMail(t, env, boxRef, map[string]interface{}{
		"mail_box_id": boxRef,
		"modseq":      999,
	})
	if got := result["modseq"]; got != int64(3) {
		t.Fatalf("client-supplied modseq not overridden: got %v, want 3", got)
	}
}
