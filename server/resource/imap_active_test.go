package resource

import (
	"testing"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/auth"
	"github.com/daptin/daptin/server/table_info"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// TestIMAPLoginRejectsInactiveOwner exercises the IMAP adapter gate using the
// same helper the SMTP adapter uses, on both sides of the active toggle.
func TestIMAPLoginRejectsInactiveOwner(t *testing.T) {
	db, _ := newActiveGateTestDB(t)
	if _, err := db.Exec(`create table mail_account (
		id integer primary key autoincrement,
		username text unique,
		password text,
		password_md5 text,
		user_account_id integer,
		version integer default 1,
		created_at timestamp, updated_at timestamp,
		reference_id blob not null unique,
		permission integer
	)`); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("mailpass"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	mailRef := uuid.New()
	if _, err := db.Exec(`insert into mail_account (id, username, password, user_account_id, reference_id)
		values (1, 'gate@example.com', ?, 1, ?)`, string(hash), mailRef[:]); err != nil {
		t.Fatal(err)
	}

	cruds := activeGateCruds(db)
	for index := range StandardTables {
		if StandardTables[index].TableName != "mail_account" {
			continue
		}
		columns := append([]api2go.ColumnInfo(nil), StandardTables[index].Columns...)
		columns = append(columns, api2go.ColumnInfo{
			Name:         "user_account_id",
			ColumnName:   "user_account_id",
			DataType:     "int(11)",
			ColumnType:   "alias",
			IsForeignKey: true,
			ForeignKeyData: api2go.ForeignKeyData{
				DataSource: "self",
				Namespace:  USER_ACCOUNT_TABLE_NAME,
				KeyName:    "id",
			},
		})
		columns = append(columns, StandardColumns...)
		info := &table_info.TableInfo{TableName: "mail_account", Columns: columns}
		cruds["mail_account"] = &DbResource{
			model:      api2go.NewApi2GoModel("mail_account", columns, int64(auth.DEFAULT_PERMISSION), nil),
			connection: db,
			tableInfo:  info,
			Cruds:      cruds,
		}
	}
	backend := &DaptinImapBackend{cruds: cruds}

	imapUser, err := backend.Login(nil, "gate@example.com", "mailpass")
	if err != nil {
		t.Fatalf("active owner IMAP login should succeed: %v", err)
	}
	daptinUser, ok := imapUser.(*DaptinImapUser)
	if !ok || daptinUser.sessionUser.UserId != 1 {
		t.Fatalf("unexpected imap user: %+v", imapUser)
	}
	if _, err := backend.Login(nil, "gate@example.com", "wrong-password"); err == nil {
		t.Fatal("wrong password should fail")
	}

	if _, err := db.Exec(`update user_account set active = 0 where id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Login(nil, "gate@example.com", "mailpass"); err == nil {
		t.Fatal("inactive owner IMAP login must be rejected")
	}

	if _, err := db.Exec(`update user_account set active = 1 where id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Login(nil, "gate@example.com", "mailpass"); err != nil {
		t.Fatalf("reactivated owner IMAP login should succeed again: %v", err)
	}

	// Deleted owner must also be rejected.
	if _, err := db.Exec(`delete from user_account where id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Login(nil, "gate@example.com", "mailpass"); err == nil {
		t.Fatal("deleted owner IMAP login must be rejected")
	}
}
