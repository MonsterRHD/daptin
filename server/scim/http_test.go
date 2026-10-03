package scim

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

const testAdminToken = "scim-admin-secret"

func newScimRouter(t *testing.T) (*gin.Engine, *scimHarness) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := newScimHarness(t)
	router := gin.New()
	RegisterRoutes(router.Group(BasePath), h.store, testAdminToken)
	return router, h
}

func scimRequest(t *testing.T, router *gin.Engine, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", ContentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testAdminToken}
}

func TestScimTokenAuth(t *testing.T) {
	router, _ := newScimRouter(t)

	resp := scimRequest(t, router, http.MethodGet, BasePath+"/ServiceProviderConfig", nil, nil)
	if resp.Code != http.StatusUnauthorized || resp.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing token must be 401 with challenge, got %d", resp.Code)
	}
	resp = scimRequest(t, router, http.MethodGet, BasePath+"/ServiceProviderConfig", nil,
		map[string]string{"Authorization": "Bearer wrong"})
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token must be 401, got %d", resp.Code)
	}
	resp = scimRequest(t, router, http.MethodGet, BasePath+"/ServiceProviderConfig", nil,
		map[string]string{"X-API-Key": testAdminToken})
	if resp.Code != http.StatusOK {
		t.Fatalf("X-API-Key must authenticate, got %d", resp.Code)
	}
	if resp.Header().Get("Content-Type") != ContentType {
		t.Fatalf("content type must be scim+json, got %q", resp.Header().Get("Content-Type"))
	}
}

func TestScimDiscoveryEndpoints(t *testing.T) {
	router, _ := newScimRouter(t)
	headers := authHeaders()
	for _, path := range []string{
		BasePath + "/ServiceProviderConfig",
		BasePath + "/ResourceTypes",
		BasePath + "/ResourceTypes/User",
		BasePath + "/Schemas",
		BasePath + "/Schemas/" + UserSchemaURI,
	} {
		resp := scimRequest(t, router, http.MethodGet, path, nil, headers)
		if resp.Code != http.StatusOK {
			t.Fatalf("discovery %s expected 200, got %d: %s", path, resp.Code, resp.Body.String())
		}
	}
	resp := scimRequest(t, router, http.MethodGet, BasePath+"/ResourceTypes/Missing", nil, headers)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("missing resource type should be 404, got %d", resp.Code)
	}
}

func TestScimUserHTTPLifecycle(t *testing.T) {
	router, _ := newScimRouter(t)
	headers := authHeaders()

	created := scimRequest(t, router, http.MethodPost, BasePath+"/Users", &User{
		Schemas:     []string{UserSchemaURI},
		UserName:    "http-user@example.com",
		DisplayName: "HTTP User",
		Active:      activePtr(true),
	}, headers)
	if created.Code != http.StatusCreated {
		t.Fatalf("create expected 201, got %d: %s", created.Code, created.Body.String())
	}
	if created.Header().Get("Location") == "" || created.Header().Get("ETag") == "" {
		t.Fatal("201 must include Location and ETag")
	}
	var user User
	if err := json.Unmarshal(created.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.Meta.Location != created.Header().Get("Location") {
		t.Fatal("location header and body must agree")
	}

	fetched := scimRequest(t, router, http.MethodGet, BasePath+"/Users/"+user.ID, nil, headers)
	if fetched.Code != http.StatusOK || fetched.Header().Get("ETag") != user.Meta.Version {
		t.Fatalf("get should return 200 with matching ETag, got %d", fetched.Code)
	}

	list := scimRequest(t, router, http.MethodGet,
		BasePath+"/Users?filter=userName%20eq%20%22http-user%40example.com%22&startIndex=1&count=10", nil, headers)
	if list.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", list.Code, list.Body.String())
	}
	var page ListResponse[User]
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.TotalResults != 1 || len(page.Resources) != 1 || page.StartIndex != 1 {
		t.Fatalf("unexpected page: %+v", page)
	}

	badFilter := scimRequest(t, router, http.MethodGet, BasePath+"/Groups?filter=unknown%20eq%201", nil, headers)
	if badFilter.Code != http.StatusBadRequest {
		t.Fatalf("bad filter expected 400, got %d", badFilter.Code)
	}

	patched := scimRequest(t, router, http.MethodPatch, BasePath+"/Users/"+user.ID, &PatchOp{
		Schemas: []string{PatchOpURI},
		Operations: []PatchOperation{
			{Op: "replace", Path: "displayName", Value: "Renamed User"},
		},
	}, headers)
	if patched.Code != http.StatusOK {
		t.Fatalf("patch expected 200, got %d: %s", patched.Code, patched.Body.String())
	}

	ifMatch := map[string]string{
		"Authorization": "Bearer " + testAdminToken,
		"If-Match":      user.Meta.Version,
	}
	put := scimRequest(t, router, http.MethodPut, BasePath+"/Users/"+user.ID, &User{
		Schemas:  []string{UserSchemaURI},
		UserName: "http-user@example.com",
	}, ifMatch)
	if put.Code != http.StatusPreconditionFailed {
		t.Fatalf("PUT against the old version should be 412, got %d", put.Code)
	}

	deleted := scimRequest(t, router, http.MethodDelete, BasePath+"/Users/"+user.ID, nil, headers)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete expected 204, got %d: %s", deleted.Code, deleted.Body.String())
	}
	gone := scimRequest(t, router, http.MethodGet, BasePath+"/Users/"+user.ID, nil, headers)
	if gone.Code != http.StatusNotFound {
		t.Fatalf("get after delete expected 404, got %d", gone.Code)
	}
}

func TestScimGroupHTTPRejectsBadBodies(t *testing.T) {
	router, _ := newScimRouter(t)
	headers := authHeaders()

	missingName := scimRequest(t, router, http.MethodPost, BasePath+"/Groups", &Group{
		Schemas: []string{GroupSchemaURI},
	}, headers)
	if missingName.Code != http.StatusBadRequest {
		t.Fatalf("group without displayName expected 400, got %d", missingName.Code)
	}

	// Unknown member must produce SCIM 400 with invalidValue scimType.
	unknown := scimRequest(t, router, http.MethodPost, BasePath+"/Groups", &Group{
		Schemas:     []string{GroupSchemaURI},
		DisplayName: "With Unknown",
		Members:     []Member{{Value: "00000000-0000-0000-0000-000000000001"}},
	}, headers)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown member expected 400, got %d: %s", unknown.Code, unknown.Body.String())
	}
	var errDoc ErrorResponse
	if err := json.Unmarshal(unknown.Body.Bytes(), &errDoc); err != nil {
		t.Fatal(err)
	}
	if errDoc.ScimType != ScimTypeInvalidValue {
		t.Fatalf("expected invalidValue scimType, got %q", errDoc.ScimType)
	}

	// Malformed JSON must be rejected with invalidSyntax.
	req := httptest.NewRequest(http.MethodPost, BasePath+"/Users", bytes.NewBufferString("{not json"))
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON expected 400, got %d", recorder.Code)
	}
}
