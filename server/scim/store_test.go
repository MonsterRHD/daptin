package scim

import (
	"testing"

	"github.com/google/uuid"
)

func activePtr(value bool) *bool { return &value }

func TestUserCreateGetList(t *testing.T) {
	h := newScimHarness(t)

	_, baselineTotal, err := h.store.ListUsers(testBaseURL, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}

	created, err := h.store.CreateUser(testBaseURL, &User{
		Schemas:     []string{UserSchemaURI},
		UserName:    "Alice@Example.com",
		ExternalID:  "ext-alice",
		DisplayName: "Alice Example",
		Active:      activePtr(false),
		Emails:      []Email{{Value: "alice@example.com", Type: "work", Primary: true}},
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected stable id")
	}
	if created.UserName != "alice@example.com" {
		t.Fatalf("email should be normalized, got %s", created.UserName)
	}
	if created.Active == nil || *created.Active != false {
		t.Fatalf("active=false must be stored, got %v", created.Active)
	}
	if created.Meta.Version != `W/"1"` || created.Meta.Location != testBaseURL+"/Users/"+created.ID {
		t.Fatalf("unexpected meta: %+v", created.Meta)
	}
	if created.Meta.LastModified == "" || created.Meta.Created == "" {
		t.Fatal("meta timestamps must be populated")
	}
	// Users are seeded into the default "users" group by the resource path.
	if len(created.Groups) == 0 {
		t.Fatal("expected default users group membership")
	}

	fetched, err := h.store.GetUser(testBaseURL, created.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if fetched.ID != created.ID || fetched.Meta.Version != created.Meta.Version {
		t.Fatal("repeated reads must be stable")
	}

	if _, err := h.store.GetUser(testBaseURL, uuid.NewString()); err == nil {
		t.Fatal("missing user must error")
	}

	// Second user + pagination.
	if _, err := h.store.CreateUser(testBaseURL, &User{
		Schemas:  []string{UserSchemaURI},
		UserName: "bob@example.com",
	}); err != nil {
		t.Fatalf("create second user: %v", err)
	}
	pageOne, total, err := h.store.ListUsers(testBaseURL, nil, 1, 1)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if total != baselineTotal+2 || len(pageOne) != 1 {
		t.Fatalf("unexpected page: total=%d baseline=%d len=%d", total, baselineTotal, len(pageOne))
	}
	pageTwo, _, err := h.store.ListUsers(testBaseURL, nil, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pageTwo[0].ID == pageOne[0].ID {
		t.Fatal("pages must not overlap")
	}

	predicate, err := ParseFilter(ResourceTypeUser, `userName eq "bob@example.com"`)
	if err != nil {
		t.Fatal(err)
	}
	filtered, filteredTotal, err := h.store.ListUsers(testBaseURL, predicate, 1, 10)
	if err != nil || filteredTotal != 1 || len(filtered) != 1 || filtered[0].UserName != "bob@example.com" {
		t.Fatalf("filtered list mismatch: total=%d len=%d err=%v", filteredTotal, len(filtered), err)
	}
}

func TestListCountZeroKeepsTotal(t *testing.T) {
	h := newScimHarness(t)
	if _, err := h.store.CreateUser(testBaseURL, &User{Schemas: []string{UserSchemaURI}, UserName: "zero@example.com"}); err != nil {
		t.Fatal(err)
	}
	users, total, err := h.store.ListUsers(testBaseURL, nil, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 || total == 0 {
		t.Fatalf("count=0 must return no resources but the total count, got len=%d total=%d", len(users), total)
	}
}

func TestUserCreateDuplicatesAndDefaults(t *testing.T) {
	h := newScimHarness(t)
	first := &User{
		Schemas:    []string{UserSchemaURI},
		UserName:   "dup@example.com",
		ExternalID: "shared-ext",
	}
	if _, err := h.store.CreateUser(testBaseURL, first); err != nil {
		t.Fatal(err)
	}
	_, err := h.store.CreateUser(testBaseURL, &User{
		Schemas:  []string{UserSchemaURI},
		UserName: "dup@example.com",
	})
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 409 {
		t.Fatalf("duplicate userName should be 409 uniqueness, got %v", err)
	}
	_, err = h.store.CreateUser(testBaseURL, &User{
		Schemas:    []string{UserSchemaURI},
		UserName:   "other@example.com",
		ExternalID: "shared-ext",
	})
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 409 {
		t.Fatalf("duplicate externalId should be 409, got %v", err)
	}
	_, err = h.store.CreateUser(testBaseURL, &User{
		Schemas: []string{UserSchemaURI},
	})
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 400 {
		t.Fatalf("missing userName should be 400, got %v", err)
	}
	_, err = h.store.CreateUser(testBaseURL, &User{
		Schemas:  []string{UserSchemaURI},
		UserName: "not-an-email",
	})
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 400 {
		t.Fatalf("invalid email should be 400, got %v", err)
	}

	// Defaults: active=true when omitted.
	defaultUser, err := h.store.CreateUser(testBaseURL, &User{
		Schemas:  []string{UserSchemaURI},
		UserName: "default-active@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if defaultUser.Active == nil || !*defaultUser.Active {
		t.Fatal("omitted active must default true")
	}
	if defaultUser.DisplayName == "" {
		t.Fatal("display name should fall back to email local part")
	}
}

func TestUserPutPatchAndEtag(t *testing.T) {
	h := newScimHarness(t)
	user, err := h.store.CreateUser(testBaseURL, &User{
		Schemas:    []string{UserSchemaURI},
		UserName:   "put@example.com",
		ExternalID: "ext-put",
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := ParseETag(user.Meta.Version)
	if err != nil {
		t.Fatal(err)
	}
	stale := version - 1

	replaced, err := h.store.ReplaceUser(testBaseURL, user.ID, &User{
		Schemas:     []string{UserSchemaURI},
		UserName:    "put2@example.com",
		DisplayName: "Put User",
		// externalId omitted -> removed
		Active: activePtr(false),
	}, &version)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replaced.ExternalID != "" {
		t.Fatalf("omitted externalId must be removed, got %q", replaced.ExternalID)
	}
	if replaced.Active == nil || *replaced.Active != false {
		t.Fatal("active must be false")
	}
	newVersion, _ := ParseETag(replaced.Meta.Version)
	if newVersion <= version {
		t.Fatalf("version must advance: %d -> %d", version, newVersion)
	}

	if _, err := h.store.ReplaceUser(testBaseURL, user.ID, &User{
		Schemas:  []string{UserSchemaURI},
		UserName: "put3@example.com",
	}, &stale); err == nil {
		t.Fatal("stale If-Match must be rejected")
	} else if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 412 {
		t.Fatalf("expected 412, got %v", err)
	}

	current, _ := h.store.GetUser(testBaseURL, user.ID)
	currentVersion, _ := ParseETag(current.Meta.Version)
	patched, err := h.store.PatchUser(testBaseURL, user.ID, &PatchOp{
		Schemas: []string{PatchOpURI},
		Operations: []PatchOperation{
			{Op: "replace", Path: "active", Value: true},
			{Op: "add", Path: "externalId", Value: "ext-restored"},
		},
	}, &currentVersion)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.Active == nil || !*patched.Active {
		t.Fatal("patched active must be true")
	}
	if patched.ExternalID != "ext-restored" {
		t.Fatalf("external id patch failed: %s", patched.ExternalID)
	}

	// Atomicity: a bad operation in the batch must leave everything unchanged.
	before, _ := h.store.GetUser(testBaseURL, user.ID)
	beforeVersion, _ := ParseETag(before.Meta.Version)
	_, err = h.store.PatchUser(testBaseURL, user.ID, &PatchOp{
		Schemas: []string{PatchOpURI},
		Operations: []PatchOperation{
			{Op: "replace", Path: "displayName", Value: "Should Not Persist"},
			{Op: "replace", Path: "userName", Value: "not-an-email"},
		},
	}, nil)
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 400 {
		t.Fatalf("expected 400 from invalid op, got %v", err)
	}
	after, _ := h.store.GetUser(testBaseURL, user.ID)
	afterVersion, _ := ParseETag(after.Meta.Version)
	if after.DisplayName != before.DisplayName || afterVersion != beforeVersion {
		t.Fatalf("failed PATCH must not partially persist: before=%q/%d after=%q/%d",
			before.DisplayName, beforeVersion, after.DisplayName, afterVersion)
	}
}

func TestGroupMembershipConsistency(t *testing.T) {
	h := newScimHarness(t)
	u1, err := h.store.CreateUser(testBaseURL, &User{Schemas: []string{UserSchemaURI}, UserName: "g1@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	u2, err := h.store.CreateUser(testBaseURL, &User{Schemas: []string{UserSchemaURI}, UserName: "g2@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	created, err := h.store.CreateGroup(testBaseURL, &Group{
		Schemas:     []string{GroupSchemaURI},
		DisplayName: "Engineering",
		ExternalID:  "grp-eng",
		Members: []Member{
			{Value: u1.ID, Type: ResourceTypeUser},
			{Value: u2.ID},
		},
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if len(created.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(created.Members))
	}
	u1Fetched, _ := h.store.GetUser(testBaseURL, u1.ID)
	found := false
	for _, group := range u1Fetched.Groups {
		if group.Value == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("user groups view must be consistent with group members")
	}

	version, _ := ParseETag(created.Meta.Version)
	patched, err := h.store.PatchGroup(testBaseURL, created.ID, &PatchOp{
		Schemas: []string{PatchOpURI},
		Operations: []PatchOperation{
			{Op: "remove", Path: `members[value eq "` + u2.ID + `"]`},
			{Op: "replace", Path: "displayName", Value: "Engineering Renamed"},
		},
	}, &version)
	if err != nil {
		t.Fatalf("patch group: %v", err)
	}
	if patched.DisplayName != "Engineering Renamed" {
		t.Fatal("group rename failed")
	}
	if len(patched.Members) != 1 || patched.Members[0].Value != u1.ID {
		t.Fatalf("only u1 should remain, got %+v", patched.Members)
	}

	// Unknown user / cross-resource type rejected with no changes.
	before := len(patched.Members)
	_, err = h.store.PatchGroup(testBaseURL, created.ID, &PatchOp{
		Schemas: []string{PatchOpURI},
		Operations: []PatchOperation{
			{Op: "add", Path: "members", Value: map[string]interface{}{"value": uuid.NewString()}},
		},
	}, nil)
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 400 {
		t.Fatalf("unknown member must be 400, got %v", err)
	}
	_, err = h.store.PatchGroup(testBaseURL, created.ID, &PatchOp{
		Schemas: []string{PatchOpURI},
		Operations: []PatchOperation{
			{Op: "add", Path: "members", Value: map[string]interface{}{
				"value": created.ID, "type": "Group",
			}},
		},
	}, nil)
	if apiErr, ok := err.(*APIError); !ok || apiErr.Status != 400 {
		t.Fatalf("group-type member must be 400, got %v", err)
	}
	unchanged, _ := h.store.GetGroup(testBaseURL, created.ID)
	if len(unchanged.Members) != before {
		t.Fatal("rejected member PATCH must not modify membership")
	}

	// Full PUT replacement with empty members clears the group.
	emptyVersion, _ := ParseETag(unchanged.Meta.Version)
	cleared, err := h.store.ReplaceGroup(testBaseURL, created.ID, &Group{
		Schemas:     []string{GroupSchemaURI},
		DisplayName: "Empty Group",
	}, &emptyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.Members) != 0 {
		t.Fatalf("group must be empty, got %d", len(cleared.Members))
	}
}

func TestDeleteGroupCascadesMembership(t *testing.T) {
	h := newScimHarness(t)
	u1, _ := h.store.CreateUser(testBaseURL, &User{Schemas: []string{UserSchemaURI}, UserName: "del1@example.com"})
	group, err := h.store.CreateGroup(testBaseURL, &Group{
		Schemas:     []string{GroupSchemaURI},
		DisplayName: "Doomed",
		Members:     []Member{{Value: u1.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteGroup(group.ID, nil); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if _, err := h.store.GetGroup(testBaseURL, group.ID); err == nil {
		t.Fatal("group must be gone")
	}
	user, _ := h.store.GetUser(testBaseURL, u1.ID)
	for _, groupRef := range user.Groups {
		if groupRef.Value == group.ID {
			t.Fatal("membership row must be removed with the group")
		}
	}
}

func TestDeleteUserPreservesBusinessRecords(t *testing.T) {
	h := newScimHarness(t)
	user, err := h.store.CreateUser(testBaseURL, &User{Schemas: []string{UserSchemaURI}, UserName: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a business record owned by the user (fixture-side SQL only).
	if _, err := h.db.Exec(`create table widget (id integer primary key, name text, user_account_id integer)`); err != nil {
		t.Fatal(err)
	}
	ref, err := parseReference(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var userID int64
	if err := h.db.Get(&userID, `select id from user_account where reference_id = ?`, ref[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`insert into widget (id, name, user_account_id) values (1, 'kept', ?)`, userID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteUser(user.ID, nil); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := h.store.GetUser(testBaseURL, user.ID); err == nil {
		t.Fatal("user must be gone")
	}
	var owner interface{}
	if err := h.db.Get(&owner, `select user_account_id from widget where id = 1`); err != nil {
		t.Fatal(err)
	}
	if int64FromAny(owner) != userID {
		t.Fatalf("owned record preserved unchanged, got %v", owner)
	}
}

func TestOptimisticVersionConflict(t *testing.T) {
	h := newScimHarness(t)
	user, err := h.store.CreateUser(testBaseURL, &User{Schemas: []string{UserSchemaURI}, UserName: "conf@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := ParseETag(user.Meta.Version)
	if _, err := h.store.ReplaceUser(testBaseURL, user.ID, &User{
		Schemas:  []string{UserSchemaURI},
		UserName: "conf@example.com",
	}, &first); err != nil {
		t.Fatal(err)
	}

	// Build a stale snapshot (version from before the commit) and update
	// through the same resource lifecycle path: WHERE version=? must reject.
	ref, err := parseReference(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := h.store.begin()
	if err != nil {
		t.Fatal(err)
	}
	staleRow := map[string]interface{}{
		"id":           int64(2),
		"reference_id": ref.String(),
		"email":        "conf@example.com",
		"name":         "Conf",
		"version":      int64(1),
	}
	err = h.store.applyUserUpdate(tx, staleRow, map[string]interface{}{
		"name": "Raced",
	})
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("expected optimistic lock conflict")
	}
	mapped := mapWriteError(err)
	if apiErr, ok := mapped.(*APIError); !ok || apiErr.Status != 409 {
		t.Fatalf("expected 409 version conflict, got %v", mapped)
	}
}

func TestStoreRestartConsistency(t *testing.T) {
	h := newScimHarness(t)
	user, err := h.store.CreateUser(testBaseURL, &User{
		Schemas:    []string{UserSchemaURI},
		UserName:   "restart@example.com",
		ExternalID: "ext-restart",
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := h.store.CreateGroup(testBaseURL, &Group{
		Schemas:     []string{GroupSchemaURI},
		DisplayName: "Restart Group",
		Members:     []Member{{Value: user.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A new store over the same database must observe identical state.
	newStore, err := NewStore(h.cruds)
	if err != nil {
		t.Fatal(err)
	}
	reloadedUser, err := newStore.GetUser(testBaseURL, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	reloadedGroup, err := newStore.GetGroup(testBaseURL, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedUser.ExternalID != "ext-restart" || reloadedUser.Meta.Version != user.Meta.Version {
		t.Fatalf("user state mismatch: %+v vs %+v", reloadedUser, user)
	}
	if len(reloadedGroup.Members) != 1 || reloadedGroup.Members[0].Value != user.ID {
		t.Fatalf("group membership mismatch after restart: %+v", reloadedGroup.Members)
	}
}
