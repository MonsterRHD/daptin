package scim

import (
	"net/http"
	"net/mail"
	"strings"

	"github.com/artpar/api2go/v2"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/resource"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

// userWriteState is the validated target state of a user_account write.
type userWriteState struct {
	email          string
	name           string
	active         bool
	hasActive      bool
	externalID     *string
	password       *string
	removePassword bool
}

func defaultUserCreateState() userWriteState {
	return userWriteState{active: true, hasActive: true}
}

func normalizeEmailAddress(value string) (string, error) {
	address, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return "", invalidValue("userName must be a valid email address")
	}
	return strings.ToLower(address.Address), nil
}

func normalizeExternalID(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// composeDisplayName resolves the single user_account.name column from the
// SCIM displayName/name components.
func composeDisplayName(displayName string, name *Name) string {
	if strings.TrimSpace(displayName) != "" {
		return strings.TrimSpace(displayName)
	}
	if name != nil {
		if strings.TrimSpace(name.Formatted) != "" {
			return strings.TrimSpace(name.Formatted)
		}
		parts := make([]string, 0, 2)
		if strings.TrimSpace(name.GivenName) != "" {
			parts = append(parts, strings.TrimSpace(name.GivenName))
		}
		if strings.TrimSpace(name.FamilyName) != "" {
			parts = append(parts, strings.TrimSpace(name.FamilyName))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	return ""
}

func primaryEmail(emails []Email) (string, bool) {
	fallback := ""
	for _, email := range emails {
		value := strings.TrimSpace(email.Value)
		if value == "" {
			continue
		}
		if email.Primary || strings.EqualFold(email.Type, "work") {
			return value, true
		}
		if fallback == "" {
			fallback = value
		}
	}
	return fallback, fallback != ""
}

func (state *userWriteState) applyUserAttributes(attrs map[string]interface{}) error {
	if value, ok := attrs["userName"]; ok && value != nil {
		text, ok := value.(string)
		if !ok {
			return invalidValue("userName must be a string")
		}
		email, err := normalizeEmailAddress(text)
		if err != nil {
			return err
		}
		state.email = email
	}
	if value, ok := attrs["externalId"]; ok {
		if value == nil {
			state.externalID = nil
		} else if text, ok := value.(string); ok {
			state.externalID = normalizeExternalID(text)
		} else {
			return invalidValue("externalId must be a string")
		}
	}
	if value, ok := attrs["active"]; ok && value != nil {
		active, ok := value.(bool)
		if !ok {
			return invalidValue("active must be a boolean")
		}
		state.active = active
		state.hasActive = true
	}
	if value, ok := attrs["password"]; ok && value != nil {
		password, ok := value.(string)
		if !ok || password == "" {
			return invalidValue("password must be a non-empty string")
		}
		state.password = &password
	}
	displayName, _ := attrs["displayName"].(string)
	var nameComponent *Name
	if value, ok := attrs["name"]; ok && value != nil {
		nameMap, ok := value.(map[string]interface{})
		if !ok {
			return invalidValue("name must be a complex object")
		}
		nameComponent = &Name{
			Formatted:  stringFromAny(nameMap["formatted"]),
			GivenName:  stringFromAny(nameMap["givenName"]),
			FamilyName: stringFromAny(nameMap["familyName"]),
		}
	}
	if _, ok := attrs["displayName"]; ok || nameComponent != nil {
		state.name = composeDisplayName(displayName, nameComponent)
	}
	if value, ok := attrs["emails"]; ok && value != nil {
		email, err := emailFromValue(value)
		if err != nil {
			return err
		}
		if state.email == "" {
			normalized, err := normalizeEmailAddress(email)
			if err != nil {
				return err
			}
			state.email = normalized
		}
	}
	return nil
}

func emailFromValue(value interface{}) (string, error) {
	items, ok := value.([]interface{})
	if !ok {
		return "", invalidValue("emails must be an array")
	}
	emails := make([]Email, 0, len(items))
	for index, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return "", invalidValue("emails entries must be objects")
		}
		email := Email{
			Value:   stringFromAny(entry["value"]),
			Type:    stringFromAny(entry["type"]),
			Display: stringFromAny(entry["display"]),
		}
		if primary, ok := entry["primary"].(bool); ok {
			email.Primary = primary
		}
		if index == 0 && !email.Primary && email.Type == "" {
			email.Primary = true
		}
		emails = append(emails, email)
	}
	value2, ok := primaryEmail(emails)
	if !ok {
		return "", invalidValue("at least one emails entry must have a value")
	}
	return value2, nil
}

func userStateFromRow(row map[string]interface{}) userWriteState {
	state := userWriteState{
		email:     stringFromAny(row["email"]),
		name:      stringFromAny(row["name"]),
		active:    resource.RowValueActive(row[resource.ACTIVE_COLUMN_NAME]),
		hasActive: true,
	}
	if externalID := externalIDString(row); externalID != "" {
		state.externalID = &externalID
	}
	return state
}

func (state userWriteState) updateAttributes() map[string]interface{} {
	attrs := map[string]interface{}{
		"email":                     state.email,
		"name":                      state.name,
		resource.ACTIVE_COLUMN_NAME: state.active,
		"external_id":               nil,
	}
	if state.externalID != nil {
		attrs["external_id"] = *state.externalID
	}
	if state.password != nil {
		attrs["password"] = *state.password
	} else if state.removePassword {
		attrs["password"] = nil
	}
	return attrs
}

// CreateUser provisions a user_account row through the resource lifecycle.
func (s *Store) CreateUser(baseURL string, input *User) (*User, error) {
	state := defaultUserCreateState()
	rawAttributes := userInputAttributes(input)
	if err := state.applyUserAttributes(rawAttributes); err != nil {
		return nil, err
	}
	if state.email == "" {
		return nil, invalidValue("userName is required")
	}
	if len(input.Emails) > 0 {
		primary, ok := primaryEmail(input.Emails)
		if ok && !strings.EqualFold(primary, state.email) {
			return nil, invalidValue("primary emails value must match userName")
		}
	}
	if state.name == "" {
		state.name = strings.Split(state.email, "@")[0]
	}

	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}
	created, err := s.user.CreateWithoutFilter(
		api2go.NewApi2GoModelWithData(resource.USER_ACCOUNT_TABLE_NAME, nil, 0, nil, state.createAttributes()),
		internalRequest(http.MethodPost, "/"+resource.USER_ACCOUNT_TABLE_NAME), transaction)
	if err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	reference := daptinid.InterfaceToDIR(created["reference_id"])
	row, err := s.getUserRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	user, err := s.buildUser(transaction, row, baseURL)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return user, nil
}

