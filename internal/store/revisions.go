package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ariga.io/atlas/sql/migrate"
)

const revisionsTable = "atlas_schema_revisions" // the name the Atlas CLI uses, so both can manage one database

// revisions keeps Atlas' migration history in the database (migrate.RevisionReadWriter).
type revisions struct{ db *sql.DB }

func newRevisions(ctx context.Context, db *sql.DB) (*revisions, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+revisionsTable+` (
		version TEXT NOT NULL PRIMARY KEY,
		description TEXT NOT NULL,
		type INTEGER NOT NULL DEFAULT 2,
		applied INTEGER NOT NULL DEFAULT 0,
		total INTEGER NOT NULL DEFAULT 0,
		executed_at DATETIME NOT NULL,
		execution_time INTEGER NOT NULL,
		error TEXT NULL,
		error_stmt TEXT NULL,
		hash TEXT NOT NULL,
		partial_hashes JSON NULL,
		operator_version TEXT NOT NULL
	)`)
	return &revisions{db: db}, err
}

func (r *revisions) Ident() *migrate.TableIdent { return &migrate.TableIdent{Name: revisionsTable} }

const revisionColumns = `version, description, type, applied, total, executed_at, execution_time, error, error_stmt, hash, partial_hashes, operator_version`

func (r *revisions) ReadRevisions(ctx context.Context) ([]*migrate.Revision, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+revisionColumns+` FROM `+revisionsTable+` ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*migrate.Revision
	for rows.Next() {
		rev, scanErr := scanRevision(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

func (r *revisions) ReadRevision(ctx context.Context, version string) (*migrate.Revision, error) {
	rev, err := scanRevision(
		r.db.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM `+revisionsTable+` WHERE version = ?`, version),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, migrate.ErrRevisionNotExist
	}
	return rev, err
}

func (r *revisions) WriteRevision(ctx context.Context, rev *migrate.Revision) error {
	partial, err := json.Marshal(rev.PartialHashes)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(
		ctx,
		`INSERT OR REPLACE INTO `+revisionsTable+` (`+revisionColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		rev.Version,
		rev.Description,
		int(rev.Type),
		rev.Applied,
		rev.Total,
		rev.ExecutedAt.UTC().Format(time.RFC3339Nano),
		int64(
			rev.ExecutionTime,
		),
		nullable(rev.Error),
		nullable(rev.ErrorStmt),
		rev.Hash,
		string(partial),
		rev.OperatorVersion,
	)
	return err
}

func (r *revisions) DeleteRevision(ctx context.Context, version string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM `+revisionsTable+` WHERE version = ?`, version)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanRevision(s scanner) (*migrate.Revision, error) {
	var (
		rev                      migrate.Revision
		typ                      int
		executedAt               string
		execTime                 int64
		errMsg, errStmt, partial sql.NullString
	)
	if err := s.Scan(&rev.Version, &rev.Description, &typ, &rev.Applied, &rev.Total, &executedAt, &execTime,
		&errMsg, &errStmt, &rev.Hash, &partial, &rev.OperatorVersion); err != nil {
		return nil, err
	}
	rev.Type = migrate.RevisionType(typ)
	rev.ExecutionTime = time.Duration(execTime)
	rev.Error, rev.ErrorStmt = errMsg.String, errStmt.String
	t, err := time.Parse(time.RFC3339Nano, executedAt)
	if err != nil {
		return nil, fmt.Errorf("revision %s: bad executed_at %q: %w", rev.Version, executedAt, err)
	}
	rev.ExecutedAt = t
	if partial.Valid && partial.String != "" {
		if err = json.Unmarshal([]byte(partial.String), &rev.PartialHashes); err != nil {
			return nil, fmt.Errorf("revision %s: bad partial_hashes: %w", rev.Version, err)
		}
	}
	return &rev, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
