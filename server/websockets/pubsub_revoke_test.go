package websockets

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/buraksezer/olric"
	olricConfig "github.com/buraksezer/olric/config"
	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/resource"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

func freePubSubPort(t *testing.T) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func startRevokePubSub(t *testing.T) *olric.PubSub {
	t.Helper()
	port := freePubSubPort(t)
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
		t.Fatal(err)
	}
	go func() { _ = emb.Start() }()
	<-started
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = emb.Shutdown(ctx)
	})
	pubsub, err := emb.NewEmbeddedClient().NewPubSub()
	if err != nil {
		t.Fatalf("new pubsub: %v", err)
	}
	return pubsub
}

func TestSessionRevocationThroughPubSub(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pubsub := startRevokePubSub(t)
	server := NewServer("/live", nil, nil, pubsub)
	router := gin.New()
	userRef := daptinid.DaptinReferenceId(uuid.New())
	router.Use(func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), "user", &auth.SessionUser{UserId: 1, UserReferenceId: userRef})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	server.Register(router)
	go server.Listen()

	httpServer := httptest.NewServer(router)
	defer httpServer.Close()

	parsed, _ := url.Parse(httpServer.URL)
	wsURL := "ws://" + parsed.Host + "/live"
	config, err := websocket.NewConfig(wsURL, "http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("session frame: %v", err)
	}

	server.WatchSessionRevocations(context.Background())
	time.Sleep(100 * time.Millisecond)
	if _, err := pubsub.Publish(context.Background(), resource.UserSessionRevokeTopic, userRef.String()); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected pubsub revocation to close the websocket connection")
	}
}