func (state userWriteState) createAttributes() map[string]interface{} {
	attrs := map[string]interface{}{
		"email":                     state.email,
		"name":                      state.name,
		resource.ACTIVE_COLUMN_NAME: state.active,
	}
	if state.externalID != nil {
		attrs["external_id"] = *state.externalID
	}
	if state.password != nil {
		attrs["password"] = *state.password
	}
	return attrs
}

func userInputAttributes(input *User) map[string]interface{} {
	attrs := map[string]interface{}{
		"userName":    input.UserName,
		"displayName": input.DisplayName,
	}
	if input.Password != "" {
		attrs["password"] = input.Password
	}
	if input.Active != nil {
		attrs["active"] = *input.Active
	}
	if input.ExternalID != "" {
		attrs["externalId"] = input.ExternalID
	}
	if input.Name != nil {
		attrs["name"] = map[string]interface{}{
			"formatted":  input.Name.Formatted,
			"givenName":  input.Name.GivenName,
			"familyName": input.Name.FamilyName,
		}
	}
	if len(input.Emails) > 0 {
		items := make([]interface{}, 0, len(input.Emails))
		for _, email := range input.Emails {
			items = append(items, map[string]interface{}{
				"value": email.Value, "type": email.Type, "primary": email.Primary,
			})
		}
		attrs["emails"] = items
	}
	return attrs
}

