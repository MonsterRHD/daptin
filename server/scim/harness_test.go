package scim

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/artpar/api2go/v2"
	"github.com/buraksezer/olric"
	olricConfig "github.com/buraksezer/olric/config"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/daptin/daptin/server/table_info"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

const testBaseURL = "http://daptin.test/scim/v2"

type scimHarness struct {
	db    *sqlx.DB
	store *Store
	cruds map[string]*resource.DbResource
}

// startHarnessOlric starts an isolated embedded olric for one fixture.
func startHarnessOlric(t *testing.T) *olric.EmbeddedClient {
	port, err := freeTCPPort()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cfg := olricConfig.New("local")
	cfg.BindAddr = "127.0.0.1"
	cfg.BindPort = port
	cfg.MemberlistConfig.BindAddr = "127.0.0.1"
	cfg.MemberlistConfig.BindPort = 0
	cfg.MemberlistConfig.Name = net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.BindPort))
	cfg.Started = func() { close(started) }
	emb, err := olric.New(cfg)
	if err != nil {
		t.Fatalf("olric: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- emb.Start() }()
	select {
	case <-started:
	case err := <-errCh:
		t.Fatalf("olric start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("olric start timeout")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = emb.Shutdown(ctx)
	})
	return emb.NewEmbeddedClient()
}

func freeTCPPort() (int, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func newScimHarness(t *testing.T) *scimHarness {
	t.Helper()
	database, err := sqlx.Open("sqlite3",
		fmt.Sprintf("file:scim-store-%s?mode=memory&cache=shared&_busy_timeout=10000", uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })

	statementbuilder.InitialiseStatementBuilder("sqlite3")
	tables := make([]table_info.TableInfo, 0)
	for _, table := range resource.StandardTables {
		switch table.TableName {
		case resource.USER_ACCOUNT_TABLE_NAME, "usergroup", "world",
			"oauth_access", "oauth_refresh", "oauth_app":
		default:
			continue
		}
		copyOfTable := table
		copyOfTable.Columns = append([]api2go.ColumnInfo(nil), table.Columns...)
		copyOfTable.Relations = append([]api2go.TableRelation(nil), table.Relations...)
		tables = append(tables, copyOfTable)
	}
	config := resource.CmsConfig{Tables: tables}
	resource.CheckRelations(&config)
	resource.CheckAllTableStatus(&config, database)
	constraintTx, err := database.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	resource.CreateUniqueConstraints(&config, constraintTx)
	if err := constraintTx.Commit(); err != nil {
		t.Fatal(err)
	}
	resource.CreateIndexes(&config, database)

	bootstrap, err := database.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if err := resource.UpdateWorldTable(&config, bootstrap); err != nil {
		_ = bootstrap.Rollback()
		t.Fatalf("bootstrap world data: %v", err)
	}
	if err := bootstrap.Commit(); err != nil {
		t.Fatal(err)
	}

	resource.ResetCachesForTests()
	olricClient := startHarnessOlric(t)
	cruds := make(map[string]*resource.DbResource)
	previous := make(map[string]*resource.DbResource)
	for index := range config.Tables {
		table := config.Tables[index]
		previous[table.TableName] = resource.CRUD_MAP[table.TableName]
		model := api2go.NewApi2GoModel(table.TableName, table.Columns,
			int64(table.DefaultPermission), table.Relations)
		crud, createErr := resource.NewDbResource(model, database, &resource.MiddlewareSet{},
			cruds, nil, olricClient, table)
		if createErr != nil {
			t.Fatalf("new resource %s: %v", table.TableName, createErr)
		}
		cruds[table.TableName] = crud
	}
	t.Cleanup(func() {
		for name, crud := range previous {
			if crud == nil {
				delete(resource.CRUD_MAP, name)
			} else {
				resource.CRUD_MAP[name] = crud
			}
		}
	})

	store, err := NewStore(cruds)
	if err != nil {
		t.Fatal(err)
	}
	return &scimHarness{db: database, store: store, cruds: cruds}
}

func internalGetRequest(path string) *http.Request {
	parsed, _ := url.Parse(path)
	return (&http.Request{Method: http.MethodGet, URL: parsed}).WithContext(context.Background())
}
