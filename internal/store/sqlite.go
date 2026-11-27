package store

import (
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps sql.DB with additional functionality
type DB struct {
	*sql.DB
}

// NewDB creates a new SQLite database connection
func NewDB(dbPath string) (*DB, error) {
	// SQLite best practices configuration:
	// - WAL mode: Better concurrency, allows concurrent readers with a single writer
	// - Busy timeout: 30s to handle lock contention
	// - Synchronous NORMAL: Good balance of safety and performance with WAL
	// - Cache size: 64MB (negative value means KB)
	// - Temp store: Use memory for temp tables
	// - Immediate transactions: Acquire write lock immediately to reduce BUSY errors
	//
	// NOTE: modernc.org/sqlite only understands _pragma=name(value) DSN
	// parameters (plus _txlock and _time_format). The mattn/go-sqlite3 style
	// (_journal_mode=WAL&_busy_timeout=...) is silently ignored, which
	// previously left the DB running with journal_mode=delete, busy_timeout=0
	// and synchronous=FULL. _pragma entries are applied to every pooled
	// connection, so they belong here rather than in ad-hoc Exec calls.
	dsn := fmt.Sprintf("%s?_txlock=immediate"+
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=busy_timeout(30000)"+
		"&_pragma=synchronous(NORMAL)"+
		"&_pragma=cache_size(-64000)"+
		// foreign_keys stays OFF on purpose: enforcement has always been
		// silently disabled (the old mattn-style _foreign_keys=ON param never
		// took effect), existing data has orphaned rows (e.g. tokens whose
		// users were deleted without cascade), and several write paths and
		// test fixtures insert children before parents. Enabling it is a
		// separate migration: clean orphans + fix fixtures first.
		"&_pragma=temp_store(MEMORY)"+
		"&_pragma=mmap_size(268435456)", dbPath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// WAL allows concurrent readers alongside a single writer; a small pool
	// keeps API reads from queueing behind background workers' writes.
	// Write contention between connections is handled by busy_timeout.
	// Exception: each :memory: connection is its own separate database, so
	// in-memory databases (used in tests) must stay on a single connection.
	poolSize := 4
	if strings.Contains(dbPath, ":memory:") {
		poolSize = 1
	}
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	db.SetConnMaxLifetime(0) // Don't close connections

	// Test connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// One-time (database-level) PRAGMAs; per-connection ones live in the DSN
	pragmas := []string{
		"PRAGMA wal_autocheckpoint=1000",  // Checkpoint every 1000 pages (~4MB)
		"PRAGMA wal_checkpoint(TRUNCATE)", // Initial checkpoint to reset WAL
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			// Log but don't fail on pragma errors
			// Some pragmas might not be applicable after DB is created
			continue
		}
	}

	return &DB{DB: db}, nil
}

// Migrate runs all pending migrations
func (db *DB) Migrate() error {
	// Create migrations table if not exists
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	// Read migration files from embedded FS
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("failed to read migrations directory: %w", err)
	}

	// Sort by filename
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version := strings.TrimSuffix(entry.Name(), ".sql")
		
		// Check if already applied
		var applied bool
		err := db.QueryRow("SELECT 1 FROM schema_migrations WHERE version = ?", version).Scan(&applied)
		if err == nil {
			continue // Already applied
		}

		// Read and execute migration
		content, err := migrationsFS.ReadFile(filepath.Join("migrations", entry.Name()))
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", entry.Name(), err)
		}

		// Execute migration in a transaction
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %w", err)
		}

		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to execute migration %s: %w", entry.Name(), err)
		}

		// Record migration
		if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to record migration %s: %w", entry.Name(), err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration %s: %w", entry.Name(), err)
		}
	}

	return nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.DB.Close()
}

// InitStores initializes all stores with the database
func InitStores(db *DB) (*BuildStore, *DeploymentStore, *RepoStore, *AppStore, *MemoryStore, *UserStore, *TokenStore, *WebhookStore, error) {
	buildStore := NewBuildStore(db)
	deploymentStore := NewDeploymentStore(db)
	repoStore := NewRepoStore(db)
	appStore := NewAppStore(db)
	memoryStore := NewMemoryStore(db)
	userStore := NewUserStore(db)
	tokenStore := NewTokenStore(db)
	webhookStore := NewWebhookStore(db)

	return buildStore, deploymentStore, repoStore, appStore, memoryStore, userStore, tokenStore, webhookStore, nil
}

