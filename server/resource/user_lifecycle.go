package resource

import (
	"context"
	"time"

	"github.com/buraksezer/olric"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	jwtmiddleware "github.com/daptin/daptin/server/jwt"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"
)

// UserSessionRevokeTopic carries the reference id of a user_account whose
// sessions must be torn down immediately (deactivation or deletion). Every
// cluster node subscribes through its websocket server.
const UserSessionRevokeTopic = "daptin:user-session-revoke"

// bumpUserAccountAuthVersionWithTransaction raises the authentication
// lifecycle version inside the caller's transaction so JWTs carrying an older
// auth_version are rejected by the global auth middleware after commit.
func bumpUserAccountAuthVersionWithTransaction(userID int64, transaction *sqlx.Tx) error {
	query, args, err := statementbuilder.Squirrel.Update(USER_ACCOUNT_TABLE_NAME).Prepared(true).
		Set(goqu.Record{auth.AuthVersionColumn: goqu.L("COALESCE(auth_version, 1) + 1")}).
		Where(goqu.Ex{"id": userID}).ToSQL()
	if err != nil {
		return err
	}
	_, err = transaction.Exec(query, args...)
	return err
}

// revokeUserAccountOAuthTokensWithTransaction marks every unrevoked OAuth
// access/refresh token of the user revoked in the same transaction that
// deactivates or deletes the account.
func revokeUserAccountOAuthTokensWithTransaction(userID int64, transaction *sqlx.Tx) error {
	for _, tableName := range []string{"oauth_access", "oauth_refresh"} {
		query, args, err := statementbuilder.Squirrel.Update(tableName).Prepared(true).
			Set(goqu.Record{"revoked_at": time.Now().Unix()}).
			Where(goqu.Ex{"user_account_id": userID}).
			Where(goqu.Or(goqu.Ex{"revoked_at": nil}, goqu.Ex{"revoked_at": 0})).ToSQL()
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}

// invalidateUserAccountSessionCaches evicts the cached identity, administrator
// and parsed-token state for a user so the next request re-evaluates the
// account from the database.
func invalidateUserAccountSessionCaches(email string, userReference daptinid.DaptinReferenceId) {
	if email != "" {
		auth.InvalidateAuthCacheForEmail(email)
	}
	InvalidateAdminCacheForUser(userReference)
	jwtmiddleware.InvalidateTokenCache()
}

// publishUserSessionRevoke asks every cluster node to close the user's live
// websocket sessions. Publishing is best-effort and never fails the lifecycle
// operation: JWT auth_version validation independently rejects new requests.
func publishUserSessionRevoke(pubsub *olric.PubSub, userReference daptinid.DaptinReferenceId) {
	if pubsub == nil || userReference == daptinid.NullReferenceId {
		return
	}
	if _, err := pubsub.Publish(context.Background(), UserSessionRevokeTopic, userReference.String()); err != nil {
		log.Warnf("failed to publish session revocation for %s: %v", userReference.String(), err)
	}
}

// revokeDeletedUserAccountSessions runs the same session teardown as a
// deactivation before the user_account row is deleted: the auth_version is
// raised, outstanding OAuth tokens are revoked in the caller's transaction,
// identity caches are evicted and websocket sessions are closed. Business
// records owned by the user are untouched.
func (dbResource *DbResource) revokeDeletedUserAccountSessionsWithTransaction(data map[string]interface{}, transaction *sqlx.Tx) error {
	userReference := daptinid.InterfaceToDIR(data["reference_id"])
	if userID, ok := data["id"].(int64); ok && userID > 0 {
		if err := bumpUserAccountAuthVersionWithTransaction(userID, transaction); err != nil {
			return err
		}
		if err := revokeUserAccountOAuthTokensWithTransaction(userID, transaction); err != nil {
			return err
		}
	}
	email, _ := data["email"].(string)
	invalidateUserAccountSessionCaches(email, userReference)
	publishUserSessionRevoke(dbResource.PubSub, userReference)
	return nil
}
