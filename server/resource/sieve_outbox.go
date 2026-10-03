package resource

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/jmoiron/sqlx"
)

// internalAdministratorUser builds the trusted backend identity used for
// queues created as a side effect of mail delivery.
func (dbResource *DbResource) internalAdministratorUser() *auth.SessionUser {
	user := &auth.SessionUser{}
	if userAccountCrud := dbResource.Cruds[USER_ACCOUNT_TABLE_NAME]; userAccountCrud != nil &&
		userAccountCrud.AdministratorGroupId != daptinid.NullReferenceId {
		user.Groups = append(user.Groups, auth.GroupPermission{
			GroupReferenceId: userAccountCrud.AdministratorGroupId,
		})
	}
	return user
}

func (dbResource *DbResource) resolveOutboundMailServer(listenInterface string,
	transaction *sqlx.Tx) (map[string]interface{}, error) {
	if strings.TrimSpace(listenInterface) != "" {
		if obj, err := dbResource.Cruds["mail_server"].GetObjectByWhereClause(
			"mail_server", "listen_interface", listenInterface, transaction); err == nil && obj != nil {
			return obj, nil
		}
	}
	return nil, errors.New("mail server not found for outbound queue")
}

// EnqueueOutboxMail creates one outbox row inside the transaction, reusing
// the existing outgoing queue for Sieve redirects and auto replies.
func (dbResource *DbResource) EnqueueOutboxMail(fromAddress, toAddress, listenInterface string,
	message []byte, transaction *sqlx.Tx) error {
	mailServerObj, err := dbResource.resolveOutboundMailServer(listenInterface, transaction)
	if err != nil {
		return err
	}

	toHost := toAddress
	if at := strings.LastIndex(toAddress, "@"); at >= 0 {
		toHost = toAddress[at+1:]
	}

	hash := GetMD5Hash(message)
	outboxBody := dbResource.Cruds["outbox"].MailColumnValue("outbox", "mail", message, hash)
	model := api2go.NewApi2GoModelWithData("outbox", nil, 0, nil, map[string]interface{}{
		"from_address":   fromAddress,
		"to_address":     toAddress,
		"to_host":        toHost,
		"mail_server_id": mailServerObj["reference_id"],
		"mail":           outboxBody,
		"sent":           false,
		"retry_count":    0,
		"next_retry_at":  time.Now(),
	})

	outboxURL, _ := url.Parse("/api/outbox")
	request := api2go.Request{PlainRequest: (&http.Request{
		Method: http.MethodPost,
		URL:    outboxURL,
	}).WithContext(context.WithValue(context.Background(), "user", dbResource.internalAdministratorUser()))}

	result, err := dbResource.Cruds["outbox"].CreateWithoutFilter(model, request, transaction)
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("failed to enqueue outbox mail")
	}
	return nil
}
