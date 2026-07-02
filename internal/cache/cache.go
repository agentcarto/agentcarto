package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agentcarto/core/domain"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type DB struct {
	db   *sql.DB
	path string
}

func Path() string {
	h, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(h, "Library", "Caches", "agentcarto", "cache.db")
	case "windows":
		return filepath.Join(os.Getenv("LocalAppData"), "agentcarto", "cache.db")
	default:
		b := os.Getenv("XDG_CACHE_HOME")
		if b == "" {
			b = filepath.Join(h, ".cache")
		}
		return filepath.Join(b, "agentcarto", "cache.db")
	}
}
func Open(path string) (*DB, error) {
	if path == "" {
		path = Path()
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	d, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	// A single connection: connection-scoped PRAGMAs (busy_timeout) would
	// otherwise apply to only one connection of the pool, and SQLite serializes
	// writers anyway, so a pool buys nothing here.
	d.SetMaxOpenConns(1)
	// Configure the connection (WAL + a short busy timeout) and create the schema:
	// sessions holds one JSON-encoded session per (plugin_id, session_id); artifacts caches
	// derived data (e.g. parsed conversations) per (plugin_id, session_id, kind), keyed also
	// by the session fingerprint and parser_version so stale entries are ignored on read.
	for _, q := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=1000",
		"CREATE TABLE IF NOT EXISTS sessions (plugin_id TEXT, session_id TEXT, data BLOB NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY(plugin_id,session_id))",
		"CREATE TABLE IF NOT EXISTS artifacts (plugin_id TEXT, session_id TEXT, fingerprint TEXT, parser_version TEXT, kind TEXT, data BLOB NOT NULL, accessed INTEGER NOT NULL, PRIMARY KEY(plugin_id,session_id,kind))",
	} {
		if _, e = d.Exec(q); e != nil {
			d.Close()
			return nil, e
		}
	}
	// Enforce reclaims space with incremental_vacuum, which is a no-op unless
	// auto_vacuum=incremental. The setting only takes effect on a fresh database
	// or after a full VACUUM, so migrate existing files once here.
	var av int
	if e := d.QueryRow("PRAGMA auto_vacuum").Scan(&av); e == nil && av != 2 {
		if _, e = d.Exec("PRAGMA auto_vacuum=incremental"); e == nil {
			_, e = d.Exec("VACUUM")
		}
		if e != nil {
			d.Close()
			return nil, e
		}
	}
	_ = os.Chmod(path, 0600)
	return &DB{d, path}, nil
}
func (d *DB) GetArtifact(ctx context.Context, s domain.Session, kind string, dst any) bool {
	var b []byte
	e := d.db.QueryRowContext(ctx, "SELECT data FROM artifacts WHERE plugin_id=? AND session_id=? AND fingerprint=? AND parser_version=? AND kind=?", s.PluginID, s.SessionID, s.Fingerprint, s.ParserVersion, kind).Scan(&b)
	if e != nil {
		return false
	}
	_, _ = d.db.ExecContext(ctx, "UPDATE artifacts SET accessed=? WHERE plugin_id=? AND session_id=? AND kind=?", time.Now().Unix(), s.PluginID, s.SessionID, kind)
	return json.Unmarshal(b, dst) == nil
}
func (d *DB) PutArtifact(ctx context.Context, s domain.Session, kind string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = d.db.ExecContext(ctx, "INSERT INTO artifacts VALUES(?,?,?,?,?,?,?) ON CONFLICT(plugin_id,session_id,kind) DO UPDATE SET fingerprint=excluded.fingerprint,parser_version=excluded.parser_version,data=excluded.data,accessed=excluded.accessed", s.PluginID, s.SessionID, s.Fingerprint, s.ParserVersion, kind, b, time.Now().Unix())
	return e
}
func (d *DB) Close() error { return d.db.Close() }
func (d *DB) Load(ctx context.Context) ([]domain.Session, error) {
	rows, e := d.db.QueryContext(ctx, "SELECT data FROM sessions ORDER BY seen DESC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []domain.Session
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			continue
		}
		var s domain.Session
		if json.Unmarshal(b, &s) == nil {
			s.Status = ""
			s.PermissionWait = false
			out = append(out, s)
		}
	}
	return out, rows.Err()
}
func (d *DB) Save(ctx context.Context, s []domain.Session) error {
	tx, e := d.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, x := range s {
		b, _ := json.Marshal(x)
		if _, e = tx.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?) ON CONFLICT(plugin_id,session_id) DO UPDATE SET data=excluded.data,seen=excluded.seen", x.PluginID, x.SessionID, b, now); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (d *DB) Prune(ctx context.Context, sessions []domain.Session, successful map[string]bool, maxAge time.Duration) error {
	seen := map[domain.SessionKey]bool{}
	for _, s := range sessions {
		seen[s.Key()] = true
	}
	rows, e := d.db.QueryContext(ctx, "SELECT plugin_id,session_id,seen FROM sessions")
	if e != nil {
		return e
	}
	defer rows.Close()
	type key struct{ p, s string }
	var del []key
	cut := time.Now().Add(-maxAge).Unix()
	for rows.Next() {
		var k key
		var at int64
		if rows.Scan(&k.p, &k.s, &at) != nil {
			continue
		}
		if successful[k.p] && !seen[domain.SessionKey{PluginID: k.p, SessionID: k.s}] && at < cut {
			del = append(del, k)
		}
	}
	for _, k := range del {
		if _, e = d.db.ExecContext(ctx, "DELETE FROM sessions WHERE plugin_id=? AND session_id=?", k.p, k.s); e != nil {
			return e
		}
		_, _ = d.db.ExecContext(ctx, "DELETE FROM artifacts WHERE plugin_id=? AND session_id=?", k.p, k.s)
	}
	return rows.Err()
}

