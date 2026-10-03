package scim

import (
	"net/http"
	"sort"
	"strings"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/resource"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

type groupWriteState struct {
	name       string
	externalID *string
}

func groupStateFromRow(row map[string]interface{}) groupWriteState {
	state := groupWriteState{name: stringFromAny(row["name"])}
	if externalID := externalIDString(row); externalID != "" {
		state.externalID = &externalID
	}
	return state
}

func (state groupWriteState) attributes() map[string]interface{} {
	attrs := map[string]interface{}{
		"name":        state.name,
		"external_id": nil,
	}
	if state.externalID != nil {
		attrs["external_id"] = *state.externalID
	}
	return attrs
}

// CreateGroup creates a usergroup and its initial membership in one
// transaction.
func (s *Store) CreateGroup(baseURL string, input *Group) (*Group, error) {
	name := strings.TrimSpace(input.DisplayName)
	if name == "" {
		return nil, invalidValue("displayName is required")
	}
	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}

	created, err := s.group.CreateWithoutFilter(
		api2go.NewApi2GoModelWithData("usergroup", nil, 0, nil, map[string]interface{}{
			"name": name,
		}), internalRequest(http.MethodPost, "/usergroup"), transaction)
	if err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	groupRef := daptinid.InterfaceToDIR(created["reference_id"])
	groupRow, err := s.getGroupRowTx(transaction, groupRef)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}

	if input.ExternalID != "" {
		model := api2go.NewApi2GoModelWithData("usergroup", nil, 0, nil, groupRow)
		model.SetAttributes(map[string]interface{}{"external_id": strings.TrimSpace(input.ExternalID)})
		if _, err := s.group.UpdateWithoutFilters(model,
			internalRequest(http.MethodPatch, "/usergroup"), transaction); err != nil {
			_ = transaction.Rollback()
			return nil, mapWriteError(err)
		}
		groupRow, _ = s.getGroupRowTx(transaction, groupRef)
	}

	if len(input.Members) > 0 {
		plan, err := s.planMembers(transaction, groupRow, []membershipOperation{
			{kind: memberActionReplace, values: input.Members},
		})
		if err != nil {
			_ = transaction.Rollback()
			return nil, err
		}
		if err := s.applyMemberPlan(transaction, groupRow, plan); err != nil {
			_ = transaction.Rollback()
			return nil, mapWriteError(err)
		}
	}
	return s.reloadAndCommitGroup(transaction, groupRef, baseURL)
}

