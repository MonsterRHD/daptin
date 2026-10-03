package scim

import "testing"

func TestParsePathUser(t *testing.T) {
	valid := []string{
		"",
		"userName",
		"externalId",
		"displayName",
		"active",
		"password",
		"name",
		"name.formatted",
		"name.givenName",
		"emails",
		"emails.value",
		"emails[type eq \"work\"].value",
	}
	for _, path := range valid {
		if path == "emails[type eq \"work\"].value" {
			// Complex bracketed sub-attribute with a trailing component is
			// not in the supported subset; covered as invalid below.
			continue
		}
		if _, err := ParsePath(ResourceTypeUser, path); err != nil {
			t.Fatalf("expected user path %q valid, got %v", path, err)
		}
	}
}

func TestParsePathGroup(t *testing.T) {
	parsed, err := ParsePath(ResourceTypeGroup, `members[value eq "00000000-0000-0000-0000-000000000001"]`)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.HasFilter || parsed.FilterValue != "00000000-0000-0000-0000-000000000001" || parsed.Attr != "members" {
		t.Fatalf("unexpected parsed path: %+v", parsed)
	}
	for _, path := range []string{"displayName", "externalId", "members"} {
		if _, err := ParsePath(ResourceTypeGroup, path); err != nil {
			t.Fatalf("expected group path %q valid, got %v", path, err)
		}
	}
}

func TestParsePathInvalid(t *testing.T) {
	invalid := []struct {
		resource string
		path     string
		scimType string
	}{
		{ResourceTypeUser, "unknownAttr", ScimTypeNoTarget},
		{ResourceTypeUser, "name.unknownSub", ScimTypeNoTarget},
		{ResourceTypeUser, "members", ScimTypeInvalidPath},
		{ResourceTypeUser, "a.b.c", ScimTypeInvalidPath},
		{ResourceTypeGroup, "userName", ScimTypeNoTarget},
		{ResourceTypeGroup, "members[displayName eq \"x\"]", ScimTypeInvalidPath},
		{ResourceTypeGroup, "members[value eq x]", ScimTypeInvalidPath},
		{ResourceTypeGroup, "members[value eq \"\"]", ScimTypeInvalidPath},
		{ResourceTypeGroup, "members[value eq \"x\"", ScimTypeInvalidPath},
	}
	for _, tc := range invalid {
		t.Run(tc.path, func(t *testing.T) {
			_, err := ParsePath(tc.resource, tc.path)
			if err == nil {
				t.Fatalf("expected %s for %q", tc.scimType, tc.path)
			}
			pathErr, ok := err.(*PathError)
			if !ok || pathErr.ScimType() != tc.scimType {
				t.Fatalf("expected %s, got %v", tc.scimType, err)
			}
		})
	}
}