// GetUser returns one SCIM user.
func (s *Store) GetUser(baseURL, id string) (*User, error) {
	reference, err := parseReference(id)
	if err != nil {
		return nil, notFound("user not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	row, err := s.getUserRowTx(transaction, reference)
	if err != nil {
		return nil, notFound("user not found")
	}
	return s.buildUser(transaction, row, baseURL)
}

// ListUsers returns one page of SCIM users and the total match count.
func (s *Store) ListUsers(baseURL string, predicate goqu.Expression, startIndex, count int) ([]*User, int, error) {
	transaction, err := s.begin()
	if err != nil {
		return nil, 0, err
	}
	defer transaction.Rollback()
	rows, total, err := listPage(transaction, resource.USER_ACCOUNT_TABLE_NAME, predicate, startIndex, count)
	if err != nil {
		return nil, 0, err
	}
	users := make([]*User, 0, len(rows))
	for _, row := range rows {
		user, buildErr := s.buildUser(transaction, row, baseURL)
		if buildErr != nil {
			return nil, 0, buildErr
		}
		users = append(users, user)
	}
	return users, total, nil
}

// ReplaceUser applies SCIM PUT full-replace semantics.
func (s *Store) ReplaceUser(baseURL, id string, input *User, ifMatch *int64) (*User, error) {
	reference, err := parseReference(id)
	if err != nil {
		return nil, notFound("user not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}
	row, err := s.getUserRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return nil, notFound("user not found")
	}
	if err := checkIfMatch(row, ifMatch); err != nil {
		_ = transaction.Rollback()
		return nil, err
	}

	// PUT resets optional attributes to defaults when omitted.
	state := userStateFromRow(row)
	state.active = true
	state.hasActive = true
	state.externalID = nil
	if err := state.applyUserAttributes(userInputAttributes(input)); err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	if state.email == "" {
		_ = transaction.Rollback()
		return nil, invalidValue("userName is required")
	}
	if state.name == "" {
		state.name = strings.Split(state.email, "@")[0]
	}
	if err := s.applyUserUpdate(transaction, row, state.updateAttributes()); err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	return s.reloadAndCommitUser(transaction, reference, baseURL)
}

// PatchUser applies one or more validated PATCH operations atomically.
func (s *Store) PatchUser(baseURL, id string, patch *PatchOp, ifMatch *int64) (*User, error) {
	reference, err := parseReference(id)
	if err != nil {
		return nil, notFound("user not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}
	row, err := s.getUserRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return nil, notFound("user not found")
	}
	if err := checkIfMatch(row, ifMatch); err != nil {
		_ = transaction.Rollback()
		return nil, err
	}

	state := userStateFromRow(row)
	for _, operation := range patch.Operations {
		if err := applyUserPatchOperation(&state, operation); err != nil {
			_ = transaction.Rollback()
			return nil, err
		}
	}
	if state.email == "" {
		_ = transaction.Rollback()
		return nil, invalidValue("userName cannot be removed")
	}
	if err := s.applyUserUpdate(transaction, row, state.updateAttributes()); err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	return s.reloadAndCommitUser(transaction, reference, baseURL)
}

func (s *Store) applyUserUpdate(transaction *sqlx.Tx, row map[string]interface{}, attrs map[string]interface{}) error {
	model := api2go.NewApi2GoModelWithData(resource.USER_ACCOUNT_TABLE_NAME, nil, 0, nil, row)
	model.SetAttributes(attrs)
	_, err := s.user.UpdateWithoutFilters(model,
		internalRequest(http.MethodPatch, "/"+resource.USER_ACCOUNT_TABLE_NAME), transaction)
	return err
}

func (s *Store) reloadAndCommitUser(transaction *sqlx.Tx, reference daptinid.DaptinReferenceId, baseURL string) (*User, error) {
	row, err := s.getUserRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	user, err := s.buildUser(transaction, row, baseURL)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return user, nil
}

// DeleteUser removes the user_account via the resource lifecycle. Owned
// business records are preserved by the existing delete semantics.
func (s *Store) DeleteUser(id string, ifMatch *int64) error {
	reference, err := parseReference(id)
	if err != nil {
		return notFound("user not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return err
	}
	row, err := s.getUserRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return notFound("user not found")
	}
	if err := checkIfMatch(row, ifMatch); err != nil {
		_ = transaction.Rollback()
		return err
	}
	if err := s.user.DeleteWithoutFilters(reference,
		internalRequest(http.MethodDelete, "/"+resource.USER_ACCOUNT_TABLE_NAME), transaction); err != nil {
		_ = transaction.Rollback()
		return mapWriteError(err)
	}
	return transaction.Commit()
}

func checkIfMatch(row map[string]interface{}, ifMatch *int64) error {
	if ifMatch != nil && *ifMatch != rowVersion(row) {
		return preconditionFailed("If-Match version does not match the current resource version")
	}
	return nil
}

// applyUserPatchOperation validates and folds one PATCH operation into the
// target state without touching the database.
func applyUserPatchOperation(state *userWriteState, operation PatchOperation) error {
	op := strings.ToLower(strings.TrimSpace(operation.Op))
	if op != "add" && op != "replace" && op != "remove" {
		return &APIError{Status: 400, ScimType: ScimTypeInvalidSyntax, Detail: "op must be add, replace or remove"}
	}
	target, err := ParsePath(ResourceTypeUser, operation.Path)
	if err != nil {
		return pathToAPIError(err)
	}

	if target.Attr == "" {
		if op == "remove" {
			return invalidSyntax("remove requires a path")
		}
		attrs, ok := operation.Value.(map[string]interface{})
		if !ok {
			return invalidValue("PATCH value must be an attribute object when path is omitted")
		}
		return state.applyUserAttributes(attrs)
	}

	if op == "remove" {
		switch target.Attr {
		case "externalId":
			state.externalID = nil
			return nil
		case "password":
			state.password = nil
			state.removePassword = true
			return nil
		case "userName":
			return invalidValue("userName cannot be removed")
		case "active":
			return invalidValue("active cannot be removed")
		default:
			// Removing an optional sub-attribute (name components, emails)
			// simply leaves the single mapped column unchanged.
			return nil
		}
	}

	switch target.Attr {
	case "userName":
		text, ok := operation.Value.(string)
		if !ok {
			return invalidValue("userName must be a string")
		}
		email, err := normalizeEmailAddress(text)
		if err != nil {
			return err
		}
		state.email = email
	case "externalId":
		text, ok := operation.Value.(string)
		if !ok {
			return invalidValue("externalId must be a string")
		}
		state.externalID = normalizeExternalID(text)
	case "displayName":
		text, ok := operation.Value.(string)
		if !ok {
			return invalidValue("displayName must be a string")
		}
		state.name = strings.TrimSpace(text)
	case "active":
		active, ok := operation.Value.(bool)
		if !ok {
			return invalidValue("active must be a boolean")
		}
		state.active = active
		state.hasActive = true
	case "password":
		password, ok := operation.Value.(string)
		if !ok {
			return invalidValue("password must be a string")
		}
		state.password = &password
	case "name":
		nameMap, ok := operation.Value.(map[string]interface{})
		if !ok {
			return invalidValue("name must be a complex object")
		}
		state.name = composeDisplayName("", &Name{
			Formatted:  stringFromAny(nameMap["formatted"]),
			GivenName:  stringFromAny(nameMap["givenName"]),
			FamilyName: stringFromAny(nameMap["familyName"]),
		})
	case "emails":
		if target.SubAttr == "value" {
			text, ok := operation.Value.(string)
			if !ok {
				return invalidValue("emails.value must be a string")
			}
			email, err := normalizeEmailAddress(text)
			if err != nil {
				return err
			}
			state.email = email
		} else {
			email, err := emailFromValue(operation.Value)
			if err != nil {
				return err
			}
			normalized, err := normalizeEmailAddress(email)
			if err != nil {
				return err
			}
			state.email = normalized
		}
	}
	return nil
}

func pathToAPIError(err error) error {
	if pathErr, ok := err.(*PathError); ok {
		return &APIError{Status: 400, ScimType: pathErr.ScimType(), Detail: pathErr.Error()}
	}
	return err
}
