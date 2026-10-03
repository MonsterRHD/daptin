package resource

import (
	"database/sql"

	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

// mailChangeBaseline is the pre-update state required to decide whether flags
// actually changed and to attribute a mailbox move to its source mailbox.
type mailChangeBaseline struct {
	id          int64
	uid         int64
	flags       string
	mailBoxId   int64
}

func loadMailChangeBaseline(referenceID interface{}, transaction *sqlx.Tx) (mailChangeBaseline, error) {
	query, args, err := statementbuilder.Squirrel.
		Select("id", "uid", "flags", "mail_box_id").
		From("mail").Prepared(true).
		Where(goqu.Ex{"reference_id": referenceID}).
		ToSQL()
	if err != nil {
		return mailChangeBaseline{}, err
	}

	var baseline mailChangeBaseline
	var uid sql.NullInt64
	var flags sql.NullString
	var boxId sql.NullInt64
	row := transaction.QueryRowx(query, args...)
	if err := row.Scan(&baseline.id, &uid, &flags, &boxId); err != nil {
		return mailChangeBaseline{}, err
	}
	if uid.Valid {
		baseline.uid = uid.Int64
	}
	if flags.Valid {
		baseline.flags = flags.String
	}
	if boxId.Valid {
		baseline.mailBoxId = boxId.Int64
	}
	return baseline, nil
}

// setUpdateColumn replaces the value of an UPDATE column by name or appends it.
func setUpdateColumn(cols *[]string, vals *[]interface{}, name string, value interface{}) {
	for i, col := range *cols {
		if col == name {
			(*vals)[i] = value
			return
		}
	}
	*cols = append(*cols, name)
	*vals = append(*vals, value)
}
