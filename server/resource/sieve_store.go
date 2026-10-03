package resource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

const sieveScriptTableName = "sieve_script"

// ListSieveScripts returns all scripts owned by the mail account.
func (dbResource *DbResource) ListSieveScripts(mailAccountId int64, transaction *sqlx.Tx) ([]map[string]interface{}, error) {
	return dbResource.GetAllObjectsWithWhereWithTransaction(sieveScriptTableName, transaction,
		goqu.Ex{"mail_account_id": mailAccountId})
}

// GetSieveScriptByName returns one script of the mail account by name.
func (dbResource *DbResource) GetSieveScriptByName(mailAccountId int64, name string,
	transaction *sqlx.Tx) (map[string]interface{}, error) {
	scripts, _, err := dbResource.GetRowsByWhereClauseWithTransaction(sieveScriptTableName, nil, transaction,
		goqu.Ex{"mail_account_id": mailAccountId}, goqu.Ex{"name": name})
	if err != nil {
		return nil, err
	}
	if len(scripts) == 0 {
		return nil, errors.New("no such sieve script")
	}
	return scripts[0], nil
}

// CreateSieveScript stores a new named script for the mail account. The
// content must have been validated by the caller; duplicate names fail.
func (dbResource *DbResource) CreateSieveScript(mailAccountId int64, mailAccountRef string,
	sessionUser *auth.SessionUser, name string, content string, transaction *sqlx.Tx) error {
	if existing, lookupErr := dbResource.GetSieveScriptByName(mailAccountId, name, transaction); lookupErr == nil && existing != nil {
		return errors.New("sieve script with name already exists")
	}

	scriptURL, _ := url.Parse("/api/sieve_script")
	httpRequest := (&http.Request{
		Method: http.MethodPost,
		URL:    scriptURL,
	}).WithContext(context.WithValue(context.Background(), "user", sessionUser))

	responder, err := dbResource.Cruds[sieveScriptTableName].createAfterAuthorizationWithTransaction(
		api2go.NewApi2GoModelWithData(sieveScriptTableName, nil, 0, nil, map[string]interface{}{
			"name":            name,
			"script":          content,
			"is_active":       false,
			"mail_account_id": mailAccountRef,
		}), api2go.Request{PlainRequest: httpRequest}, transaction)
	if err != nil {
		return err
	}
	if responder == nil || responder.Result() == nil {
		return errors.New("failed to store sieve script")
	}
	return nil
}

// DeleteSieveScript removes a script of the mail account. If it was active,
// the account is left without an active script.
func (dbResource *DbResource) DeleteSieveScript(mailAccountId int64, name string,
	sessionUser *auth.SessionUser, transaction *sqlx.Tx) error {
	script, err := dbResource.GetSieveScriptByName(mailAccountId, name, transaction)
	if err != nil {
		return err
	}
	reference := daptinid.InterfaceToDIR(script["reference_id"])

	scriptURL, _ := url.Parse("/api/sieve_script")
	request := api2go.Request{PlainRequest: (&http.Request{
		Method: http.MethodDelete,
		URL:    scriptURL,
	}).WithContext(context.WithValue(context.Background(), "user", sessionUser))}

	_, err = dbResource.Cruds[sieveScriptTableName].deleteAfterAuthorizationWithTransaction(reference, request, transaction)
	return err
}

// RenameSieveScript changes the name of a script; the operation fails when the
// target name already exists.
func (dbResource *DbResource) RenameSieveScript(mailAccountId int64, oldName, newName string,
	sessionUser *auth.SessionUser, transaction *sqlx.Tx) error {
	script, err := dbResource.GetSieveScriptByName(mailAccountId, oldName, transaction)
	if err != nil {
		return err
	}
	if existing, lookupErr := dbResource.GetSieveScriptByName(mailAccountId, newName, transaction); lookupErr == nil && existing != nil {
		return errors.New("sieve script with target name already exists")
	}

	model := api2go.NewApi2GoModelWithData(sieveScriptTableName, nil, 0, nil, map[string]interface{}{
		"name": newName,
	})
	model.SetID(daptinid.InterfaceToDIR(script["reference_id"]).String())

	scriptURL, _ := url.Parse("/api/sieve_script")
	request := api2go.Request{PlainRequest: (&http.Request{
		Method: http.MethodPatch,
		URL:    scriptURL,
	}).WithContext(context.WithValue(context.Background(), "user", sessionUser))}

	_, err = dbResource.Cruds[sieveScriptTableName].updateAfterAuthorizationWithTransaction(model, request, transaction)
	return err
}

// SetActiveSieveScript atomically makes one script the single active script of
// the mail account. An empty name clears the active state. The target script
// is re-validated before activation, so an invalid script can never become
// active. The mail-account lock serializes concurrent switches.
func (dbResource *DbResource) SetActiveSieveScript(mailAccountId int64, name string,
	sessionUser *auth.SessionUser, transaction *sqlx.Tx) error {
	if err := dbResource.Cruds["mail_account"].LockMailAccountForMailboxCreation(mailAccountId, transaction); err != nil {
		return err
	}

	if name != "" {
		script, err := dbResource.GetSieveScriptByName(mailAccountId, name, transaction)
		if err != nil {
			return err
		}
		content, _ := script["script"].(string)
		if err := ValidateSieveScript(content); err != nil {
			return fmt.Errorf("cannot activate sieve script: %w", err)
		}
	}

	clearQuery, clearArgs, err := statementbuilder.Squirrel.
		Update(sieveScriptTableName).Prepared(true).
		Set(goqu.Record{"is_active": false}).
		Where(goqu.Ex{"mail_account_id": mailAccountId}).ToSQL()
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(clearQuery, clearArgs...); err != nil {
		return err
	}

	if name == "" {
		return nil
	}

	activateQuery, activateArgs, err := statementbuilder.Squirrel.
		Update(sieveScriptTableName).Prepared(true).
		Set(goqu.Record{"is_active": true}).
		Where(goqu.Ex{"mail_account_id": mailAccountId}, goqu.Ex{"name": name}).ToSQL()
	if err != nil {
		return err
	}
	result, err := transaction.Exec(activateQuery, activateArgs...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("no such sieve script")
	}
	return nil
}

// GetActiveSieveScript returns the name and content of the mail account's
// single active script. found is false when no script is active.
func (dbResource *DbResource) GetActiveSieveScript(mailAccountId int64, transaction *sqlx.Tx) (
	name string, content string, found bool, err error) {
	scripts, _, err := dbResource.GetRowsByWhereClauseWithTransaction(sieveScriptTableName, nil, transaction,
		goqu.Ex{"mail_account_id": mailAccountId}, goqu.Ex{"is_active": true})
	if err != nil {
		return "", "", false, err
	}
	if len(scripts) == 0 {
		return "", "", false, nil
	}
	name, _ = scripts[0]["name"].(string)
	content, _ = scripts[0]["script"].(string)
	return name, content, true, nil
}
