package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
)

type migrationFixture struct {
	data        []byte
	foreignKeys int
}

var migrationFixtures = struct {
	sync.Mutex
	byPragmas map[string]map[int64]migrationFixture
}{byPragmas: make(map[string]map[int64]migrationFixture)}

// openMigrationFixture reuses only the empty history before a test's upgrade.
// Each clone has its own file; seeded upgrades, repairs, and rollbacks still run
// real migrations. Fresh-install tests continue to call migrate or Open directly.
func openMigrationFixture(t *testing.T, version int64, uriPragmas string) *sql.DB {
	t.Helper()
	fixture := func() migrationFixture {
		migrationFixtures.Lock()
		defer migrationFixtures.Unlock()
		return migrationFixtureAt(t, version, uriPragmas)
	}()
	path := filepath.Join(t.TempDir(), "ao.db")
	if err := os.WriteFile(path, fixture.data, 0o600); err != nil {
		t.Fatalf("clone migration fixture: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+uriPragmas)
	if err != nil {
		t.Fatalf("open migration fixture: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	// Some historical migrations change this connection-local pragma.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA foreign_keys = %d", fixture.foreignKeys)); err != nil {
		t.Fatalf("restore fixture foreign_keys: %v", err)
	}
	return db
}

// The caller holds migrationFixtures.Mutex; cache every intermediate version so
// a later test starting earlier does not replay the same empty migration history.
func migrationFixtureAt(t *testing.T, version int64, uriPragmas string) migrationFixture {
	t.Helper()
	fixtures := migrationFixtures.byPragmas[uriPragmas]
	if fixtures == nil {
		fixtures = make(map[int64]migrationFixture)
		migrationFixtures.byPragmas[uriPragmas] = fixtures
	}
	if fixture, ok := fixtures[version]; ok {
		return fixture
	}
	var start int64
	for cached := range fixtures {
		if cached < version && cached > start {
			start = cached
		}
	}
	path := filepath.Join(t.TempDir(), "template.db")
	if start != 0 {
		if err := os.WriteFile(path, fixtures[start].data, 0o600); err != nil {
			t.Fatalf("seed migration fixture: %v", err)
		}
	}
	db, err := sql.Open("sqlite", "file:"+path+uriPragmas)
	if err != nil {
		t.Fatalf("open fixture builder: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if start != 0 {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA foreign_keys = %d", fixtures[start].foreignKeys)); err != nil {
			t.Fatalf("restore builder foreign_keys: %v", err)
		}
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read fixture migrations: %v", err)
	}
	for _, entry := range entries {
		next, err := goose.NumericComponent(entry.Name())
		if err != nil {
			t.Fatalf("parse fixture migration: %v", err)
		}
		if next <= start || next > version {
			continue
		}
		upTo(t, db, next)
		// Flush the WAL before reading the main file; never clone a partial DB.
		var busy, pages, checkpointed int
		if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &checkpointed); err != nil || busy != 0 {
			t.Fatalf("checkpoint migration fixture: busy=%d err=%v", busy, err)
		}
		var fixture migrationFixture
		if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fixture.foreignKeys); err != nil {
			t.Fatalf("read fixture foreign_keys: %v", err)
		}
		fixture.data, err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration fixture: %v", err)
		}
		fixtures[next] = fixture
	}
	fixture, ok := fixtures[version]
	if !ok {
		t.Fatalf("no migration fixture for version %d", version)
	}
	return fixture
}

func TestMigrationFixturesPreserveHistoricalVersionAndIsolation(t *testing.T) {
	first := openMigrationFixture(t, 12, pragmas)
	seedProjectRow(t, first, "fixture-marker")
	upTo(t, first, 13)

	// Advancing one clone and extending the cache must leave older clones empty
	// and at their requested version, including data flushed through the WAL.
	_ = openMigrationFixture(t, 43, pragmas)
	second := openMigrationFixture(t, 12, pragmas)
	var version, count, foreignKeys int
	if err := second.QueryRow("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 12 {
		t.Fatalf("historical version = %d, want 12", version)
	}
	if err := second.QueryRow("SELECT COUNT(*) FROM projects").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fixture inherited %d project rows", count)
	}
	if err := second.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatal("fixture lost foreign-key enforcement")
	}
}
