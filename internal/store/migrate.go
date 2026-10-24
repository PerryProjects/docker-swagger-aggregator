package store

import (
	"context"
	"fmt"
	"io/fs"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/sqlite"
)

// migrate applies the embedded Atlas migrations that are still pending, in process (no Atlas CLI needed).
// The directory is validated against atlas.sum first, so an edited or reordered migration is refused.
func (s *Store) migrate(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	dir, err := embeddedDir()
	if err != nil {
		return err
	}
	drv, err := sqlite.Open(sqlDB)
	if err != nil {
		return fmt.Errorf("atlas driver: %w", err)
	}
	rrw, err := newRevisions(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("revision table: %w", err)
	}
	ex, err := migrate.NewExecutor(drv, dir, rrw)
	if err != nil {
		return err
	}
	if err = ex.ExecuteN(ctx, 0); err != nil && err != migrate.ErrNoPendingFiles { //nolint:errorlint // sentinel returned as is
		return err
	}
	return nil
}

// embeddedDir loads migrations/ (SQL files and atlas.sum) into an in-memory Atlas directory.
func embeddedDir() (*migrate.MemDir, error) {
	dir := &migrate.MemDir{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, readErr := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if readErr != nil {
			return nil, readErr
		}
		if err = dir.WriteFile(e.Name(), b); err != nil {
			return nil, err
		}
	}
	return dir, nil
}
