package scim

import (
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

func rowReference(row map[string]interface{}) daptinid.DaptinReferenceId {
	return daptinid.InterfaceToDIR(row["reference_id"])
}

func userLocation(baseURL string, id string) string {
	return baseURL + "/" + UsersPath + "/" + id
}

func groupLocation(baseURL string, id string) string {
	return baseURL + "/" + GroupsPath + "/" + id
}

func (s *Store) buildUser(transaction *sqlx.Tx, row map[string]interface{}, baseURL string) (*User, error) {
	reference := rowReference(row)
	id := reference.String()
	email := stringFromAny(row["email"])
	displayName := stringFromAny(row["name"])

	user := &User{
		Schemas:     []string{UserSchemaURI},
		ID:          id,
		ExternalID:  externalIDString(row),
		UserName:    email,
		DisplayName: displayName,
		Active:      boolPtr(resource.RowValueActive(row[resource.ACTIVE_COLUMN_NAME])),
		Meta: &Meta{
			ResourceType: ResourceTypeUser,
			Created:      rowCreated(row),
			LastModified: rowLastModified(row),
			Location:     userLocation(baseURL, id),
			Version:      ETag(rowVersion(row)),
		},
	}
	if displayName != "" {
		user.Name = &Name{Formatted: displayName}
	}
	if email != "" {
		user.Emails = []Email{{Value: email, Type: "work", Primary: true}}
	}

	memberships, err := s.membershipRows(transaction, goqu.Ex{"j.user_account_id": rowID(row)})
	if err != nil {
		return nil, err
	}
	user.Groups = make([]GroupRef, 0, len(memberships))
	for _, membership := range memberships {
		if membership.groupRef == daptinid.NullReferenceId {
			continue
		}
		user.Groups = append(user.Groups, GroupRef{
			Type:    ResourceTypeGroup,
			Value:   membership.groupRef.String(),
			Ref:     groupLocation(baseURL, membership.groupRef.String()),
			Display: membership.groupName,
		})
	}
	return user, nil
}

func (s *Store) buildGroup(transaction *sqlx.Tx, row map[string]interface{}, baseURL string) (*Group, error) {
	reference := rowReference(row)
	id := reference.String()
	group := &Group{
		Schemas:     []string{GroupSchemaURI},
		ID:          id,
		ExternalID:  externalIDString(row),
		DisplayName: stringFromAny(row["name"]),
		Members:     []Member{},
		Meta: &Meta{
			ResourceType: ResourceTypeGroup,
			Created:      rowCreated(row),
			LastModified: rowLastModified(row),
			Location:     groupLocation(baseURL, id),
			Version:      ETag(rowVersion(row)),
		},
	}
	memberships, err := s.membershipRows(transaction, goqu.Ex{"j.usergroup_id": rowID(row)})
	if err != nil {
		return nil, err
	}
	for _, membership := range memberships {
		if membership.userRef == daptinid.NullReferenceId {
			continue
		}
		display := membership.userEmail
		if display == "" {
			display = membership.userName
		}
		group.Members = append(group.Members, Member{
			Type:    ResourceTypeUser,
			Value:   membership.userRef.String(),
			Ref:     userLocation(baseURL, membership.userRef.String()),
			Display: display,
		})
	}
	return group, nil
}

func boolPtr(value bool) *bool {
	return &value
}

// listPage loads one deterministic, ordered page plus the total match count.
// Both reads happen in the caller's single transaction, so the page is a
// consistent snapshot.
func listPage(transaction *sqlx.Tx, tableName string, predicate goqu.Expression, startIndex, count int) ([]map[string]interface{}, int, error) {
	totalQuery := statementbuilderCount(tableName, predicate)
	totalSQL, totalArgs, err := totalQuery.ToSQL()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := transaction.Get(&total, totalSQL, totalArgs...); err != nil {
		return nil, 0, err
	}

	// count=0 returns no resources while totalResults stays accurate. goqu
	// omits a zero LIMIT, so skip the row query explicitly.
	if count == 0 {
		return []map[string]interface{}{}, total, nil
	}

	builder := statementbuilderSelectFrom(tableName, predicate)
	builder = builder.Order(goqu.I("id").Asc()).Limit(uint(count))
	if startIndex > 1 {
		builder = builder.Offset(uint(startIndex - 1))
	}
	rows, err := queryRows(transaction, builder)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func statementbuilderCount(tableName string, predicate goqu.Expression) *goqu.SelectDataset {
	builder := statementbuilder.Squirrel.Select(goqu.COUNT("*")).From(tableName).Prepared(true)
	if predicate != nil {
		builder = builder.Where(predicate)
	}
	return builder
}

func statementbuilderSelectFrom(tableName string, predicate goqu.Expression) *goqu.SelectDataset {
	builder := statementbuilder.Squirrel.Select(goqu.L("*")).From(tableName).Prepared(true)
	if predicate != nil {
		builder = builder.Where(predicate)
	}
	return builder
}
