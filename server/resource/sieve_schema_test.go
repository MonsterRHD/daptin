package resource

import (
	"testing"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/table_info"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

func TestSieveTablesCreateWithRelationsAndUniqueConstraint(t *testing.T) {
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tables := make([]table_info.TableInfo, 0, 2)
	for _, tableName := range []string{"sieve_script", "vacation_track"} {
		source := findStandardTable(t, tableName)
		table := table_info.TableInfo{
			TableName: source.TableName,
			Columns:   append([]api2go.ColumnInfo(nil), source.Columns...),
		}
		// Relations are expanded into foreign-key columns during schema sync.
		table.Columns = append(table.Columns, api2go.ColumnInfo{
			Name:       "mail_account_id",
			ColumnName: "mail_account_id",
			DataType:   "int(11)",
			ColumnType: "value",
		})
		if err := CreateTable(&table, db); err != nil {
			t.Fatalf("create table %s: %v", tableName, err)
		}
		tables = append(tables, table)
	}
	CreateIndexes(&CmsConfig{Tables: tables}, db)

	if _, err := db.Exec(`insert into vacation_track (track_key, sender, handle, last_sent_at, mail_account_id)
		values ('1#a@x.test#h', 'a@x.test', 'h', current_timestamp, 1)`); err != nil {
		t.Fatalf("insert vacation_track: %v", err)
	}
	if _, err := db.Exec(`insert into vacation_track (track_key, sender, handle, last_sent_at, mail_account_id)
		values ('1#a@x.test#h', 'a@x.test', 'h', current_timestamp, 1)`); err == nil {
		t.Fatal("duplicate track_key was accepted")
	}

	if _, err := db.Exec(`insert into sieve_script (name, script, is_active, mail_account_id)
		values ('main', 'keep;', false, 1)`); err != nil {
		t.Fatalf("insert sieve_script: %v", err)
	}
}

func TestSieveScriptCompositeUniqueConstraint(t *testing.T) {
	env := newSieveStoreTestEnv(t, false)

	tx := env.db.MustBegin()
	source := findStandardTable(t, "sieve_script")
	source.Columns = append(source.Columns, api2go.ColumnInfo{
		Name: "mail_account_id", ColumnName: "mail_account_id", DataType: "int(11)", ColumnType: "value",
	})
	CreateUniqueConstraints(&CmsConfig{Tables: []table_info.TableInfo{source}}, tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	insert := func() error {
		_, err := env.db.Exec(
			`insert into sieve_script (name, mail_account_id, reference_id, permission, version)
			values ('dup', 1, randomblob(16), 0, 1)`)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("duplicate (mail_account_id, name) pair accepted")
	}
}

func findStandardTable(t *testing.T, tableName string) table_info.TableInfo {
	t.Helper()
	for _, table := range StandardTables {
		if table.TableName == tableName {
			return table
		}
	}
	t.Fatalf("table %s not found in StandardTables", tableName)
	return table_info.TableInfo{}
}
