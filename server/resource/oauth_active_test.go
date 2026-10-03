package resource

import (
	"fmt"
	"testing"

	"github.com/artpar/api2go/v2"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/table_info"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

func newActiveGateTestDB(t *testing.T) (*sqlx.DB, daptinid.DaptinReferenceId) {
	t.Helper()
	db, err := sqlx.Open("sqlite3", fmt.Sprintf("file:active-gate-%s?mode=memory&cache=shared", uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	statements := []string{
		`create table user_account (
			id integer primary key autoincrement,
			name text, email text, password text,
			confirmed bool default false,
			auth_version integer not null default 1,
			active bool not null default 1,
			external_id text,
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
			created_at timestamp
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	userRef := daptinid.DaptinReferenceId(uuid.New())
	if _, err := db.Exec(`insert into user_account (id, name, email, active, version, reference_id, permission)
		values (1, 'Gate User', 'gate@example.com', 1, 1, ?, 0)`, userRef[:]); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return db, userRef
}

func activeGateTableInfo() *table_info.TableInfo {
	for index := range StandardTables {
		if StandardTables[index].TableName == USER_ACCOUNT_TABLE_NAME {
			table := StandardTables[index]
			table.Columns = append(append([]api2go.ColumnInfo(nil), table.Columns...), StandardColumns...)
			return &table
		}
	}
	panic("user_account standard table missing")
}

func activeGateCruds(db *sqlx.DB) map[string]*DbResource {
	info := activeGateTableInfo()
	cruds := make(map[string]*DbResource)
	crud := &DbResource{
		model:      api2go.NewApi2GoModel(info.TableName, info.Columns, int64(info.DefaultPermission), info.Relations),
		connection: db,
		tableInfo:  info,
		Cruds:      cruds,
	}
	cruds[USER_ACCOUNT_TABLE_NAME] = crud
	return cruds
}

// TestIsUserAccountActiveHelper covers the single authority used by the IMAP
// and SMTP login adapters after resolving the mail_account owner.
func TestIsUserAccountActiveHelper(t *testing.T) {
	db, userRef := newActiveGateTestDB(t)

	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	active, err := IsUserAccountActiveWithTransaction(userRef, tx)
	if err != nil {
		t.Fatalf("active lookup: %v", err)
	}
	if !active {
		t.Fatal("seeded user must be active")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`update user_account set active = 0 where id = 1`); err != nil {
		t.Fatal(err)
	}
	tx2, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	active, err = IsUserAccountActiveWithTransaction(userRef, tx2)
	if err != nil {
		t.Fatalf("inactive lookup: %v", err)
	}
	if active {
		t.Fatal("user must be reported inactive")
	}
	_ = tx2.Commit()

	missingRef := daptinid.DaptinReferenceId(uuid.New())
	tx3, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	active, err = IsUserAccountActiveWithTransaction(missingRef, tx3)
	_ = tx3.Commit()
	if active || err != nil {
		t.Fatalf("missing user must be inactive without error, got active=%v err=%v", active, err)
	}
}

// TestOAuthSessionUserRejectsInactiveOwner ensures every OAuth token path
// (authorization code exchange, refresh, resource validation) inherits the
// active gate because they all resolve identity through sessionUserFromRow.
func TestOAuthSessionUserRejectsInactiveOwner(t *testing.T) {
	db, userRef := newActiveGateTestDB(t)
	provider := &OAuthProvider{cruds: activeGateCruds(db)}

	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	sessionUser, err := provider.sessionUserFromRow(map[string]interface{}{
		"user_account_id": userRef,
	}, tx)
	if err != nil {
		t.Fatalf("active owner must resolve a session user: %v", err)
	}
	if !sessionUser.Active || sessionUser.UserId != 1 {
		t.Fatalf("unexpected session user: %+v", sessionUser)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`update user_account set active = 0 where id = 1`); err != nil {
		t.Fatal(err)
	}
	tx2, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.sessionUserFromRow(map[string]interface{}{
		"user_account_id": userRef,
	}, tx2); err == nil {
		t.Fatal("inactive owner must be rejected on every OAuth token path")
	}
	_ = tx2.Commit()
}
