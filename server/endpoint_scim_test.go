package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daptin/daptin/server/resource"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

func newScimSwitchDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`create table _config (
		id integer primary key autoincrement,
		name text not null,
		configtype text not null,
		configstate text not null,
		configenv text not null,
		value text
	)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func scimProbe(router *gin.Engine) int {
	req := httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
	req.Header.Set("Authorization", "Bearer anything")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder.Code
}

func TestScimDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newScimSwitchDB(t)
	router := gin.New()
	if err := InitializeScimResources(router, db, nil, &resource.ConfigStore{}); err != nil {
		t.Fatal(err)
	}
	// Absent config is seeded to false and no route is registered.
	if code := scimProbe(router); code != http.StatusNotFound {
		t.Fatalf("SCIM must not be exposed when disabled, got %d", code)
	}
	var value string
	if err := db.Get(&value, `select value from _config where name = 'scim.enable'`); err != nil || value != "false" {
		t.Fatalf("expected scim.enable default false, got %q err=%v", value, err)
	}
}

func TestScimEnabledRequiresToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newScimSwitchDB(t)
	router := gin.New()
	if _, err := db.Exec(`insert into _config (name, configstate, configtype, configenv, value)
		values ('scim.enable', 'enabled', 'backend', '', 'true')`); err != nil {
		t.Fatal(err)
	}
	if err := InitializeScimResources(router, db, nil, &resource.ConfigStore{}); err != nil {
		t.Fatal(err)
	}
	if code := scimProbe(router); code != http.StatusNotFound {
		t.Fatalf("SCIM must stay unexposed without an admin token, got %d", code)
	}
}

func TestScimBufferedRequestPath(t *testing.T) {
	if !bufferedRequestPath(http.MethodPost, "/scim/v2/Users") {
		t.Fatal("POST /scim/v2/Users must be buffered")
	}
	if !bufferedRequestPath(http.MethodPatch, "/scim/v2/Groups/123") {
		t.Fatal("PATCH group must be buffered")
	}
	if bufferedRequestPath(http.MethodGet, "/scim/v2/Users") {
		t.Fatal("GET list must not be buffered")
	}
	if bufferedRequestPath(http.MethodPost, "/scim/v2/ServiceProviderConfig") {
		t.Fatal("discovery writes must not be buffered")
	}
}
