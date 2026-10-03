package scim

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/artpar/api2go/v2"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// userGroupJoinTable is the single existing authority for user/group
// membership and usergroup-based grants.
const userGroupJoinTable = "user_account_user_account_id_has_usergroup_usergroup_id"

// Store adapts SCIM operations onto the existing Daptin resource lifecycle.
// It owns no state: every request runs in one sqlx.Tx and all writes go
// through DbResource functions.
type Store struct {
	user  *resource.DbResource
	group *resource.DbResource
	join  *resource.DbResource
}

// NewStore builds a Store from the runtime resource map.
func NewStore(cruds map[string]*resource.DbResource) (*Store, error) {
	userCrud, ok := cruds[resource.USER_ACCOUNT_TABLE_NAME]
	if !ok {
		return nil, fmt.Errorf("user_account resource is not configured")
	}
	groupCrud, ok := cruds["usergroup"]
	if !ok {
		return nil, fmt.Errorf("usergroup resource is not configured")
	}
	joinCrud, ok := cruds[userGroupJoinTable]
	if !ok {
		return nil, fmt.Errorf("user/usergroup relation resource is not configured")
	}
	return &Store{user: userCrud, group: groupCrud, join: joinCrud}, nil
}

func (s *Store) begin() (*sqlx.Tx, error) {
	return s.user.Connection().Beginx()
}

func internalRequest(method, path string) api2go.Request {
	parsed, _ := url.Parse(path)
	return api2go.Request{
		PlainRequest: (&http.Request{Method: method, URL: parsed}).
			WithContext(context.Background()),
	}
}

// ---- time / row helpers ----

func timestampString(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case time.Time:
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format(time.RFC3339)
	case string:
		return parseTimestampString(v)
	case []byte:
		return parseTimestampString(string(v))
	default:
		return ""
	}
}

func parseTimestampString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func rowCreated(row map[string]interface{}) string {
	return timestampString(row["created_at"])
}

func rowLastModified(row map[string]interface{}) string {
	if value := timestampString(row["updated_at"]); value != "" {
		return value
	}
	return timestampString(row["created_at"])
}

func rowVersion(row map[string]interface{}) int64 {
	switch v := row["version"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 1
	}
}

func rowID(row map[string]interface{}) int64 {
	switch v := row["id"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func externalIDString(row map[string]interface{}) string {
	value, _ := row["external_id"].(string)
	return value
}

func parseReference(value string) (daptinid.DaptinReferenceId, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return daptinid.NullReferenceId, err
	}
	return daptinid.DaptinReferenceId(parsed), nil
}

// queryRows runs a read-only select built with goqu and maps rows.
func queryRows(transaction *sqlx.Tx, selectBuilder *goqu.SelectDataset) ([]map[string]interface{}, error) {
	query, args, err := selectBuilder.ToSQL()
	if err != nil {
		return nil, err
	}
	rows, err := transaction.Queryx(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]map[string]interface{}, 0)
	for rows.Next() {
		row := make(map[string]interface{})
		if err := rows.MapScan(row); err != nil {
			return nil, err
		}
		result = append(result, normalizeMapRow(row))
	}
	return result, rows.Err()
}

// normalizeMapRow converts MapScan []byte values into stable scalar forms,
// while leaving reference-id columns as raw UUID bytes.
func normalizeMapRow(row map[string]interface{}) map[string]interface{} {
	for key, value := range row {
		bytes, ok := value.([]byte)
		if !ok {
			continue
		}
		if key == "reference_id" || strings.HasSuffix(key, "_ref") {
			continue
		}
		row[key] = string(bytes)
	}
	return row
}

// GetUserRowTx loads a single user row inside a transaction.
func (s *Store) getUserRowTx(transaction *sqlx.Tx, reference daptinid.DaptinReferenceId) (map[string]interface{}, error) {
	row, _, err := s.user.GetSingleRowByReferenceIdWithTransaction(resource.USER_ACCOUNT_TABLE_NAME, reference, nil, transaction)
	return row, err
}

func (s *Store) getGroupRowTx(transaction *sqlx.Tx, reference daptinid.DaptinReferenceId) (map[string]interface{}, error) {
	row, _, err := s.group.GetSingleRowByReferenceIdWithTransaction("usergroup", reference, nil, transaction)
	return row, err
}

func (s *Store) userExistsTx(transaction *sqlx.Tx, reference daptinid.DaptinReferenceId) (bool, error) {
	rows, err := queryRows(transaction, statementbuilder.Squirrel.
		Select("id").From(resource.USER_ACCOUNT_TABLE_NAME).Prepared(true).
		Where(goqu.Ex{"reference_id": reference[:]}))
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

type groupMembershipRow struct {
	joinID    int64
	joinRef   daptinid.DaptinReferenceId
	userID    int64
	userRef   daptinid.DaptinReferenceId
	userEmail string
	userName  string
	groupID   int64
	groupRef  daptinid.DaptinReferenceId
	groupName string
}

func (s *Store) membershipRows(transaction *sqlx.Tx, where goqu.Expression) ([]groupMembershipRow, error) {
	builder := statementbuilder.Squirrel.Select(
		goqu.I("j.id").As("join_id"),
		goqu.I("j.reference_id").As("join_ref"),
		goqu.I("j.user_account_id").As("user_id"),
		goqu.I("j.usergroup_id").As("group_id"),
		goqu.I("u.reference_id").As("user_ref"),
		goqu.I("u.email").As("user_email"),
		goqu.I("u.name").As("user_name"),
		goqu.I("g.reference_id").As("group_ref"),
		goqu.I("g.name").As("group_name"),
	).From(goqu.T(userGroupJoinTable).As("j")).
		LeftJoin(goqu.T("user_account").As("u"), goqu.On(goqu.Ex{"u.id": goqu.I("j.user_account_id")})).
		LeftJoin(goqu.T("usergroup").As("g"), goqu.On(goqu.Ex{"g.id": goqu.I("j.usergroup_id")})).
		Where(where).Order(goqu.I("j.id").Asc()).Prepared(true)

	rows, err := queryRows(transaction, builder)
	if err != nil {
		return nil, err
	}
	memberships := make([]groupMembershipRow, 0, len(rows))
	for _, row := range rows {
		membership := groupMembershipRow{
			joinID:    int64FromAny(row["join_id"]),
			userID:    int64FromAny(row["user_id"]),
			groupID:   int64FromAny(row["group_id"]),
			userEmail: stringFromAny(row["user_email"]),
			userName:  stringFromAny(row["user_name"]),
			groupName: stringFromAny(row["group_name"]),
		}
		membership.joinRef = daptinid.InterfaceToDIR(row["join_ref"])
		membership.userRef = daptinid.InterfaceToDIR(row["user_ref"])
		membership.groupRef = daptinid.InterfaceToDIR(row["group_ref"])
		memberships = append(memberships, membership)
	}
	return memberships, nil
}

func int64FromAny(value interface{}) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		var parsed int64
		if _, err := fmt.Sscan(v, &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

func stringFromAny(value interface{}) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprintf("%v", value)
}