// GetGroup returns one SCIM group.
func (s *Store) GetGroup(baseURL, id string) (*Group, error) {
	reference, err := parseReference(id)
	if err != nil {
		return nil, notFound("group not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	row, err := s.getGroupRowTx(transaction, reference)
	if err != nil {
		return nil, notFound("group not found")
	}
	return s.buildGroup(transaction, row, baseURL)
}

// ListGroups returns one page of SCIM groups and the total match count.
func (s *Store) ListGroups(baseURL string, predicate goqu.Expression, startIndex, count int) ([]*Group, int, error) {
	transaction, err := s.begin()
	if err != nil {
		return nil, 0, err
	}
	defer transaction.Rollback()
	rows, total, err := listPage(transaction, "usergroup", predicate, startIndex, count)
	if err != nil {
		return nil, 0, err
	}
	groups := make([]*Group, 0, len(rows))
	for _, row := range rows {
		group, buildErr := s.buildGroup(transaction, row, baseURL)
		if buildErr != nil {
			return nil, 0, buildErr
		}
		groups = append(groups, group)
	}
	return groups, total, nil
}

// ReplaceGroup applies SCIM PUT full-replace semantics, including full
// membership replacement.
func (s *Store) ReplaceGroup(baseURL, id string, input *Group, ifMatch *int64) (*Group, error) {
	reference, err := parseReference(id)
	if err != nil {
		return nil, notFound("group not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}
	row, err := s.getGroupRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return nil, notFound("group not found")
	}
	if err := checkIfMatch(row, ifMatch); err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	if strings.TrimSpace(input.DisplayName) == "" {
		_ = transaction.Rollback()
		return nil, invalidValue("displayName is required")
	}
	state := groupStateFromRow(row)
	state.name = strings.TrimSpace(input.DisplayName)
	state.externalID = normalizeExternalID(input.ExternalID)
	if err := s.applyGroupUpdate(transaction, row, state.attributes()); err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	row, _ = s.getGroupRowTx(transaction, reference)
	plan, err := s.planMembers(transaction, row, []membershipOperation{
		{kind: memberActionReplace, values: input.Members},
	})
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	if err := s.applyMemberPlan(transaction, row, plan); err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	return s.reloadAndCommitGroup(transaction, reference, baseURL)
}

// PatchGroup applies attribute and membership PATCH operations atomically.
func (s *Store) PatchGroup(baseURL, id string, patch *PatchOp, ifMatch *int64) (*Group, error) {
	reference, err := parseReference(id)
	if err != nil {
		return nil, notFound("group not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return nil, err
	}
	row, err := s.getGroupRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return nil, notFound("group not found")
	}
	if err := checkIfMatch(row, ifMatch); err != nil {
		_ = transaction.Rollback()
		return nil, err
	}

	state := groupStateFromRow(row)
	memberOps := make([]membershipOperation, 0)
	for _, operation := range patch.Operations {
		attrTarget, memberOp, err := applyGroupPatchOperation(&state, operation)
		if err != nil {
			_ = transaction.Rollback()
			return nil, err
		}
		if attrTarget == "members" && memberOp != nil {
			memberOps = append(memberOps, *memberOp)
		}
	}
	if err := s.applyGroupUpdate(transaction, row, state.attributes()); err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	row, _ = s.getGroupRowTx(transaction, reference)
	plan, err := s.planMembers(transaction, row, memberOps)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	if err := s.applyMemberPlan(transaction, row, plan); err != nil {
		_ = transaction.Rollback()
		return nil, mapWriteError(err)
	}
	return s.reloadAndCommitGroup(transaction, reference, baseURL)
}

type memberActionKind int

const (
	memberActionNone memberActionKind = iota
	memberActionAdd
	memberActionReplace
	memberActionRemoveAll
	memberActionRemoveIDs
)

type membershipOperation struct {
	kind   memberActionKind
	values []Member
	id     string
}

func applyGroupPatchOperation(state *groupWriteState, operation PatchOperation) (string, *membershipOperation, error) {
	op := strings.ToLower(strings.TrimSpace(operation.Op))
	if op != "add" && op != "replace" && op != "remove" {
		return "", nil, invalidSyntax("op must be add, replace or remove")
	}
	target, err := ParsePath(ResourceTypeGroup, operation.Path)
	if err != nil {
		return "", nil, pathToAPIError(err)
	}
	if target.Attr == "" {
		if op == "remove" {
			return "", nil, invalidSyntax("remove requires a path")
		}
		attrs, ok := operation.Value.(map[string]interface{})
		if !ok {
			return "", nil, invalidValue("PATCH value must be an attribute object")
		}
		if displayName, ok := attrs["displayName"]; ok {
			if err := applyDisplayName(state, displayName); err != nil {
				return "", nil, err
			}
		}
		if externalID, ok := attrs["externalId"]; ok {
			applyExternalID(state, externalID)
		}
		if rawMembers, ok := attrs["members"]; ok {
			kind := memberActionAdd
			if op == "replace" {
				kind = memberActionReplace
			}
			return "members", &membershipOperation{kind: kind, values: operationValuesToMembers(rawMembers)}, nil
		}
		return "", nil, nil
	}

	switch target.Attr {
	case "displayName":
		if op == "remove" {
			return "", nil, invalidValue("displayName cannot be removed")
		}
		return "", nil, applyDisplayName(state, operation.Value)
	case "externalId":
		if op == "remove" {
			state.externalID = nil
			return "", nil, nil
		}
		applyExternalID(state, operation.Value)
		return "", nil, nil
	case "members":
		if op == "replace" {
			if target.HasFilter {
				return "", nil, &APIError{Status: 400, ScimType: ScimTypeInvalidPath, Detail: "replace cannot combine with a member value filter"}
			}
			return target.Attr, &membershipOperation{kind: memberActionReplace, values: operationValuesToMembers(operation.Value)}, nil
		}
		if op == "remove" {
			if target.HasFilter {
				return target.Attr, &membershipOperation{kind: memberActionRemoveIDs, id: target.FilterValue}, nil
			}
			if operation.Value != nil {
				return target.Attr, &membershipOperation{kind: memberActionRemoveIDs,
					values: operationValuesToMembers(operation.Value)}, nil
			}
			return target.Attr, &membershipOperation{kind: memberActionRemoveAll}, nil
		}
		return target.Attr, &membershipOperation{kind: memberActionAdd,
			values: operationValuesToMembers(operation.Value)}, nil
	}
	return "", nil, nil
}

func applyDisplayName(state *groupWriteState, value interface{}) error {
	text, ok := value.(string)
	if !ok {
		return invalidValue("displayName must be a string")
	}
	if strings.TrimSpace(text) == "" {
		return invalidValue("displayName cannot be empty")
	}
	state.name = strings.TrimSpace(text)
	return nil
}

func applyExternalID(state *groupWriteState, value interface{}) {
	text, _ := value.(string)
	state.externalID = normalizeExternalID(text)
}

// operationValuesToMembers accepts a single value, a member object or an
// array of either.
func operationValuesToMembers(value interface{}) []Member {
	members := make([]Member, 0)
	switch typed := value.(type) {
	case nil:
		return members
	case string:
		members = append(members, Member{Value: typed})
	case map[string]interface{}:
		members = append(members, memberFromMap(typed))
	case []interface{}:
		for _, item := range typed {
			switch entry := item.(type) {
			case string:
				members = append(members, Member{Value: entry})
			case map[string]interface{}:
				members = append(members, memberFromMap(entry))
			}
		}
	}
	return members
}

func memberFromMap(entry map[string]interface{}) Member {
	member := Member{
		Value:   stringFromAny(entry["value"]),
		Type:    stringFromAny(entry["type"]),
		Ref:     stringFromAny(entry["$ref"]),
		Display: stringFromAny(entry["display"]),
	}
	if member.Value == "" && member.Ref != "" {
		member.Value = lastPathSegment(member.Ref)
	}
	return member
}

func lastPathSegment(ref string) string {
	index := strings.LastIndex(ref, "/")
	if index >= 0 && index+1 < len(ref) {
		return ref[index+1:]
	}
	return ref
}

type memberPlan struct {
	add    []int64
	remove []daptinid.DaptinReferenceId
}

// planMembers validates every referenced member against user_account and
// folds the ordered PATCH operations into a join-row delta against the
// current membership snapshot. Validation completes before any join write.
func (s *Store) planMembers(transaction *sqlx.Tx, groupRow map[string]interface{},
	operations []membershipOperation) (*memberPlan, error) {

	currentRows, err := s.membershipRows(transaction, goqu.Ex{"j.usergroup_id": rowID(groupRow)})
	if err != nil {
		return nil, err
	}
	currentUsers := make(map[int64]groupMembershipRow)
	desired := make(map[int64]bool)
	for _, membership := range currentRows {
		currentUsers[membership.userID] = membership
		desired[membership.userID] = true
	}

	resolve := func(member Member) (int64, error) {
		if member.Type != "" && !strings.EqualFold(member.Type, ResourceTypeUser) {
			return 0, invalidValue("group members must reference User resources")
		}
		reference, err := parseReference(member.Value)
		if err != nil {
			return 0, invalidValue("member value must be a valid user UUID")
		}
		exists, err := s.userExistsTx(transaction, reference)
		if err != nil {
			return 0, err
		}
		if !exists {
			return 0, invalidValue("member user does not exist: " + member.Value)
		}
		id, err := resource.GetReferenceIdToIdWithTransaction(resource.USER_ACCOUNT_TABLE_NAME, reference, transaction)
		if err != nil {
			return 0, err
		}
		return id, nil
	}

	resolveSet := func(members []Member, target map[int64]bool) error {
		for _, member := range members {
			id, err := resolve(member)
			if err != nil {
				return err
			}
			target[id] = true
		}
		return nil
	}

	for _, operation := range operations {
		switch operation.kind {
		case memberActionAdd:
			if err := resolveSet(operation.values, desired); err != nil {
				return nil, err
			}
		case memberActionReplace:
			desired = make(map[int64]bool)
			if err := resolveSet(operation.values, desired); err != nil {
				return nil, err
			}
		case memberActionRemoveAll:
			desired = make(map[int64]bool)
		case memberActionRemoveIDs:
			ids := make([]string, 0, len(operation.values)+1)
			if operation.id != "" {
				ids = append(ids, operation.id)
			}
			for _, member := range operation.values {
				ids = append(ids, member.Value)
			}
			for _, idText := range ids {
				reference, err := parseReference(idText)
				if err != nil {
					return nil, invalidValue("removed member value must be a valid user UUID")
				}
				id, err := resource.GetReferenceIdToIdWithTransaction(resource.USER_ACCOUNT_TABLE_NAME, reference, transaction)
				if err != nil {
					return nil, err
				}
				delete(desired, id)
			}
		}
	}

	plan := &memberPlan{add: make([]int64, 0), remove: make([]daptinid.DaptinReferenceId, 0)}
	for id := range desired {
		if _, exists := currentUsers[id]; !exists {
			plan.add = append(plan.add, id)
		}
	}
	for id, membership := range currentUsers {
		if !desired[id] {
			plan.remove = append(plan.remove, membership.joinRef)
		}
	}
	sort.Slice(plan.add, func(i, j int) bool { return plan.add[i] < plan.add[j] })
	sort.Slice(plan.remove, func(i, j int) bool { return plan.remove[i].String() < plan.remove[j].String() })
	return plan, nil
}

func (s *Store) applyMemberPlan(transaction *sqlx.Tx, groupRow map[string]interface{}, plan *memberPlan) error {
	groupRef := rowReference(groupRow)
	for _, joinRef := range plan.remove {
		if err := s.join.DeleteWithoutFilters(joinRef,
			internalRequest(http.MethodDelete, "/"+userGroupJoinTable), transaction); err != nil {
			return err
		}
	}
	for _, userID := range plan.add {
		userRef, err := resource.GetIdToReferenceIdWithTransaction(resource.USER_ACCOUNT_TABLE_NAME, userID, transaction)
		if err != nil {
			return err
		}
		_, err = s.join.CreateWithoutFilter(
			api2go.NewApi2GoModelWithData(userGroupJoinTable, nil, int64(auth.DEFAULT_PERMISSION), nil, map[string]interface{}{
				"user_account_id": userRef.String(),
				"usergroup_id":    groupRef.String(),
				"permission":      int64(auth.DEFAULT_PERMISSION),
			}), internalRequest(http.MethodPost, "/"+userGroupJoinTable), transaction)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyGroupUpdate(transaction *sqlx.Tx, row map[string]interface{}, attrs map[string]interface{}) error {
	model := api2go.NewApi2GoModelWithData("usergroup", nil, 0, nil, row)
	model.SetAttributes(attrs)
	_, err := s.group.UpdateWithoutFilters(model,
		internalRequest(http.MethodPatch, "/usergroup"), transaction)
	return err
}

func (s *Store) reloadAndCommitGroup(transaction *sqlx.Tx, reference daptinid.DaptinReferenceId, baseURL string) (*Group, error) {
	row, err := s.getGroupRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	group, err := s.buildGroup(transaction, row, baseURL)
	if err != nil {
		_ = transaction.Rollback()
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return group, nil
}

// DeleteGroup removes every join row referencing the group (user memberships
// and resource grants) through the relation resources, then the group itself,
// all in one transaction.
func (s *Store) DeleteGroup(id string, ifMatch *int64) error {
	reference, err := parseReference(id)
	if err != nil {
		return notFound("group not found")
	}
	transaction, err := s.begin()
	if err != nil {
		return err
	}
	row, err := s.getGroupRowTx(transaction, reference)
	if err != nil {
		_ = transaction.Rollback()
		return notFound("group not found")
	}
	if err := checkIfMatch(row, ifMatch); err != nil {
		_ = transaction.Rollback()
		return err
	}

	groupInternalID := rowID(row)
	for tableName, joinCrud := range s.group.Cruds {
		if !strings.HasSuffix(tableName, "_has_usergroup_usergroup_id") {
			continue
		}
		rows, queryErr := queryRows(transaction, statementbuilderSelectFrom(tableName,
			goqu.Ex{"usergroup_id": groupInternalID}))
		if queryErr != nil {
			_ = transaction.Rollback()
			return queryErr
		}
		for _, joinRow := range rows {
			joinRef := daptinid.InterfaceToDIR(joinRow["reference_id"])
			if joinRef == daptinid.NullReferenceId {
				continue
			}
			if err := joinCrud.DeleteWithoutFilters(joinRef,
				internalRequest(http.MethodDelete, "/"+tableName), transaction); err != nil {
				_ = transaction.Rollback()
				return mapWriteError(err)
			}
		}
	}

	if err := s.group.DeleteWithoutFilters(reference,
		internalRequest(http.MethodDelete, "/usergroup"), transaction); err != nil {
		_ = transaction.Rollback()
		return mapWriteError(err)
	}
	return transaction.Commit()
}
