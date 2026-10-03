package resource

import (
	"path/filepath"
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

type sieveStoreTestEnv struct {
	db            *sqlx.DB
	dbPath        string
	root          *DbResource
	sessionUser   *auth.SessionUser
	mailAccountId int64
	accountRef    string
}

func TestSieveScriptCreateListDuplicateGet(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	tx := env.db.MustBegin()

	if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef, env.sessionUser, "a", `keep;`, tx); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef, env.sessionUser, "a", `keep;`, tx); err == nil {
		t.Fatal("duplicate create accepted")
	}
	script, err := env.root.GetSieveScriptByName(env.mailAccountId, "a", tx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if script["script"] != `keep;` {
		t.Fatalf("content = %v", script["script"])
	}
	scripts, err := env.root.ListSieveScripts(env.mailAccountId, tx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(scripts) != 1 {
		t.Fatalf("list count = %d", len(scripts))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSetActiveSieveScriptAtomicSwitchAndClear(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	tx := env.db.MustBegin()

	for name, content := range map[string]string{
		"a": `keep;`,
		"b": `discard;`,
	} {
		if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef, env.sessionUser, name, content, tx); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	if err := env.root.SetActiveSieveScript(env.mailAccountId, "a", env.sessionUser, tx); err != nil {
		t.Fatalf("activate a: %v", err)
	}
	if countActive(t, tx, env.mailAccountId) != 1 {
		t.Fatal("active count != 1")
	}
	if name, _, found, _ := env.root.GetActiveSieveScript(env.mailAccountId, tx); !found || name != "a" {
		t.Fatalf("active = %q found=%v", name, found)
	}

	if err := env.root.SetActiveSieveScript(env.mailAccountId, "b", env.sessionUser, tx); err != nil {
		t.Fatalf("activate b: %v", err)
	}
	if countActive(t, tx, env.mailAccountId) != 1 {
		t.Fatal("active count != 1 after switch")
	}
	if name, _, _, _ := env.root.GetActiveSieveScript(env.mailAccountId, tx); name != "b" {
		t.Fatalf("active = %q, want b", name)
	}

	if err := env.root.SetActiveSieveScript(env.mailAccountId, "", env.sessionUser, tx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if countActive(t, tx, env.mailAccountId) != 0 {
		t.Fatal("active count != 0 after clear")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSetActiveRejectsInvalidScriptAndKeepsState(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	tx := env.db.MustBegin()

	if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef, env.sessionUser, "good", `keep;`, tx); err != nil {
		t.Fatal(err)
	}
	// Invalid script stored directly via resource create (caller-validation is
	// bypassed in tests to simulate malformed content).
	goodRef := daptinid.InterfaceToDIR(getScriptRow(t, env, tx, "good")["reference_id"])
	if _, err := tx.Exec(`insert into sieve_script (name, script, is_active, mail_account_id, reference_id, permission, version, created_at, updated_at)
		values ('bad', 'this is not sieve', false, ?, ?, 0, 1, current_timestamp, current_timestamp)`,
		env.mailAccountId, goodRef[:]); err != nil {
		t.Fatal(err)
	}

	if err := env.root.SetActiveSieveScript(env.mailAccountId, "good", env.sessionUser, tx); err != nil {
		t.Fatal(err)
	}
	if err := env.root.SetActiveSieveScript(env.mailAccountId, "bad", env.sessionUser, tx); err == nil {
		t.Fatal("invalid script activated")
	}
	if name, _, found, _ := env.root.GetActiveSieveScript(env.mailAccountId, tx); !found || name != "good" {
		t.Fatalf("active state changed after rejected activation: %q found=%v", name, found)
	}
	if countActive(t, tx, env.mailAccountId) != 1 {
		t.Fatal("active count != 1")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestSieveScriptDeleteAndRename(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)
	tx := env.db.MustBegin()

	if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef, env.sessionUser, "old", `keep;`, tx); err != nil {
		t.Fatal(err)
	}
	if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef, env.sessionUser, "taken", `keep;`, tx); err != nil {
		t.Fatal(err)
	}
	if err := env.root.RenameSieveScript(env.mailAccountId, "old", "new", env.sessionUser, tx); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := env.root.RenameSieveScript(env.mailAccountId, "new", "taken", env.sessionUser, tx); err == nil {
		t.Fatal("rename to existing accepted")
	}
	if _, err := env.root.GetSieveScriptByName(env.mailAccountId, "new", tx); err != nil {
		t.Fatalf("get renamed: %v", err)
	}
	if err := env.root.DeleteSieveScript(env.mailAccountId, "new", env.sessionUser, tx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := env.root.GetSieveScriptByName(env.mailAccountId, "new", tx); err == nil {
		t.Fatal("deleted script found")
	}
	if err := env.root.DeleteSieveScript(env.mailAccountId, "missing", env.sessionUser, tx); err == nil {
		t.Fatal("delete missing accepted")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSieveScriptsSurviveRestart(t *testing.T) {
	env := newSieveStoreTestEnv(t, true)
	tx := env.db.MustBegin()
	if err := env.root.CreateSieveScript(env.mailAccountId, env.accountRef, env.sessionUser, "main", `keep;`, tx); err != nil {
		t.Fatal(err)
	}
	if err := env.root.SetActiveSieveScript(env.mailAccountId, "main", env.sessionUser, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same database file: simulates a service restart.
	path := env.dbPath
	_ = env.db.Close()
	reopened, err := sqlx.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.SetMaxOpenConns(1)

	root := sieveCrudGraphFromDB(t, reopened)
	newTx := reopened.MustBegin()
	scripts, err := root.ListSieveScripts(env.mailAccountId, newTx)
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 1 || scripts[0]["name"] != "main" || scripts[0]["script"] != `keep;` {
		t.Fatalf("scripts after restart: %#v", scripts)
	}
	name, content, found, err := root.GetActiveSieveScript(env.mailAccountId, newTx)
	if err != nil || !found || name != "main" || content != `keep;` {
		t.Fatalf("active after restart: name=%q found=%v err=%v", name, found, err)
	}
	if err := newTx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func countActive(t *testing.T, tx *sqlx.Tx, mailAccountId int64) int {
	t.Helper()
	var count int
	if err := tx.QueryRowx(`select count(*) from sieve_script where is_active = ? and mail_account_id = ?`,
		true, mailAccountId).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func getScriptRow(t *testing.T, env *sieveStoreTestEnv, tx *sqlx.Tx, name string) map[string]interface{} {
	t.Helper()
	row, err := env.root.GetSieveScriptByName(env.mailAccountId, name, tx)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// ---------------------------------------------------------------------------
// Test environment
// ---------------------------------------------------------------------------

func newSieveStoreTestEnv(t *testing.T, fileBacked bool) *sieveStoreTestEnv {
	t.Helper()

	var db *sqlx.DB
	var err error
	dbPath := ""
	if fileBacked {
		dbPath = filepath.Join(t.TempDir(), "sieve-test.db")
		db, err = sqlx.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	} else {
		db, err = sqlx.Open("sqlite3", ":memory:")
	}
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	for _, statement := range sieveStoreSetupStatements() {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("setup: %v\nstatement: %s", err, statement)
		}
	}

	adminGroupRef := daptinid.DaptinReferenceId(uuid.New())
	userRef := daptinid.DaptinReferenceId(uuid.New())
	accountRef := daptinid.DaptinReferenceId(uuid.New())
	now := time.Now()
	if _, err := db.Exec(`insert into usergroup (id, name, reference_id, permission) values (?, ?, ?, ?)`,
		2, "administrators", adminGroupRef[:], int64(auth.DEFAULT_PERMISSION)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into user_account (id, reference_id, permission, created_at, updated_at) values (?, ?, ?, ?, ?)`,
		1, userRef[:], int64(auth.DEFAULT_PERMISSION), now, now); err != nil {
		t.Fatal(err)
	}
	mailServerRef := daptinid.DaptinReferenceId(uuid.New())
	if _, err := db.Exec(`insert into mail_server (id, hostname, is_enabled, listen_interface, max_size, max_clients,
		timeout, tls_on, xclient_on, always_on_tls, authentication_required, reference_id, permission, version, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		1, "example.test", true, "test-listener", 10000, 20, 30, false, false, true, true,
		mailServerRef[:], 0, 1, now, now); err != nil {
		t.Fatal(err)
	}
	passwordHash, err := BcryptHashString("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into mail_account (id, username, password, user_account_id, reference_id, permission, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?)`, 1, "rcpt@example.test", passwordHash, userRef.String(), accountRef[:], int64(16256), now, now); err != nil {
		t.Fatal(err)
	}

	root := sieveCrudGraphFromDB(t, db)
	return &sieveStoreTestEnv{
		db:            db,
		dbPath:        dbPath,
		root:          root,
		mailAccountId: 1,
		accountRef:    accountRef.String(),
		sessionUser: &auth.SessionUser{
			UserId:          1,
			UserReferenceId: userRef,
		},
	}
}

func sieveCrudGraphFromDB(t *testing.T, db *sqlx.DB) *DbResource {
	t.Helper()

	cruds := map[string]*DbResource{
		USER_ACCOUNT_TABLE_NAME: sentMailTestCrud(db, USER_ACCOUNT_TABLE_NAME, sentMailUserAccountColumns(), int64(auth.DEFAULT_PERMISSION)),
		"mail_account":          sentMailTestCrud(db, "mail_account", sentMailAccountColumns(), int64(auth.DEFAULT_PERMISSION)),
		"mail_box":              sentMailTestCrud(db, "mail_box", sentMailBoxColumns(), 16256),
		"mail":                  sentMailTestCrud(db, "mail", sieveStoreMailColumns(), 768),
		"sieve_script":          sentMailTestCrud(db, "sieve_script", sieveScriptColumns(), 0),
		"vacation_track":        sentMailTestCrud(db, "vacation_track", vacationTrackColumns(), 0),
		"mail_server":           sentMailTestCrud(db, "mail_server", sieveTestMailServerColumns(), 0),
		"outbox":                sentMailTestCrud(db, "outbox", sieveTestOutboxColumns(), 0),
	}
	adminGroupRef := daptinid.DaptinReferenceId(uuid.New())
	for _, crud := range cruds {
		crud.Cruds = cruds
		crud.AdministratorGroupId = adminGroupRef
	}

	CreateIndexes(&CmsConfig{Tables: allTableInfos(cruds)}, db)

	oldUserAccountCrud := CRUD_MAP[USER_ACCOUNT_TABLE_NAME]
	CRUD_MAP[USER_ACCOUNT_TABLE_NAME] = cruds[USER_ACCOUNT_TABLE_NAME]
	t.Cleanup(func() {
		if oldUserAccountCrud == nil {
			delete(CRUD_MAP, USER_ACCOUNT_TABLE_NAME)
			return
		}
		CRUD_MAP[USER_ACCOUNT_TABLE_NAME] = oldUserAccountCrud
	})
	return cruds["mail"]
}

func allTableInfos(cruds map[string]*DbResource) []table_info.TableInfo {
	tables := make([]table_info.TableInfo, 0, len(cruds))
	for _, crud := range cruds {
		tables = append(tables, table_info.TableInfo{
			TableName: crud.tableInfo.TableName,
			Columns:   crud.tableInfo.Columns,
		})
	}
	return tables
}

func sieveStoreSetupStatements() []string {
	return append([]string{
		`create table usergroup (id integer primary key, name text, reference_id blob, permission integer)`,
		`create table user_account (
			id integer primary key, reference_id blob, user_account_id integer,
			permission integer, created_at timestamp, updated_at timestamp)`,
		`create table user_account_user_account_id_has_usergroup_usergroup_id (
			id integer primary key, user_account_id integer, usergroup_id integer,
			reference_id blob, permission integer, created_at timestamp)`,
		`create table mail_account (
			id integer primary key, username text, password text, password_md5 text,
			user_account_id integer, reference_id blob, permission integer,
			created_at timestamp, updated_at timestamp)`,
		`create table mail_account_mail_account_id_has_usergroup_usergroup_id (
			id integer primary key, mail_account_id integer, usergroup_id integer,
			reference_id blob, permission integer, created_at timestamp)`,
		`create table mail_box (
			id integer primary key, name text, mail_account_id integer, uidvalidity integer,
			nextuid integer, subscribed bool, attributes text, flags text, permanent_flags text,
			user_account_id integer, reference_id blob, permission integer,
			created_at timestamp, updated_at timestamp)`,
		`create table mail_box_mail_box_id_has_usergroup_usergroup_id (
			id integer primary key, mail_box_id integer, usergroup_id integer,
			reference_id blob, permission integer, created_at timestamp)`,
		`create table mail_server (
			id integer primary key, hostname text, is_enabled bool, listen_interface text,
			max_size integer, max_clients integer, timeout integer, tls_on bool,
			xclient_on bool, always_on_tls bool, authentication_required bool,
			reference_id blob, permission integer, version integer,
			created_at timestamp, updated_at timestamp)`,
	}, sieveStoreMailAndTrackDDL()...)
}

func sieveStoreMailAndTrackDDL() []string {
	return []string{
		`create table mail (
			id integer primary key, message_id text, mail_id text, from_address text,
			to_address text, cc_address text, bcc_address text, sender_address text,
			subject text, body text, mail text, spam_score float, spam bool, hash text,
			internal_date timestamp, content_type text, reply_to_address text, recipient text,
			has_attachment bool, ip_addr text, return_path text, is_tls bool,
			mail_box_id integer, user_account_id integer, uid integer, seen bool, recent bool,
			deleted bool, size integer, flags text, reference_id blob, permission integer,
			created_at timestamp, updated_at timestamp)`,
		`create table sieve_script (
			id integer primary key, name text, script text, is_active bool,
			mail_account_id integer, user_account_id integer, reference_id blob,
			permission integer, version integer, created_at timestamp, updated_at timestamp)`,
		`create table vacation_track (
			id integer primary key, track_key text, sender text, handle text,
			last_sent_at timestamp, mail_account_id integer, user_account_id integer,
			reference_id blob, permission integer, version integer,
			created_at timestamp, updated_at timestamp)`,
		`create table outbox (
			id integer primary key, from_address text, to_address text, to_host text,
			mail text, sent bool, retry_count integer, last_error text, next_retry_at timestamp,
			mail_server_id integer, reference_id blob, permission integer, version integer,
			created_at timestamp, updated_at timestamp)`,
	}
}

func sieveStoreMailColumns() []api2go.ColumnInfo {
	return append(sentMailColumns(),
		api2go.ColumnInfo{Name: "cc_address", ColumnName: "cc_address", DataType: "text", ColumnType: "label", IsNullable: true},
		api2go.ColumnInfo{Name: "bcc_address", ColumnName: "bcc_address", DataType: "text", ColumnType: "label", IsNullable: true},
	)
}

func sieveScriptColumns() []api2go.ColumnInfo {
	return []api2go.ColumnInfo{
		{Name: "id", ColumnName: "id", DataType: "INTEGER", ColumnType: "id", IsAutoIncrement: true},
		{Name: "name", ColumnName: "name", DataType: "varchar(255)", ColumnType: "label"},
		{Name: "script", ColumnName: "script", DataType: "text", ColumnType: "content"},
		{Name: "is_active", ColumnName: "is_active", DataType: "bool", ColumnType: "truefalse", DefaultValue: "false"},
		{
			Name:         "mail_account",
			ColumnName:   "mail_account_id",
			DataType:     "int(11)",
			ColumnType:   "alias",
			IsForeignKey: true,
			ForeignKeyData: api2go.ForeignKeyData{
				Namespace:  "mail_account",
				KeyName:    "id",
				DataSource: "self",
			},
		},
		{Name: "user_account_id", ColumnName: "user_account_id", DataType: "int(11)", ColumnType: "value", IsNullable: true},
		{Name: "reference_id", ColumnName: "reference_id", DataType: "blob", ColumnType: "alias"},
		{Name: "permission", ColumnName: "permission", DataType: "integer"},
		{Name: "version", ColumnName: "version", DataType: "integer", ColumnType: "measurement", DefaultValue: "1"},
		{Name: "created_at", ColumnName: "created_at", DataType: "timestamp", ColumnType: "datetime"},
		{Name: "updated_at", ColumnName: "updated_at", DataType: "timestamp", ColumnType: "datetime", IsNullable: true},
	}
}

func vacationTrackColumns() []api2go.ColumnInfo {
	return []api2go.ColumnInfo{
		{Name: "id", ColumnName: "id", DataType: "INTEGER", ColumnType: "id", IsAutoIncrement: true},
		{Name: "track_key", ColumnName: "track_key", DataType: "varchar(160)", ColumnType: "label", IsUnique: true},
		{Name: "sender", ColumnName: "sender", DataType: "varchar(255)", ColumnType: "label"},
		{Name: "handle", ColumnName: "handle", DataType: "varchar(255)", ColumnType: "label"},
		{Name: "last_sent_at", ColumnName: "last_sent_at", DataType: "timestamp", ColumnType: "datetime"},
		{
			Name:         "mail_account",
			ColumnName:   "mail_account_id",
			DataType:     "int(11)",
			ColumnType:   "alias",
			IsForeignKey: true,
			ForeignKeyData: api2go.ForeignKeyData{
				Namespace:  "mail_account",
				KeyName:    "id",
				DataSource: "self",
			},
		},
		{Name: "user_account_id", ColumnName: "user_account_id", DataType: "int(11)", ColumnType: "value", IsNullable: true},
		{Name: "reference_id", ColumnName: "reference_id", DataType: "blob", ColumnType: "alias"},
		{Name: "permission", ColumnName: "permission", DataType: "integer"},
		{Name: "version", ColumnName: "version", DataType: "integer", ColumnType: "measurement", DefaultValue: "1"},
		{Name: "created_at", ColumnName: "created_at", DataType: "timestamp", ColumnType: "datetime"},
		{Name: "updated_at", ColumnName: "updated_at", DataType: "timestamp", ColumnType: "datetime", IsNullable: true},
	}
}

func sieveTestMailServerColumns() []api2go.ColumnInfo {
	return []api2go.ColumnInfo{
		{Name: "id", ColumnName: "id", DataType: "INTEGER", ColumnType: "id", IsAutoIncrement: true},
		{Name: "hostname", ColumnName: "hostname", DataType: "varchar(100)", ColumnType: "label"},
		{Name: "is_enabled", ColumnName: "is_enabled", DataType: "bool", ColumnType: "truefalse"},
		{Name: "listen_interface", ColumnName: "listen_interface", DataType: "varchar(100)", ColumnType: "label"},
		{Name: "max_size", ColumnName: "max_size", DataType: "int(11)", ColumnType: "measurement"},
		{Name: "max_clients", ColumnName: "max_clients", DataType: "int(11)", ColumnType: "measurement"},
		{Name: "reference_id", ColumnName: "reference_id", DataType: "blob", ColumnType: "alias"},
		{Name: "permission", ColumnName: "permission", DataType: "integer"},
		{Name: "version", ColumnName: "version", DataType: "integer", ColumnType: "measurement"},
		{Name: "created_at", ColumnName: "created_at", DataType: "timestamp", ColumnType: "datetime"},
		{Name: "updated_at", ColumnName: "updated_at", DataType: "timestamp", ColumnType: "datetime", IsNullable: true},
	}
}

func sieveTestOutboxColumns() []api2go.ColumnInfo {
	return []api2go.ColumnInfo{
		{Name: "id", ColumnName: "id", DataType: "INTEGER", ColumnType: "id", IsAutoIncrement: true},
		{Name: "from_address", ColumnName: "from_address", DataType: "varchar(200)", ColumnType: "label"},
		{Name: "to_address", ColumnName: "to_address", DataType: "varchar(200)", ColumnType: "label"},
		{Name: "to_host", ColumnName: "to_host", DataType: "varchar(200)", ColumnType: "label"},
		{Name: "mail", ColumnName: "mail", DataType: "blob", ColumnType: "gzip"},
		{Name: "sent", ColumnName: "sent", DataType: "bool", ColumnType: "truefalse"},
		{Name: "retry_count", ColumnName: "retry_count", DataType: "int(11)", ColumnType: "value"},
		{Name: "last_error", ColumnName: "last_error", DataType: "text", ColumnType: "label", IsNullable: true},
		{Name: "next_retry_at", ColumnName: "next_retry_at", DataType: "timestamp", ColumnType: "datetime", IsNullable: true},
		{
			Name:         "mail_server",
			ColumnName:   "mail_server_id",
			DataType:     "int(11)",
			ColumnType:   "alias",
			IsForeignKey: true,
			ForeignKeyData: api2go.ForeignKeyData{
				Namespace:  "mail_server",
				KeyName:    "id",
				DataSource: "self",
			},
		},
		{Name: "reference_id", ColumnName: "reference_id", DataType: "blob", ColumnType: "alias"},
		{Name: "permission", ColumnName: "permission", DataType: "integer"},
		{Name: "version", ColumnName: "version", DataType: "integer", ColumnType: "measurement"},
		{Name: "created_at", ColumnName: "created_at", DataType: "timestamp", ColumnType: "datetime"},
		{Name: "updated_at", ColumnName: "updated_at", DataType: "timestamp", ColumnType: "datetime", IsNullable: true},
	}
}
