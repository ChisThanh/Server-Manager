// Package db is the app's local SQLite database (metrics history, audit log,
// event timeline, alert state, run history and module configuration).
// It lives next to servers.json in the user config dir; nothing is stored on
// the managed servers.
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

var migrations = []string{
	// 1
	`CREATE TABLE IF NOT EXISTS kv (
		ns    TEXT NOT NULL,
		key   TEXT NOT NULL,
		value TEXT NOT NULL,
		updated INTEGER NOT NULL,
		PRIMARY KEY (ns, key)
	);
	CREATE TABLE IF NOT EXISTS metrics (
		server TEXT NOT NULL,
		res    INTEGER NOT NULL,
		ts     INTEGER NOT NULL,
		data   TEXT NOT NULL,
		PRIMARY KEY (server, res, ts)
	) WITHOUT ROWID;
	CREATE TABLE IF NOT EXISTS events (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		ts       INTEGER NOT NULL,
		server   TEXT NOT NULL,
		kind     TEXT NOT NULL,
		severity TEXT NOT NULL,
		code     TEXT NOT NULL,
		params   TEXT NOT NULL DEFAULT '{}',
		detail   TEXT NOT NULL DEFAULT '',
		actor    TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS events_server_ts ON events(server, ts);
	CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
	CREATE TABLE IF NOT EXISTS audit (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		ts          INTEGER NOT NULL,
		actor       TEXT NOT NULL,
		server      TEXT NOT NULL,
		server_name TEXT NOT NULL,
		action      TEXT NOT NULL,
		target      TEXT NOT NULL,
		detail      TEXT NOT NULL,
		ok          INTEGER NOT NULL,
		error       TEXT NOT NULL,
		prev_hash   TEXT NOT NULL,
		hash        TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS audit_ts ON audit(ts);
	CREATE INDEX IF NOT EXISTS audit_server ON audit(server, ts);
	CREATE TABLE IF NOT EXISTS alerts (
		key           TEXT PRIMARY KEY,
		rule          TEXT NOT NULL,
		server        TEXT NOT NULL,
		target        TEXT NOT NULL,
		state         TEXT NOT NULL,
		severity      TEXT NOT NULL,
		value         REAL NOT NULL,
		message       TEXT NOT NULL,
		since         INTEGER NOT NULL,
		fired         INTEGER NOT NULL,
		resolved      INTEGER NOT NULL,
		acked         INTEGER NOT NULL,
		acked_by      TEXT NOT NULL,
		last_notified INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS runs (
		id       TEXT PRIMARY KEY,
		kind     TEXT NOT NULL,
		server   TEXT NOT NULL,
		ref      TEXT NOT NULL,
		started  INTEGER NOT NULL,
		finished INTEGER NOT NULL,
		status   TEXT NOT NULL,
		actor    TEXT NOT NULL,
		version  TEXT NOT NULL,
		data     TEXT NOT NULL,
		log      TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS runs_kind_ref ON runs(kind, ref, started);
	CREATE INDEX IF NOT EXISTS runs_server ON runs(server, started);`,
}

// Open opens (creating/migrating) the database in dir.
func Open(dir string) (*DB, error) {
	path := filepath.Join(dir, "data.db")
	sdb, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY storms.
	sdb.SetMaxOpenConns(1)
	d := &DB{sdb}
	if err := d.migrate(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return d, nil
}

// OpenMemory opens a throwaway in-memory database (tests).
func OpenMemory() (*DB, error) {
	// A private in-memory DB lives as long as its single connection.
	sdb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1)
	sdb.SetMaxIdleConns(1)
	sdb.SetConnMaxLifetime(0)
	d := &DB{sdb}
	return d, d.migrate()
}

func (d *DB) migrate() error {
	var v int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ErrNotFound is returned by Get when the key does not exist.
var ErrNotFound = errors.New("not found")

// Put stores v (JSON-encoded) under ns/key.
func (d *DB) Put(ns, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO kv(ns, key, value, updated) VALUES(?,?,?,?)
		ON CONFLICT(ns, key) DO UPDATE SET value=excluded.value, updated=excluded.updated`,
		ns, key, string(b), time.Now().Unix())
	return err
}

// Get decodes ns/key into v; returns ErrNotFound if absent.
func (d *DB) Get(ns, key string, v any) error {
	var s string
	err := d.QueryRow(`SELECT value FROM kv WHERE ns=? AND key=?`, ns, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(s), v)
}

func (d *DB) Delete(ns, key string) error {
	_, err := d.Exec(`DELETE FROM kv WHERE ns=? AND key=?`, ns, key)
	return err
}

// List decodes every value in ns, in key order.
func List[T any](d *DB, ns string) ([]T, error) {
	rows, err := d.Query(`SELECT value FROM kv WHERE ns=? ORDER BY key`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			continue // skip records written by an incompatible version
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Tx runs fn in a transaction.
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
