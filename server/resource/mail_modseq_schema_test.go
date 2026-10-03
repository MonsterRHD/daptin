package resource

import (
	"database/sql"
	"testing"

	"github.com/daptin/daptin/server/table_info"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

func standardTableByName(t *testing.T, name string) table_info.TableInfo {
	t.Helper()
	for _, table := range StandardTables {
		if table.TableName == name {
			return table
		}
	}
	t.Fatalf("standard table %q not found", name)
	return table_info.TableInfo{}
}

func TestSchemaAddsModSeqColumnsAndMailDeletion(t *testing.T) {
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, name := range []string{"mail_box", "mail", "mail_deletion"} {
		table := standardTableByName(t, name)
		if err := CheckTable(&table, db); err != nil {
			t.Fatalf("create table %s: %v", name, err)
		}
	}

	assertColumnPresent(t, db, "mail_box", "highestmodseq")
	assertColumnPresent(t, db, "mail_box", "deletion_history_modseq")
	assertColumnPresent(t, db, "mail", "modseq")
	assertColumnPresent(t, db, "mail_deletion", "mail_box_id")
	assertColumnPresent(t, db, "mail_deletion", "uid")
	assertColumnPresent(t, db, "mail_deletion", "modseq")
}

func assertColumnPresent(t *testing.T, db *sqlx.DB, table, column string) {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		if name == column {
			return
		}
	}
	t.Fatalf("column %q not found on table %q", column, table)
}
