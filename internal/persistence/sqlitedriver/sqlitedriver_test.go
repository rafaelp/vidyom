package sqlitedriver

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rafaelp/vidyom/schemas"
)

func open(t *testing.T, name string, opts Options) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), name), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The fork's compile-time defaults are synchronous off and autocheckpoint off, so
// this test is what keeps job state durable (I1). It checks several pooled
// connections because both settings are per connection.
func TestDurableSettingsOnEveryConnection(t *testing.T) {
	db := open(t, "t.db", Options{})
	ctx := context.Background()
	var conns []*sql.Conn
	for range 4 {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		var journal string
		var synchronous, autocheckpoint, foreignKeys, busy int
		queries := []struct {
			q    string
			dest any
		}{
			{"PRAGMA journal_mode", &journal},
			{"PRAGMA synchronous", &synchronous},
			{"PRAGMA wal_autocheckpoint", &autocheckpoint},
			{"PRAGMA foreign_keys", &foreignKeys},
			{"PRAGMA busy_timeout", &busy},
		}
		for _, q := range queries {
			if err := c.QueryRowContext(ctx, q.q).Scan(q.dest); err != nil {
				t.Fatalf("%s: %v", q.q, err)
			}
		}
		if journal != "wal" {
			t.Errorf("conn %d: journal_mode = %s", i, journal)
		}
		if synchronous != 2 { // FULL
			t.Errorf("conn %d: synchronous = %d, want 2 (FULL)", i, synchronous)
		}
		if autocheckpoint != walAutocheckpointPages {
			t.Errorf("conn %d: wal_autocheckpoint = %d", i, autocheckpoint)
		}
		if foreignKeys != 1 {
			t.Errorf("conn %d: foreign_keys = %d", i, foreignKeys)
		}
		if busy == 0 {
			t.Errorf("conn %d: busy_timeout is off", i)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestFTS5(t *testing.T) {
	db := open(t, "t.db", Options{})
	mustExec(t, db, `CREATE VIRTUAL TABLE docs USING fts5(body, tokenize='unicode61 remove_diacritics 2')`)
	mustExec(t, db, `INSERT INTO docs(body) VALUES ('explanation of Ujjayi breathing'), ('respiração consciente')`)
	var body string
	if err := db.QueryRow(`SELECT body FROM docs WHERE docs MATCH 'ujjayi' ORDER BY bm25(docs)`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "explanation of Ujjayi breathing" {
		t.Fatalf("body = %q", body)
	}
	if err := db.QueryRow(`SELECT body FROM docs WHERE docs MATCH 'respiracao'`).Scan(&body); err != nil {
		t.Fatalf("diacritics folding: %v", err)
	}
}

// Asset-scoped KNN through a partition key is the main search path (D5).
func TestSqliteVec(t *testing.T) {
	db := open(t, "t.db", Options{})
	mustExec(t, db, `CREATE VIRTUAL TABLE vectors USING vec0(asset TEXT PARTITION KEY, embedding FLOAT[4])`)
	insert := func(asset string, rowid int, v []float32) {
		blob, err := SerializeVector(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO vectors(rowid, asset, embedding) VALUES (?, ?, ?)`, rowid, asset, blob); err != nil {
			t.Fatal(err)
		}
	}
	insert("ORI-001", 1, []float32{1, 0, 0, 0})
	insert("ORI-001", 2, []float32{0, 1, 0, 0})
	insert("ORI-002", 3, []float32{1, 0, 0, 0})

	query, _ := SerializeVector([]float32{1, 0, 0, 0})
	rows, err := db.Query(`SELECT rowid FROM vectors WHERE embedding MATCH ? AND k = 5 AND asset = 'ORI-001' ORDER BY distance`, query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("asset-scoped KNN = %v, want [1 2]", got)
	}
}

func TestPublicDBSchemaApplies(t *testing.T) {
	db := open(t, "public.db", Options{})
	schema, err := schemas.PublicDBSchema()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, schema)
	mustExec(t, db, `INSERT INTO videos(public_id, title, kind, published_at) VALUES ('p1', 'A title', 'clip', '2026-11-09T00:00:00Z')`)
	mustExec(t, db, `INSERT INTO search_fts(public_id, source, start_seconds, end_seconds, text) VALUES ('p1', 'title', 0, 0, 'A title')`)
	var id string
	if err := db.QueryRow(`SELECT public_id FROM search_fts WHERE search_fts MATCH 'title'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != "p1" {
		t.Fatalf("id = %q", id)
	}
}

func TestConcurrentWriters(t *testing.T) {
	db := open(t, "t.db", Options{})
	mustExec(t, db, `CREATE TABLE seq (n INTEGER)`)
	mustExec(t, db, `INSERT INTO seq VALUES (0)`)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				// Read-then-write, the shape of friendly ID allocation (R5).
				tx, err := db.Begin()
				if err != nil {
					errs <- err
					return
				}
				var n int
				if err := tx.QueryRow(`SELECT n FROM seq`).Scan(&n); err != nil {
					tx.Rollback()
					errs <- err
					return
				}
				if _, err := tx.Exec(`UPDATE seq SET n = ?`, n+1); err != nil {
					tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer failed: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT n FROM seq`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 160 {
		t.Fatalf("n = %d, want 160: lost updates", n)
	}
}

func TestReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "with space's.db")
	rw, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, rw, `CREATE TABLE t (x)`)
	rw.Close()

	ro, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`INSERT INTO t VALUES (1)`); err == nil {
		t.Fatal("a read-only handle must refuse writes")
	}
	var n int
	if err := ro.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(filepath.Join(t.TempDir(), "missing.db"), Options{ReadOnly: true}); err == nil {
		t.Fatal("read-only open must not create a file")
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
