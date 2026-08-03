package database

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

func Connect() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "./database/files.db?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	// One writer at a time keeps sqlite out of "database is locked" territory;
	// the worker and the HTTP handlers both write.
	db.SetMaxOpenConns(1)
	return db, nil
}

func CreateTables(db *sql.DB) error {
	stmts := []string{
		// files is the registry of everything on disk: both the uploaded source
		// and the compressed output get a row. path is emptied when the file is
		// removed from disk but the row is still worth keeping for its name.
		`CREATE TABLE IF NOT EXISTS files (
			id TEXT NOT NULL PRIMARY KEY,
			filename TEXT NOT NULL,
			size INTEGER CHECK (size IS NULL OR size >= 0),
			path TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'source',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT NOT NULL PRIMARY KEY,
			file_id TEXT NOT NULL,
			output_file_id TEXT,
			status TEXT NOT NULL DEFAULT 'queued',
			error TEXT,
			preset TEXT,
			encoder TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (file_id) REFERENCES files(id),
			FOREIGN KEY (output_file_id) REFERENCES files(id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs(created_at DESC);`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	// CREATE TABLE IF NOT EXISTS is a no-op on databases created by an earlier
	// version, so columns added since then have to be applied separately.
	if err := addColumns(db, "files", map[string]string{
		"path": "TEXT NOT NULL DEFAULT ''",
		"kind": "TEXT NOT NULL DEFAULT 'source'",
	}); err != nil {
		return err
	}
	if err := addColumns(db, "jobs", map[string]string{
		"preset":         "TEXT",
		"encoder":        "TEXT",
		"output_file_id": "TEXT",
	}); err != nil {
		return err
	}

	// files.path supersedes jobs.output_path. Dropping it needs sqlite 3.35+;
	// on an older build the column simply stays behind unused, which is
	// harmless, so a failure here is logged rather than fatal.
	if err := dropColumn(db, "jobs", "output_path"); err != nil {
		log.Printf("could not drop obsolete column jobs.output_path: %v", err)
	}
	return nil
}

// addColumns adds any of the given columns the table does not already have.
func addColumns(db *sql.DB, table string, columns map[string]string) error {
	existing, err := tableColumns(db, table)
	if err != nil {
		return err
	}

	for name, typ := range columns {
		if existing[name] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, typ)); err != nil {
			return fmt.Errorf("adding column %s.%s: %w", table, name, err)
		}
	}
	return nil
}

// dropColumn removes a column that is no longer used. It is a no-op when the
// column is already gone.
func dropColumn(db *sql.DB, table, column string) error {
	existing, err := tableColumns(db, table)
	if err != nil {
		return err
	}
	if !existing[column] {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", table, column)); err != nil {
		return fmt.Errorf("dropping column %s.%s: %w", table, column, err)
	}
	return nil
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}
