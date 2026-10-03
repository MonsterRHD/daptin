package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/artpar/api2go/v2"
	daptinid "github.com/daptin/daptin/server/id"
	jwtmiddleware "github.com/daptin/daptin/server/jwt"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

func setupActiveScimAuthDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	statements := []string{
		`create table user_account (id integer primary key, email text, name text, reference_id blob, auth_version integer not null default 1, active bool not null default 1)`,
		`create table usergroup (id integer primary key, name text, reference_id blob)`,
		`create table user_account_user_account_id_has_usergroup_usergroup_id (
			id integer primary key,
			user_account_id integer,
			usergroup_id integer,
			reference_id blob,
			permission integer,
			created_at timestamp
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("setup statement failed: %v", err)
		}
	}
	return db
}

func scimAuthRequest(t *testing.T, secret []byte, issuer, email string, ref uuid.UUID) *http.Request {
	t.Helper()
	claims := jwt.MapClaims{
		"email":          email,
		"name":           "Active User",
		"sub":            ref.String(),
		"nbf":            time.Now().Add(-time.Minute).Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
		"iss":            issuer,
		"iat":            time.Now().Unix(),
		"jti":            uuid.New().String(),
		AuthVersionClaim: float64(1),
	}
	tokenString, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/user_account", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	return req
}

func TestAuthMiddlewareInactiveDeletedReactivated(t *testing.T) {
	db := setupActiveScimAuthDB(t)
	userRef := uuid.New()
	if _, err := db.Exec(`insert into user_account (id, email, name, reference_id, auth_version, active) values (1, ?, ?, ?, 1, 1)`,
		"active@example.com", "Active User", userRef[:]); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	oldJWTMiddleware := jwtMiddleware
	oldAuthCache := olricCache
	oldTokenCache := jwtmiddleware.TokenCache
	defer func() {
		jwtMiddleware = oldJWTMiddleware
		olricCache = oldAuthCache
		jwtmiddleware.TokenCache = oldTokenCache
	}()
	olricCache = nil
	jwtmiddleware.TokenCache = nil

	secret := []byte("jwt-secret-active")
	issuer := "issuer-active"
	_, olricClient := startTestOlric(t)
	InitJwtMiddleware(secret, issuer, olricClient)
	middleware := &AuthMiddleware{db: db, olricDb: olricClient}

	request := scimAuthRequest(t, secret, issuer, "active@example.com", userRef)
	if ok, abort, _ := middleware.AuthCheckMiddlewareWithHttp(request, httptest.NewRecorder(), false); !ok || abort {
		t.Fatalf("active user must authenticate, got ok=%v abort=%v", ok, abort)
	}

	// Deactivate: stale cache must be evicted, then the user is rejected.
	if _, err := db.Exec(`update user_account set active = 0 where id = 1`); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	InvalidateAuthCacheForEmail("active@example.com")
	if ok, _, _ := middleware.AuthCheckMiddlewareWithHttp(
		scimAuthRequest(t, secret, issuer, "active@example.com", userRef), httptest.NewRecorder(), false); ok {
		t.Fatal("deactivated user must be rejected without a session")
	}

	// Delete: a previously valid token must never build a zero-value session.
	if _, err := db.Exec(`delete from user_account where id = 1`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	InvalidateAuthCacheForEmail("active@example.com")
	if ok, abort, gotReq := middleware.AuthCheckMiddlewareWithHttp(
		scimAuthRequest(t, secret, issuer, "active@example.com", userRef), httptest.NewRecorder(), false); ok || abort {
		t.Fatalf("deleted user must be rejected, got ok=%v abort=%v", ok, abort)
	} else if sessionUser := gotReq.Context().Value("user"); sessionUser != nil {
		t.Fatalf("deleted user must not attach any session user, got %v", sessionUser)
	}

	// Re-create (re-onboard) as active: login works again.
	newRef := uuid.New()
	if _, err := db.Exec(`insert into user_account (id, email, name, reference_id, auth_version, active) values (1, ?, ?, ?, 1, 1)`,
		"active@example.com", "Active User", newRef[:]); err != nil {
		t.Fatalf("recreate user: %v", err)
	}
	InvalidateAuthCacheForEmail("active@example.com")
	if ok, abort, _ := middleware.AuthCheckMiddlewareWithHttp(
		scimAuthRequest(t, secret, issuer, "active@example.com", newRef), httptest.NewRecorder(), false); !ok || abort {
		t.Fatalf("reactivated user must authenticate, got ok=%v abort=%v", ok, abort)
	}
}

type mockBasicUserCrud struct {
	api2go.CRUD
	passwordHash string
	active       bool
}

func (m *mockBasicUserCrud) GetUserPassword(email string, _ *sqlx.Tx) (string, error) {
	return m.passwordHash, nil
}

func (m *mockBasicUserCrud) GetUserActiveByEmail(email string, _ *sqlx.Tx) (bool, error) {
	return m.active, nil
}

func TestBasicAuthRejectsInactiveUser(t *testing.T) {
	db := setupActiveScimAuthDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("pass123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}

	request := func() *http.Request {
		credentials := base64.StdEncoding.EncodeToString([]byte("user@example.com:pass123"))
		req := httptest.NewRequest(http.MethodGet, "/caldav/", nil)
		req.Header.Set("Authorization", "Basic "+credentials)
		return req
	}
	middleware := NewAuthMiddlewareBuilder(db, "issuer", nil)
	middleware.SetUserCrud(&mockBasicUserCrud{passwordHash: string(hash), active: true})

	if token, err := middleware.BasicAuthCheckMiddlewareWithHttp(request(), httptest.NewRecorder()); err != nil || token == nil {
		t.Fatalf("active user basic auth should succeed, token=%v err=%v", token, err)
	}

	middleware.SetUserCrud(&mockBasicUserCrud{passwordHash: string(hash), active: false})
	if token, _ := middleware.BasicAuthCheckMiddlewareWithHttp(request(), httptest.NewRecorder()); token != nil {
		t.Fatal("inactive user basic auth must not produce a token")
	}
}

func TestSessionUserBinaryRoundTripActive(t *testing.T) {
	for _, active := range []bool{true, false} {
		sessionUser := SessionUser{
			UserId:          9,
			UserReferenceId: daptinid.DaptinReferenceId(uuid.New()),
			AuthVersion:     3,
			Active:          active,
		}
		data, err := sessionUser.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded SessionUser
		if err := decoded.UnmarshalBinary(data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.Active != active {
			t.Fatalf("active round trip: want %v got %v", active, decoded.Active)
		}
	}
}

func TestSessionUserBinaryLegacyCacheDefaultsActive(t *testing.T) {
	referenceID := daptinid.DaptinReferenceId(uuid.New())
	sessionUser := SessionUser{UserId: 5, UserReferenceId: referenceID, AuthVersion: 2}
	data, err := sessionUser.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	// Strip the trailing active byte to simulate a pre-feature cache entry.
	legacy := data[:len(data)-1]
	var decoded SessionUser
	if err := decoded.UnmarshalBinary(legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if !decoded.Active {
		t.Fatal("legacy cache entries must default to active")
	}
}
