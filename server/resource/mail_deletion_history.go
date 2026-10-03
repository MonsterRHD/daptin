package resource

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/artpar/api2go/v2"
	"github.com/artpar/go-imap"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

// EffectiveMailUID returns the UID exposed over IMAP for a mail row: the
// explicit UID when assigned, otherwise the internal id, matching the sequence
// resolution used by SelectMailBoxCandidates.
func EffectiveMailUID(data map[string]interface{}) uint32 {
	if uid, ok := internalIdFromValue(data["uid"]); ok && uid > 0 {
		return uint32(uid)
	}
	if id, ok := internalIdFromValue(data["id"]); ok && id > 0 {
		return uint32(id)
	}
	return 0
}

// RecordMailDeletion persists one entry of the mailbox deletion history: the
// expunged UID together with the modseq assigned to the deletion event.
func (dbResource *DbResource) RecordMailDeletion(mailBoxId int64, uid uint32, modseq int64,
	req api2go.Request, transaction *sqlx.Tx) error {
	mailDeletionCrud, ok := dbResource.Cruds["mail_deletion"]
	if !ok {
		return errors.New("mail_deletion resource is not configured")
	}

	boxReference, err := GetIdToReferenceIdWithTransaction("mail_box", mailBoxId, transaction)
	if err != nil {
		return err
	}

	model := api2go.NewApi2GoModelWithData("mail_deletion", nil, 768, nil, map[string]interface{}{
		"mail_box_id": boxReference.String(),
		"uid":         int64(uid),
		"modseq":      modseq,
	})

	requestURL, _ := url.Parse("/api/mail_deletion")
	plainRequest := &http.Request{Method: http.MethodPost, URL: requestURL}
	if req.PlainRequest != nil {
		plainRequest = plainRequest.WithContext(req.PlainRequest.Context())
	}
	createRequest := api2go.Request{PlainRequest: plainRequest}

	_, err = mailDeletionCrud.createAfterAuthorizationWithTransaction(model, createRequest, transaction)
	return err
}

// DeletionUIDsSince returns all UIDs whose deletion was recorded at a modseq
// strictly greater than since, aggregated into a compact UID set.
func (dbResource *DbResource) DeletionUIDsSince(mailBoxId int64, since int64, transaction *sqlx.Tx) (*imap.SeqSet, error) {
	return dbResource.deletionUIDs(mailBoxId, since, nil, transaction)
}

// DeletionUIDsInSet restricts DeletionUIDsSince to UIDs contained in knownUIDs.
func (dbResource *DbResource) DeletionUIDsInSet(mailBoxId int64, since int64, knownUIDs *imap.SeqSet,
	transaction *sqlx.Tx) (*imap.SeqSet, error) {
	return dbResource.deletionUIDs(mailBoxId, since, knownUIDs, transaction)
}

func (dbResource *DbResource) deletionUIDs(mailBoxId int64, since int64, knownUIDs *imap.SeqSet,
	transaction *sqlx.Tx) (*imap.SeqSet, error) {
	query, args, err := statementbuilder.Squirrel.
		Select("uid").
		From("mail_deletion").
		Where(goqu.Ex{"mail_box_id": mailBoxId}).
		Where(goqu.C("modseq").Gt(since)).
		Order(goqu.C("uid").Asc()).
		Prepared(true).
		ToSQL()
	if err != nil {
		return nil, err
	}

	rows, err := transaction.Queryx(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &imap.SeqSet{}
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		if knownUIDs != nil && !knownUIDs.Contains(uint32(uid)) {
			continue
		}
		result.AddNum(uint32(uid))
	}
	return result, rows.Err()
}
