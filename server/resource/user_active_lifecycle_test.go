package resource

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/table_info"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

func setupActiveLifecycleDB(t *testing.T) (*sqlx.DB, *DbResource) {
	t.Helper()
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	statements := []string{
		`create table user_account (
			id integer primary key autoincrement,
			name text, email text, password text,
			confirmed bool default false,
			auth_version integer not null default 1,
			active bool not null default 1,
			version integer not null default 1,
			created_at timestamp, updated_at timestamp,
			reference_id blob not null unique,
			permission integer
		)`,
		`create table usergroup (id integer primary key autoincrement, name text, reference_id blob)`,
		`create table user_account_user_account_id_has_usergroup_usergroup_id (
			id integer primary key,
			user_account_id integer,
			usergroup_id integer,
			reference_id blob,
			permission integer,
			version integer not null default 1,
			created_at timestamp
		)`,
		`create table oauth_access (
			id integer primary key autoincrement,
			token_hash text, token_type text, scope text,
			expires_at bigint, revoked_at bigint,
			oauth_app_id integer, user_account_id integer,
			reference_id blob, version integer default 1
		)`,
		`create table oauth_refresh (
			id integer primary key autoincrement,
			token_hash text, scope text,
			expires_at bigint, revoked_at bigint,
			oauth_app_id integer, user_account_id integer,
			reference_id blob, version integer default 1
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	columns := []api2go.ColumnInfo{
		{Name: "name", ColumnName: "name", DataType: "varchar(80)", ColumnType: "label"},
		{Name: "email", ColumnName: "email", DataType: "varchar(80)", ColumnType: "email"},
		{Name: "password", ColumnName: "password", DataType: "varchar(100)", ColumnType: "password", IsNullable: true},
		{Name: "confirmed", ColumnName: "confirmed", DataType: "bool", ColumnType: "truefalse", DefaultValue: "false"},
		{Name: auth.AuthVersionColumn, ColumnName: auth.AuthVersionColumn, DataType: "INTEGER", ColumnType: "measurement", DefaultValue: "1", ExcludeFromApi: true},
		{Name: ACTIVE_COLUMN_NAME, ColumnName: ACTIVE_COLUMN_NAME, DataType: "bool", ColumnType: "truefalse", DefaultValue: "true"},
	}
	columns = append(columns, StandardColumns...)
	tableInfo := &table_info.TableInfo{TableName: USER_ACCOUNT_TABLE_NAME, Columns: columns}
	model := api2go.NewApi2GoModel(USER_ACCOUNT_TABLE_NAME, columns, int64(auth.DEFAULT_PERMISSION), nil)
	cruds := map[string]*DbResource{}
	dbResource := &DbResource{model: model, db: db, connection: db, tableInfo: tableInfo, Cruds: cruds}
	cruds[USER_ACCOUNT_TABLE_NAME] = dbResource
	return db, dbResource
}

func insertLifecycleUser(t *testing.T, db *sqlx.DB, id int64, email string) uuid.UUID {
	t.Helper()
	ref := uuid.New()
	if _, err := db.Exec(`insert into user_account (id, name, email, password, confirmed, auth_version, active, version, reference_id, permission)
		values (?, ?, ?, ?, 0, 1, 1, 1, ?, 0)`, id, "User "+email, email, "$2a$10$abcdefghijklmnopqrstuv", ref[:]); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return ref
}

func insertLifecycleToken(t *testing.T, db *sqlx.DB, table string, id, userID int64, revoked interface{}) {
	t.Helper()
	ref := uuid.New()
	if _, err := db.Exec(`insert into `+table+` (id, token_hash, expires_at, revoked_at, oauth_app_id, user_account_id, reference_id)
		values (?, ?, ?, ?, 1, ?, ?)`, id, "hash-"+table+"-"+emailish(id), time.Now().Add(time.Hour).Unix(), revoked, userID, ref[:]); err != nil {
		t.Fatalf("insert token: %v", err)
	}
}

func emailish(id int64) string {
	return string(rune('a' + id%26))
}

func updateLifecycleActive(t *testing.T, dbResource *DbResource, ref uuid.UUID, currentActive, targetActive bool, version, authVersion int64) {
	t.Helper()
	model := api2go.NewApi2GoModelWithData(USER_ACCOUNT_TABLE_NAME, dbResource.model.GetColumns(), int64(auth.DEFAULT_PERMISSION), nil, map[string]interface{}{
		"id":               int64(1),
		"email":            "u1@example.com",
		"confirmed":        false,
		"auth_version":     authVersion,
		ACTIVE_COLUMN_NAME: currentActive,
		"version":          version,
		"reference_id":     ref,
		"permission":       int64(0),
	})
	model.SetAttributes(map[string]interface{}{ACTIVE_COLUMN_NAME: targetActive})
	u, _ := url.Parse("/user_account")
	req := api2go.Request{PlainRequest: (&http.Request{Method: "PATCH", URL: u}).WithContext(context.Background())}
	tx, err := dbResource.connection.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbResource.UpdateWithoutFilters(model, req, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("update active: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDeactivationRaisesVersionAndRevokesTokens(t *testing.T) {
	db, crud := setupActiveLifecycleDB(t)
	userRef := insertLifecycleUser(t, db, 1, "u1@example.com")
	insertLifecycleUser(t, db, 2, "u2@example.com")
	insertLifecycleToken(t, db, "oauth_access", 1, 1, nil)
	insertLifecycleToken(t, db, "oauth_refresh", 2, 1, nil)
	insertLifecycleToken(t, db, "oauth_access", 3, 2, nil)

	// One membership row that must survive active toggles.
	groupRef := uuid.New()
	joinRef := uuid.New()
	if _, err := db.Exec(`insert into usergroup (id, name, reference_id) values (10, 'g1', ?)`, groupRef[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into user_account_user_account_id_has_usergroup_usergroup_id (id, user_account_id, usergroup_id, reference_id, permission, created_at)
		values (1, 1, 10, ?, 0, ?)`, joinRef[:], time.Now()); err != nil {
		t.Fatal(err)
	}

	updateLifecycleActive(t, crud, userRef, true, false, 1, 1)

	var active int
	var authVersion int64
	if err := db.Get(&active, `select active from user_account where id = 1`); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("user should be inactive, got %d", active)
	}
	if err := db.Get(&authVersion, `select auth_version from user_account where id = 1`); err != nil {
		t.Fatal(err)
	}
	if authVersion != 2 {
		t.Fatalf("deactivation must raise auth_version to 2, got %d", authVersion)
	}
	for _, id := range []int64{1, 2} {
		var revoked int64
		table := "oauth_access"
		if id == 2 {
			table = "oauth_refresh"
		}
		if err := db.Get(&revoked, `select revoked_at from `+table+` where id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if revoked == 0 {
			t.Fatalf("token %d of deactivated user must be revoked", id)
		}
	}
	var otherRevoked interface{}
	if err := db.Get(&otherRevoked, `select revoked_at from oauth_access where id = 3`); err != nil {
		t.Fatal(err)
	}
	if otherRevoked != nil {
		t.Fatalf("other user's token must not be revoked, got %v", otherRevoked)
	}
	var joinCount int
	if err := db.Get(&joinCount, `select count(*) from user_account_user_account_id_has_usergroup_usergroup_id where user_account_id = 1`); err != nil {
		t.Fatal(err)
	}
	if joinCount != 1 {
		t.Fatalf("membership must survive deactivation, got %d rows", joinCount)
	}

	// Re-activation raises the version again but still does not touch membership.
	updateLifecycleActive(t, crud, userRef, false, true, 2, 2)
	if err := db.Get(&authVersion, `select auth_version from user_account where id = 1`); err != nil {
		t.Fatal(err)
	}
	if authVersion != 3 {
		t.Fatalf("reactivation must raise auth_version to 3, got %d", authVersion)
	}
	if err := db.Get(&active, `select active from user_account where id = 1`); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatal("user should be active again")
	}
	if err := db.Get(&joinCount, `select count(*) from user_account_user_account_id_has_usergroup_usergroup_id where user_account_id = 1`); err != nil {
		t.Fatal(err)
	}
	if joinCount != 1 {
		t.Fatalf("membership must not change on active toggles, got %d", joinCount)
	}
}

func TestUserDeletionRevokesTokensAndKeepsBusinessRecords(t *testing.T) {
	db, crud := setupActiveLifecycleDB(t)
	userRef := insertLifecycleUser(t, db, 1, "gone@example.com")
	insertLifecycleToken(t, db, "oauth_access", 1, 1, nil)
	insertLifecycleToken(t, db, "oauth_refresh", 2, 1, nil)

	// Business record owned by the user (simulated owner column data).
	if _, err := db.Exec(`create table widget (id integer primary key, name text, user_account_id integer)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into widget (id, name, user_account_id) values (1, 'owned', 1)`); err != nil {
		t.Fatal(err)
	}

	u, _ := url.Parse("/user_account")
	req := api2go.Request{PlainRequest: (&http.Request{Method: "DELETE", URL: u}).WithContext(context.Background())}
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if err := crud.DeleteWithoutFilters(daptinid.DaptinReferenceId(userRef), req, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("delete user: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.Get(&count, `select count(*) from user_account where id = 1`); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("user must be deleted")
	}
	for _, id := range []int64{1, 2} {
		var revoked int64
		table := "oauth_access"
		if id == 2 {
			table = "oauth_refresh"
		}
		if err := db.Get(&revoked, `select revoked_at from `+table+` where id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if revoked == 0 {
			t.Fatalf("token %d of deleted user must be revoked", id)
		}
	}
	var owner interface{}
	if err := db.Get(&owner, `select user_account_id from widget where id = 1`); err != nil {
		t.Fatal(err)
	}
	if owner.(int64) != 1 {
		t.Fatalf("owned business record must be preserved, got owner=%v", owner)
	}
}
