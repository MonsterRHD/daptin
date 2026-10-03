package resource

import (
	"path/filepath"
	"sync"
	"testing"

	daptinid "github.com/daptin/daptin/server/id"
	"github.com/daptin/daptin/server/table_info"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

func insertModSeqTestMailbox(t *testing.T, db *sqlx.DB, id int64, highestmodseq int64) {
	t.Helper()
	ref := daptinid.DaptinReferenceId(uuid.New())
	_, err := db.Exec(`insert into mail_box
		(id, name, reference_id, permission, uidvalidity, nextuid, attributes, flags, permanent_flags, subscribed, highestmodseq)
		values (?, 'INBOX', ?, 16256, 1, 1, '', '\*', '\*', true, ?)`,
		id, ref[:], highestmodseq)
	if err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
}

func newMailModSeqTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	table := standardTableByName(t, "mail_box")
	if err := CheckTable(&table, db); err != nil {
		t.Fatalf("create mail_box: %v", err)
	}
	insertModSeqTestMailbox(t, db, 1, 0)
	return db
}

func TestAllocateMailBoxModSeqAdvancesPersistedValue(t *testing.T) {
	db := newMailModSeqTestDB(t)
	crud := &DbResource{db: db}

	for want := int64(1); want <= 3; want++ {
		tx := db.MustBegin()
		got, err := crud.AllocateMailBoxModSeq(1, tx)
		if err != nil {
			t.Fatalf("allocate %d: %v", want, err)
		}
		if got != want {
			t.Fatalf("modseq = %d, want %d", got, want)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		stored, err := crud.GetMailBoxHighestModSeq(1, nil)
		if err != nil {
			t.Fatalf("read highestmodseq: %v", err)
		}
		if stored != want {
			t.Fatalf("stored highestmodseq = %d, want %d", stored, want)
		}
	}
}

func TestAllocateMailBoxModSeqRequiresTransaction(t *testing.T) {
	crud := &DbResource{}
	if _, err := crud.AllocateMailBoxModSeq(1, nil); err == nil {
		t.Fatal("expected error when transaction is nil")
	}
}

func TestAllocateMailBoxModSeqConcurrentAssignmentsAreUniqueAndDense(t *testing.T) {
	t.Helper()

	dsn := "file:modseq_concurrency?mode=memory&cache=shared&_busy_timeout=10000"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open shared sqlite: %v", err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })

	table := standardTableByName(t, "mail_box")
	if err := CheckTable(&table, db); err != nil {
		t.Fatalf("create mail_box: %v", err)
	}
	insertModSeqTestMailbox(t, db, 1, 0)

	crud := &DbResource{db: db}
	const goroutines = 8
	const perGoroutine = 10

	var wg sync.WaitGroup
	results := make(chan int64, goroutines*perGoroutine)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perGoroutine; i++ {
				// Retry on busy: the write lock is held by another transaction.
				var value int64
				for attempt := 0; attempt < 50; attempt++ {
					tx, err := db.Beginx()
					if err != nil {
						continue
					}
					value, err = crud.AllocateMailBoxModSeq(1, tx)
					if err != nil {
						_ = tx.Rollback()
						if attempt == 49 {
							t.Errorf("allocate failed after retries: %v", err)
						}
						continue
					}
					if err := tx.Commit(); err != nil {
						continue
					}
					break
				}
				results <- value
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	seen := make(map[int64]bool, goroutines*perGoroutine)
	for value := range results {
		if seen[value] {
			t.Fatalf("duplicate modseq allocation: %d", value)
		}
		seen[value] = true
	}
	total := goroutines * perGoroutine
	if len(seen) != total {
		t.Fatalf("got %d unique values, want %d", len(seen), total)
	}
	for v := int64(1); v <= int64(total); v++ {
		if !seen[v] {
			t.Fatalf("missing modseq %d (non-dense allocation)", v)
		}
	}
}

func TestAllocateMailBoxModSeqSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "modseq.db")

	db, err := sqlx.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite file: %v", err)
	}
	table := table_info.TableInfo(standardTableByName(t, "mail_box"))
	if err := CheckTable(&table, db); err != nil {
		t.Fatalf("create mail_box: %v", err)
	}
	insertModSeqTestMailbox(t, db, 1, 0)
	crud := &DbResource{db: db}
	tx := db.MustBegin()
	before, err := crud.AllocateMailBoxModSeq(1, tx)
	if err != nil {
		t.Fatalf("allocate before restart: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := sqlx.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite file: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	crud2 := &DbResource{db: db2}
	tx2 := db2.MustBegin()
	after, err := crud2.AllocateMailBoxModSeq(1, tx2)
	if err != nil {
		t.Fatalf("allocate after restart: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if after <= before {
		t.Fatalf("post-restart modseq %d must exceed pre-restart %d", after, before)
	}
}

func TestDeletionHistoryWatermarkInitialization(t *testing.T) {
	db := newMailModSeqTestDB(t)
	crud := &DbResource{db: db}

	tx := db.MustBegin()
	if _, initialized, err := crud.GetDeletionHistoryWatermark(1, tx); err != nil {
		t.Fatalf("read watermark: %v", err)
	} else if initialized {
		t.Fatal("watermark must start uninitialized")
	}

	// Advance highestmodseq to 3 first; the watermark must record the
	// pre-initialization boundary (3), not 0.
	for i := 0; i < 3; i++ {
		if _, err := crud.AllocateMailBoxModSeq(1, tx); err != nil {
			t.Fatalf("advance modseq: %v", err)
		}
	}

	w, err := crud.InitDeletionHistoryWatermark(1, tx)
	if err != nil {
		t.Fatalf("init watermark: %v", err)
	}
	if w != 3 {
		t.Fatalf("watermark = %d, want 3", w)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Idempotent: another init in a new tx returns the same watermark.
	tx2 := db.MustBegin()
	w2, err := crud.InitDeletionHistoryWatermark(1, tx2)
	if err != nil {
		t.Fatalf("re-init watermark: %v", err)
	}
	if w2 != 3 {
		t.Fatalf("watermark changed to %d, want idempotent 3", w2)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
