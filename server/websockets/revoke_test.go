package websockets

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

func dialRevokeClient(t *testing.T, wsURL string, userRef daptinid.DaptinReferenceId) *websocket.Conn {
	t.Helper()
	config, err := websocket.NewConfig(wsURL, "http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	// Drain the session-open frame.
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read session frame: %v", err)
	}
	return conn
}

func TestCloseUserSessionsOnlyAffectsTargetUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := NewServer("/live", nil, nil, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		ref := daptinid.DaptinReferenceId(uuid.MustParse(c.Request.URL.Query().Get("u")))
		ctx := context.WithValue(c.Request.Context(), "user", &auth.SessionUser{UserId: 1, UserReferenceId: ref})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	server.Register(router)
	go server.Listen()

	httpServer := httptest.NewServer(router)
	defer httpServer.Close()

	u, _ := url.Parse(httpServer.URL)
	wsURL := "ws://" + u.Host + "/live?u="
	userA := daptinid.DaptinReferenceId(uuid.New())
	userB := daptinid.DaptinReferenceId(uuid.New())
	connA := dialRevokeClient(t, wsURL+userA.String(), userA)
	connB := dialRevokeClient(t, wsURL+userB.String(), userB)

	// The session-open frames prove both clients were registered; give the
	// listener a moment to index them before revoking.
	time.Sleep(100 * time.Millisecond)

	server.CloseUserSessions(userA.String())

	// Target user's connection is closed by the server.
	_ = connA.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if _, err := connA.Read(buf); err == nil {
		t.Fatal("expected revoked user connection to be closed")
	}

	// The other user's connection stays alive (read times out, not closes).
	_ = connB.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := connB.Read(buf); err == nil {
		t.Fatal("unexpected frame for the other user")
	} else if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("other user connection must stay open with a read deadline, got: %v", err)
	}
}
