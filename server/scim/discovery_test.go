package scim

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestServiceProviderConfigDocument(t *testing.T) {
	doc := ServiceProviderConfig()
	if len(doc.Schemas) != 1 || doc.Schemas[0] != ServiceConfigURI {
		t.Fatalf("unexpected schemas: %v", doc.Schemas)
	}
	if !doc.Patch.Supported || !doc.Filter.Supported || !doc.ETag.Supported {
		t.Fatal("patch, filter and etag must be supported")
	}
	if doc.Bulk.Supported || doc.Sort.Supported {
		t.Fatal("bulk and sort must not be advertised")
	}
	if doc.Filter.MaxResults != maxFilterResults {
		t.Fatalf("filter maxResults mismatch: %d", doc.Filter.MaxResults)
	}
	if _, ok := doc.AuthenticationSchemes["bearer"]; !ok {
		t.Fatal("bearer authentication scheme must be documented")
	}
}

func TestResourceTypesDocument(t *testing.T) {
	docs := ResourceTypes()
	if len(docs) != 2 {
		t.Fatalf("expected 2 resource types, got %d", len(docs))
	}
	endpoints := map[string]string{}
	for _, doc := range docs {
		endpoints[doc.Name] = doc.Endpoint
		if len(doc.Schemas) != 1 || doc.Schemas[0] != ResourceTypeURI {
			t.Fatalf("resource type %s has wrong schemas: %v", doc.Name, doc.Schemas)
		}
	}
	if endpoints["User"] != UsersPath || endpoints["Group"] != GroupsPath {
		t.Fatalf("unexpected endpoints: %v", endpoints)
	}
	if _, ok := ResourceType("User"); !ok {
		t.Fatal("User resource type must resolve")
	}
	if _, ok := ResourceType("Missing"); ok {
		t.Fatal("unknown resource type must not resolve")
	}
}

func TestSchemasDocument(t *testing.T) {
	all := Schemas()
	ids := make(map[string]bool)
	for _, schema := range all {
		ids[schema.ID] = true
		if len(schema.Schemas) != 1 || schema.Schemas[0] != SchemaURI {
			t.Fatalf("schema %s has wrong schema URN list: %v", schema.ID, schema.Schemas)
		}
		if len(schema.Attributes) == 0 {
			t.Fatalf("schema %s must declare attributes", schema.ID)
		}
	}
	for _, id := range []string{UserSchemaURI, GroupSchemaURI, PatchOpURI, ListResponseURI, ErrorURI, ServiceConfigURI, SchemaURI, ResourceTypeURI} {
		if !ids[id] {
			t.Fatalf("schemas list must contain %s", id)
		}
		if _, ok := SchemaByID(id); !ok {
			t.Fatalf("schema %s must resolve by id", id)
		}
	}

	userSchema, _ := SchemaByID(UserSchemaURI)
	userAttrs := map[string]SchemaAttribute{}
	for _, attr := range userSchema.Attributes {
		userAttrs[attr.Name] = attr
	}
	userName, ok := userAttrs["userName"]
	if !ok || !userName.Required || userName.Uniqueness != "server" {
		t.Fatalf("userName must be required and server-unique: %+v", userName)
	}
	active, ok := userAttrs["active"]
	if !ok || active.Type != "boolean" {
		t.Fatalf("active attribute missing: %+v", active)
	}

	groupSchema, _ := SchemaByID(GroupSchemaURI)
	var members SchemaAttribute
	for _, attr := range groupSchema.Attributes {
		if attr.Name == "members" {
			members = attr
		}
	}
	if !members.MultiValued || len(members.SubAttributes) == 0 {
		t.Fatalf("members must be a multi-valued complex attribute: %+v", members)
	}
}

func TestErrorDocumentShape(t *testing.T) {
	doc := NewError(409, ScimTypeUniqueness, "already exists")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, fragment := range []string{ErrorURI, `"status":"409"`, `"scimType":"uniqueness"`, "already exists"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("error document %s missing %s", text, fragment)
		}
	}
}
