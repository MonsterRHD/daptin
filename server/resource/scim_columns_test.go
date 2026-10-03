package resource

import (
	"fmt"
	"testing"

	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func newScimSchemaTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	database, err := sqlx.Open("sqlite3", fmt.Sprintf("file:scim-cols-%s?mode=memory&cache=shared&_busy_timeout=10000", uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	return database
}

func scimTablesForTest() CmsConfig {
	statementbuilder.InitialiseStatementBuilder("sqlite3")
	config := CmsConfig{Tables: standardTablesForTest(map[string]bool{
		USER_ACCOUNT_TABLE_NAME: true,
		"usergroup":             true,
	})}
	CheckRelations(&config)
	return config
}

func TestScimColumnsFreshSchema(t *testing.T) {
	db := newScimSchemaTestDB(t)
	config := scimTablesForTest()
	CheckAllTableStatus(&config, db)

	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	CreateUniqueConstraints(&config, tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	CreateIndexes(&config, db)

	userTable := tableFromConfig(t, &config, USER_ACCOUNT_TABLE_NAME)
	activeColumn := columnFromTable(t, userTable, ACTIVE_COLUMN_NAME)
	if activeColumn.DataType != "bool" || activeColumn.IsNullable || activeColumn.DefaultValue != "true" || !activeColumn.IsIndexed {
		t.Fatalf("user_account.active column definition invalid: %#v", activeColumn)
	}
	externalColumn := columnFromTable(t, userTable, EXTERNAL_ID_COLUMN_NAME)
	if !externalColumn.IsNullable || !externalColumn.IsUnique || !externalColumn.IsIndexed {
		t.Fatalf("user_account.external_id must be nullable unique indexed: %#v", externalColumn)
	}
	groupTable := tableFromConfig(t, &config, "usergroup")
	groupExternal := columnFromTable(t, groupTable, EXTERNAL_ID_COLUMN_NAME)
	if !groupExternal.IsNullable || !groupExternal.IsUnique {
		t.Fatalf("usergroup.external_id must be nullable unique: %#v", groupExternal)
	}

	// Existing rows default active to true; two NULL external ids coexist.
	ref1 := uuid.New()
	ref2 := uuid.New()
	for index, ref := range []uuid.UUID{ref1, ref2} {
		_, err = db.Exec(`insert into user_account (name, email, password, confirmed, version, reference_id, permission, external_id)
		 values (?, ?, ?, ?, ?, ?, ?, NULL)`,
			fmt.Sprintf("User %d", index), fmt.Sprintf("u%d@example.com", index), "x", false, 1, ref[:], 0)
		if err != nil {
			t.Fatalf("insert user %d: %v", index, err)
		}
	}

	var activeValue int
	if err := db.Get(&activeValue, `select active from user_account where reference_id = ?`, ref1[:]); err != nil {
		t.Fatalf("read default active: %v", err)
	}
	if activeValue != 1 {
		t.Fatalf("new user_account rows must default active to 1, got %d", activeValue)
	}

	// Duplicate non-null external id is rejected on both tables.
	_, err = db.Exec(`update user_account set external_id = 'shared-ext' where reference_id = ?`, ref1[:])
	if err != nil {
		t.Fatalf("set first external id: %v", err)
	}
	if _, err = db.Exec(`update user_account set external_id = 'shared-ext' where reference_id = ?`, ref2[:]); err == nil {
		t.Fatal("expected unique constraint violation on duplicate user external_id")
	}

	g1, g2 := uuid.New(), uuid.New()
	for index, ref := range []uuid.UUID{g1, g2} {
		_, err = db.Exec(`insert into usergroup (name, version, reference_id, permission, external_id) values (?, ?, ?, ?, NULL)`,
			fmt.Sprintf("g%d", index), 1, ref[:], 0)
		if err != nil {
			t.Fatalf("insert group %d: %v", index, err)
		}
	}
	if _, err = db.Exec(`update usergroup set external_id = 'grp-ext' where reference_id = ?`, g1[:]); err != nil {
		t.Fatalf("set group external id: %v", err)
	}
	if _, err = db.Exec(`update usergroup set external_id = 'grp-ext' where reference_id = ?`, g2[:]); err == nil {
		t.Fatal("expected unique constraint violation on duplicate group external_id")
	}
}

func TestScimColumnsMigrateLegacySchema(t *testing.T) {
	db := newScimSchemaTestDB(t)

	// Simulate a deployment created before the SCIM columns existed.
	legacyStatements := []string{
		`create table user_account (
			id integer primary key autoincrement,
			name text,
			email text,
			password text,
			confirmed bool default false,
			auth_version integer not null default 1,
			version integer not null default 1,
			created_at timestamp,
			updated_at timestamp,
			reference_id blob not null unique,
			permission integer
		)`,
		`create table usergroup (
			id integer primary key autoincrement,
			name text,
			version integer not null default 1,
			created_at timestamp,
			updated_at timestamp,
			reference_id blob not null unique,
			permission integer
		)`,
	}
	for _, statement := range legacyStatements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("legacy table: %v", err)
		}
	}
	legacyUser := uuid.New()
	legacyGroup := uuid.New()
	if _, err := db.Exec(`insert into user_account (name, email, reference_id, version) values (?, ?, ?, 1)`,
		"Legacy", "legacy@example.com", legacyUser[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into usergroup (name, reference_id, version) values (?, ?, 1)`,
		"legacy-group", legacyGroup[:]); err != nil {
		t.Fatal(err)
	}

	config := scimTablesForTest()
	CheckAllTableStatus(&config, db)
	CreateIndexes(&config, db)

	// Backfilled column exists and existing users are active.
	if _, err := db.Exec(`select active, external_id from user_account`); err != nil {
		t.Fatalf("user_account must expose migrated columns: %v", err)
	}
	if _, err := db.Exec(`select external_id from usergroup`); err != nil {
		t.Fatalf("usergroup must expose migrated column: %v", err)
	}
	var activeValue int
	if err := db.Get(&activeValue, `select active from user_account where reference_id = ?`, legacyUser[:]); err != nil {
		t.Fatalf("read migrated active: %v", err)
	}
	if activeValue != 1 {
		t.Fatalf("legacy users must be backfilled active=1, got %d", activeValue)
	}

	// Unique index effective after migration.
	newUserRef := uuid.New()
	if _, err := db.Exec(`insert into user_account (name, email, reference_id, version, external_id)
	 values ('X', 'x@example.com', ?, 1, 'dup')`, newUserRef[:]); err != nil {
		t.Fatalf("insert with external id after migration: %v", err)
	}
	if _, err := db.Exec(`update user_account set external_id = 'dup' where reference_id = ?`, legacyUser[:]); err == nil {
		t.Fatal("migrated unique index must reject duplicate external_id")
	}
}

func TestRowValueActive(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  bool
	}{
		{"nil defaults active", nil, true},
		{"bool true", true, true},
		{"bool false", false, false},
		{"int64 1", int64(1), true},
		{"int64 0", int64(0), false},
		{"int 0", 0, false},
		{"float 1", float64(1), true},
		{"string 1", "1", true},
		{"string true", "true", true},
		{"string false", "false", false},
		{"bytes", []byte("0"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowValueActive(tc.value); got != tc.want {
				t.Fatalf("RowValueActive(%v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
