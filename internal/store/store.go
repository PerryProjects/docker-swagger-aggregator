package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

//go:embed migrations/*.sql migrations/atlas.sum
var migrationsFS embed.FS

// FileName is the database file inside the data directory.
const FileName = "swagger-aggregator.db"

// Store wraps the database.
type Store struct {
	db *gorm.DB
}

// Open opens (creating it if needed) the SQLite database in dir and applies pending migrations.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	return openDSN(ctx, dsn(filepath.Join(dir, FileName)))
}

// dsn enables WAL, foreign keys and a busy timeout for every connection.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	return (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(path), RawQuery: q.Encode()}).String()
}

func openDSN(ctx context.Context, dsn string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Store{db: db}
	if err = s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return s, nil
}

// OpenMemory opens a private in-memory database (tests).
func OpenMemory(ctx context.Context) (*Store, error) {
	s, err := openDSN(ctx, "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// every new connection to ":memory:" would be a new empty database
	sqlDB, err := s.db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return errors.Join(sqlDB.Close())
}
