package resource

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

func internalIdFromValue(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	case nil:
		return 0, false
	default:
		return 0, false
	}
}

// NormalizeFlags returns a sorted, de-duplicated, lower-cased copy of flags.
func NormalizeFlags(values []string) []string {
	set := map[string]bool{}
	for _, f := range values {
		f = strings.TrimSpace(f)
		if f != "" {
			set[strings.ToLower(f)] = true
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// FlagsEqual reports whether two flag lists represent the same set of flags.
func FlagsEqual(a, b []string) bool {
	x := NormalizeFlags(a)
	y := NormalizeFlags(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// FlagsFromStoredString splits a comma-separated stored flags value.
func FlagsFromStoredString(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

// setInsertColumn replaces the value of an existing INSERT column by name or
// appends it when it is absent.
func setInsertColumn(cols *[]interface{}, vals *[]interface{}, name string, value interface{}) {
	for i, col := range *cols {
		if col == name {
			(*vals)[i] = value
			return
		}
	}
	*cols = append(*cols, name)
	*vals = append(*vals, value)
}

// resolveRelatedInternalId accepts either an internal numeric id or a public
// reference (DaptinReferenceId / []byte / UUID string) for a related resource
// and returns the internal id.
func resolveRelatedInternalId(value interface{}, namespace string, transaction *sqlx.Tx) (int64, error) {
	if id, ok := internalIdFromValue(value); ok {
		return id, nil
	}

	var reference daptinid.DaptinReferenceId
	ok := false
	switch v := value.(type) {
	case daptinid.DaptinReferenceId:
		if v == daptinid.NullReferenceId {
			return 0, nil
		}
		reference, ok = v, true
	case []byte:
		if len(v) == 16 {
			copy(reference[:], v)
			ok = true
		}
	case string:
		dir := daptinid.InterfaceToDIR(v)
		if dir != daptinid.NullReferenceId {
			reference, ok = dir, true
		}
	}
	if !ok {
		return 0, nil
	}
	return GetReferenceIdToIdWithTransaction(namespace, reference, transaction)
}

// AllocateMailBoxModSeq atomically advances the mailbox's persistent
// highestmodseq and returns the new value. The returned value is strictly
// greater than any value previously committed for the mailbox. The mailbox row
// lock is the same one used for UID allocation, so modseq and UID allocation
// serialize against each other within the database authority.
func (dbResource *DbResource) AllocateMailBoxModSeq(mailBoxId int64, transaction *sqlx.Tx) (int64, error) {
	if transaction == nil {
		return 0, errors.New("mailbox modseq allocation requires a transaction")
	}
	if err := dbResource.lockMailboxForUidAllocation(mailBoxId, transaction); err != nil {
		return 0, err
	}

	current, err := dbResource.readHighestModSeq(mailBoxId, transaction)
	if err != nil {
		return 0, err
	}

	next := current + 1
	query, args, err := statementbuilder.Squirrel.
		Update("mail_box").Prepared(true).
		Set(goqu.Record{"highestmodseq": next}).
		Where(goqu.Ex{"id": mailBoxId}).
		ToSQL()
	if err != nil {
		return 0, err
	}
	if _, err := transaction.Exec(query, args...); err != nil {
		return 0, err
	}
	return next, nil
}

// GetMailBoxHighestModSeq reads the current highestmodseq without allocating.
// The read can use a transaction (then it participates in the tx snapshot) or
// run directly against the database when transaction is nil.
func (dbResource *DbResource) GetMailBoxHighestModSeq(mailBoxId int64, transaction *sqlx.Tx) (int64, error) {
	return dbResource.readHighestModSeq(mailBoxId, transaction)
}

func (dbResource *DbResource) readHighestModSeq(mailBoxId int64, transaction *sqlx.Tx) (int64, error) {
	query, args, err := statementbuilder.Squirrel.
		Select("highestmodseq").
		From("mail_box").Prepared(true).
		Where(goqu.Ex{"id": mailBoxId}).
		ToSQL()
	if err != nil {
		return 0, err
	}

	var highest sql.NullInt64
	if transaction != nil {
		err = transaction.QueryRowx(query, args...).Scan(&highest)
	} else {
		err = dbResource.db.QueryRowx(query, args...).Scan(&highest)
	}
	if err != nil {
		return 0, err
	}
	if !highest.Valid || highest.Int64 < 0 {
		return 0, nil
	}
	return highest.Int64, nil
}

// GetDeletionHistoryWatermark returns the deletion history watermark:
//
//	The watermark value w means deletion records with modseq > w are
//	guaranteed complete. Deletions with modseq <= w may have been missed
//	(e.g. before the QRESYNC instrumentation was installed).
//
// The boolean is false while the mailbox watermark was never initialized.
func (dbResource *DbResource) GetDeletionHistoryWatermark(mailBoxId int64, transaction *sqlx.Tx) (int64, bool, error) {
	query, args, err := statementbuilder.Squirrel.
		Select("deletion_history_modseq").
		From("mail_box").Prepared(true).
		Where(goqu.Ex{"id": mailBoxId}).
		ToSQL()
	if err != nil {
		return 0, false, err
	}

	var watermark sql.NullInt64
	if transaction != nil {
		err = transaction.QueryRowx(query, args...).Scan(&watermark)
	} else {
		err = dbResource.db.QueryRowx(query, args...).Scan(&watermark)
	}
	if err != nil {
		return 0, false, err
	}
	if !watermark.Valid {
		return 0, false, nil
	}
	return watermark.Int64, true, nil
}

// InitDeletionHistoryWatermark ensures the watermark is initialized for a
// mailbox. It is idempotent: an already initialized watermark is returned
// unchanged. For mailboxes that predate QRESYNC instrumentation, the first
// initialization records the current highestmodseq as the boundary, i.e. only
// strictly later deletions are guaranteed to have been recorded.
func (dbResource *DbResource) InitDeletionHistoryWatermark(mailBoxId int64, transaction *sqlx.Tx) (int64, error) {
	if transaction == nil {
		return 0, errors.New("deletion history watermark initialization requires a transaction")
	}
	if watermark, initialized, err := dbResource.GetDeletionHistoryWatermark(mailBoxId, transaction); err != nil {
		return 0, err
	} else if initialized {
		return watermark, nil
	}

	current, err := dbResource.readHighestModSeq(mailBoxId, transaction)
	if err != nil {
		return 0, err
	}

	query, args, err := statementbuilder.Squirrel.
		Update("mail_box").Prepared(true).
		Set(goqu.Record{"deletion_history_modseq": current}).
		Where(goqu.Ex{"id": mailBoxId}).
		ToSQL()
	if err != nil {
		return 0, err
	}
	if _, err := transaction.Exec(query, args...); err != nil {
		return 0, err
	}
	return current, nil
}
