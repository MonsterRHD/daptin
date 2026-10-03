package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buraksezer/olric"
	olricConfig "github.com/buraksezer/olric/config"
	"github.com/daptin/daptin/server"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/statementbuilder"
)

const scimE2EToken = "e2e-scim-admin-token"

func freeScimE2EPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func startScimE2EOlric(t *testing.T) (*olric.Olric, *olric.EmbeddedClient) {
	t.Helper()
	port := freeScimE2EPort(t)
	started := make(chan struct{})
	cfg := olricConfig.New("local")
	cfg.BindAddr = "127.0.0.1"
	cfg.BindPort = port
	cfg.MemberlistConfig.BindAddr = "127.0.0.1"
	cfg.MemberlistConfig.BindPort = 0
	cfg.MemberlistConfig.Name = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.Started = func() { close(started) }
	emb, err := olric.New(cfg)
	if err != nil {
		t.Fatalf("olric: %v", err)
	}
	go func() { _ = emb.Start() }()
	<-started
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = emb.Shutdown(ctx)
	})
	return emb, emb.NewEmbeddedClient()
}

// startScimRuntime builds a full in-process Daptin runtime with SCIM enabled.
func startScimRuntime(t *testing.T, dbPath string, olricClient *olric.EmbeddedClient) http.Handler {
	t.Helper()
	resource.ResetCachesForTests()
	t.Cleanup(resource.ResetCachesForTests)
	statementbuilder.InitialiseStatementBuilder("sqlite3")
	db, err := server.GetDbConnection("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := resource.NewConfigStore(db)
	if err != nil {
		t.Fatal(err)
	}
	tx := db.MustBegin()
	for key, value := range map[string]string{
		"scim.enable":      "true",
		"scim.admin_token": scimE2EToken,
		"yjs.enable":       "false",
		"caldav.enable":    "false",
		"imap.enabled":     "false",
	} {
		if err := store.SetConfigValueFor(key, value, "backend", tx); err != nil {
			t.Fatalf("config %s: %v", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	boxRoot := http.Dir(t.TempDir())
	runtimeInstance, err := server.NewRuntime(context.Background(), boxRoot, db, t.TempDir(), olricClient, "scim-e2e")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runtimeInstance.Quiesce()
		_ = runtimeInstance.Drain(ctx)
	})
	return runtimeInstance.Handler
}

func scimE2ERequest(t *testing.T, handler http.Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/scim+json")
	req.Header.Set("Authorization", "Bearer "+scimE2EToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestSCIMServiceEndToEndLifecycle(t *testing.T) {
	_, olricClient := startScimE2EOlric(t)
	dbPath := filepath.Join(t.TempDir(), "scim-e2e.db")
	handler := startScimRuntime(t, dbPath, olricClient)

	// Discovery is reachable through the real router with admin token.
	resp := scimE2ERequest(t, handler, http.MethodGet, "/scim/v2/ServiceProviderConfig", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("SPC expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	// Provision a user with a password through SCIM.
	provision := scimE2ERequest(t, handler, http.MethodPost, "/scim/v2/Users", map[string]interface{}{
		"schemas":     []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName":    "e2e-user@example.com",
		"displayName": "E2E User",
		"active":      true,
		"password":    "Sup3rSecret!",
		"emails":      []map[string]interface{}{{"value": "e2e-user@example.com", "type": "work", "primary": true}},
	})
	if provision.Code != http.StatusCreated {
		t.Fatalf("provision expected 201, got %d: %s", provision.Code, provision.Body.String())
	}
	var user struct {
		ID   string `json:"id"`
		Meta struct {
			Version string `json:"version"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(provision.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}

	// signin must succeed for the active, password-enabled user.
	signinBody := map[string]interface{}{"email": "e2e-user@example.com", "password": "Sup3rSecret!"}
	signinReq := httptest.NewRequest(http.MethodPost, "/action/user_account/signin",
		bytes.NewReader(mustJSON(signinBody)))
	signinReq.Header.Set("Content-Type", "application/json")
	signinRec := httptest.NewRecorder()
	handler.ServeHTTP(signinRec, signinReq)
	if signinRec.Code != http.StatusOK || !strings.Contains(signinRec.Body.String(), "token") {
		t.Fatalf("active user signin should succeed, got %d: %s", signinRec.Code, signinRec.Body.String())
	}

	// Deactivate through SCIM PATCH.
	patch := scimE2ERequest(t, handler, http.MethodPatch, "/scim/v2/Users/"+user.ID, map[string]interface{}{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]interface{}{
			{"op": "replace", "path": "active", "value": false},
		},
	})
	if patch.Code != http.StatusOK {
		t.Fatalf("deactivate patch expected 200, got %d: %s", patch.Code, patch.Body.String())
	}

	// signin must now be refused on the same live runtime.
	signinReq2 := httptest.NewRequest(http.MethodPost, "/action/user_account/signin",
		bytes.NewReader(mustJSON(signinBody)))
	signinReq2.Header.Set("Content-Type", "application/json")
	signinRec2 := httptest.NewRecorder()
	handler.ServeHTTP(signinRec2, signinReq2)
	if !strings.Contains(signinRec2.Body.String(), "Invalid username or password") {
		t.Fatalf("deactivated signin must be rejected as invalid credentials, got %d: %s",
			signinRec2.Code, signinRec2.Body.String())
	}

	// Re-enable; login works again.
	reactivate := scimE2ERequest(t, handler, http.MethodPatch, "/scim/v2/Users/"+user.ID, map[string]interface{}{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]interface{}{
			{"op": "replace", "path": "active", "value": true},
		},
	})
	if reactivate.Code != http.StatusOK {
		t.Fatalf("reactivate expected 200, got %d", reactivate.Code)
	}
	signinReq3 := httptest.NewRequest(http.MethodPost, "/action/user_account/signin",
		bytes.NewReader(mustJSON(signinBody)))
	signinReq3.Header.Set("Content-Type", "application/json")
	signinRec3 := httptest.NewRecorder()
	handler.ServeHTTP(signinRec3, signinReq3)
	if signinRec3.Code != http.StatusOK {
		t.Fatalf("reactivated user signin should succeed, got %d: %s", signinRec3.Code, signinRec3.Body.String())
	}

	// Restart a fresh runtime over the same database: SCIM stays enabled and
	// the provisioned identity/version is intact.
	handler2 := startScimRuntime(t, dbPath, olricClient)
	fetched := scimE2ERequest(t, handler2, http.MethodGet, "/scim/v2/Users/"+user.ID, nil)
	if fetched.Code != http.StatusOK {
		t.Fatalf("user must survive runtime restart, got %d: %s", fetched.Code, fetched.Body.String())
	}
	var restarted struct {
		ExternalID string `json:"externalId"`
		UserName   string `json:"userName"`
		Active     bool   `json:"active"`
	}
	if err := json.Unmarshal(fetched.Body.Bytes(), &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.UserName != "e2e-user@example.com" || !restarted.Active {
		t.Fatalf("restarted state mismatch: %+v", restarted)
	}
}

func mustJSON(value map[string]interface{}) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