// sizeOnDisk reports the cache's real footprint: the main file plus the WAL
// (which can hold most of the data between checkpoints).
func (d *DB) sizeOnDisk() (int64, error) {
	st, e := os.Stat(d.path)
	if e != nil {
		return 0, e
	}
	size := st.Size()
	if wst, e := os.Stat(d.path + "-wal"); e == nil {
		size += wst.Size()
	}
	return size, nil
}
func (d *DB) Stats(ctx context.Context) (int, int64, error) {
	var n int
	if e := d.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions").Scan(&n); e != nil {
		return 0, 0, e
	}
	size, e := d.sizeOnDisk()
	return n, size, e
}

// Enforce evicts least-recently-accessed artifacts until the on-disk size
// drops below max. Deletions alone never shrink a SQLite file, so each round
// releases the freed pages (incremental_vacuum, enabled in Open) and truncates
// the WAL; without that the loop degenerated into wiping the whole artifacts
// table while the file stayed oversized.
func (d *DB) Enforce(ctx context.Context, max int64) error {
	if max <= 0 {
		return fmt.Errorf("max size must be positive")
	}
	for {
		size, e := d.sizeOnDisk()
		if e != nil {
			return e
		}
		if size <= max {
			return nil
		}
		res, e := d.db.ExecContext(ctx, "DELETE FROM artifacts WHERE rowid IN (SELECT rowid FROM artifacts ORDER BY CASE kind WHEN 'conversation' THEN 0 ELSE 1 END, accessed LIMIT 32)")
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil
		}
		if _, e = d.db.ExecContext(ctx, "PRAGMA incremental_vacuum"); e != nil {
			return e
		}
		if _, e = d.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
			return e
		}
	}
}
func Clear(path string) error {
	if path == "" {
		path = Path()
	}
	for _, s := range []string{"", "-wal", "-shm"} {
		e := os.Remove(path + s)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("remove cache: %w", e)
		}
	}
	return nil
}
