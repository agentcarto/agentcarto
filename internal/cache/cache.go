package cache

import (
	"bytes"
	"compress/gzip"
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
	"strings"
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
	// Configure the connection (WAL + a busy timeout that outlasts another
	// process's write) and create the schema:
	// sessions holds one JSON-encoded session per (plugin_id, session_id); artifacts caches
	// derived data (e.g. parsed conversations) per (plugin_id, session_id, kind), keyed also
	// by the session fingerprint and parser_version so stale entries are ignored on read.
	for _, q := range []string{
		// Patience first: switching the journal mode and creating the schema both
		// take the write lock, so a timeout set after them would come too late.
		// Several agentcarto processes share this file — a TUI left open, a couple of
		// agents running "search" at once — and a writer holds the lock for as long
		// as its transaction. A second's patience was not enough: a concurrent open
		// would give up with SQLITE_BUSY, and the run it belonged to fell back to no
		// cache and reparsed everything.
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE IF NOT EXISTS sessions (plugin_id TEXT, session_id TEXT, data BLOB NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY(plugin_id,session_id))",
		"CREATE TABLE IF NOT EXISTS artifacts (plugin_id TEXT, session_id TEXT, fingerprint TEXT, parser_version TEXT, kind TEXT, data BLOB NOT NULL, accessed INTEGER NOT NULL, PRIMARY KEY(plugin_id,session_id,kind))",
	} {
		if _, e = d.Exec(q); e != nil {
			d.Close()
			return nil, e
		}
	}
	// Enforce reclaims space with incremental_vacuum, which is a no-op unless
	// auto_vacuum=incremental. The setting only takes effect on an empty database
	// or after a full VACUUM, so migrate existing files once here. On a database
	// that holds nothing yet the VACUUM is instant, but it still takes an
	// exclusive lock, which is why it is not left to run while others wait.
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
	return d.put(ctx, s, kind, b)
}

// put writes the bytes of one artifact, whatever they are. The row is keyed by
// (plugin, session, kind) and carries the fingerprint and parser version the
// bytes were made from, so a session that changed underneath never reads back a
// stale artifact.
func (d *DB) put(ctx context.Context, s domain.Session, kind string, b []byte) error {
	_, e := d.db.ExecContext(ctx, "INSERT INTO artifacts VALUES(?,?,?,?,?,?,?) ON CONFLICT(plugin_id,session_id,kind) DO UPDATE SET fingerprint=excluded.fingerprint,parser_version=excluded.parser_version,data=excluded.data,accessed=excluded.accessed", s.PluginID, s.SessionID, s.Fingerprint, s.ParserVersion, kind, b, time.Now().Unix())
	return e
}

// PutBlob stores a value the way PutArtifact does, gzipped. It is for the one
// artifact that is the size of the log it came from — the conversation itself —
// where keeping the JSON as it is would make the cache as large as every log on
// the machine put together.
func (d *DB) PutBlob(ctx context.Context, s domain.Session, kind string, v any) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if e := json.NewEncoder(zw).Encode(v); e != nil {
		return e
	}
	if e := zw.Close(); e != nil {
		return e
	}
	return d.put(ctx, s, kind, buf.Bytes())
}

// GetBlob reads back what PutBlob wrote (ok=false if it is not there, was
// written for a different version of the session, or cannot be read).
func (d *DB) GetBlob(ctx context.Context, s domain.Session, kind string, dst any) bool {
	var b []byte
	if d.db.QueryRowContext(ctx, "SELECT data FROM artifacts WHERE plugin_id=? AND session_id=? AND fingerprint=? AND parser_version=? AND kind=?", s.PluginID, s.SessionID, s.Fingerprint, s.ParserVersion, kind).Scan(&b) != nil {
		return false
	}
	zr, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return false
	}
	defer zr.Close()
	if json.NewDecoder(zr).Decode(dst) != nil {
		return false
	}
	_, _ = d.db.ExecContext(ctx, "UPDATE artifacts SET accessed=? WHERE plugin_id=? AND session_id=? AND kind=?", time.Now().Unix(), s.PluginID, s.SessionID, kind)
	return true
}

// HasArtifact reports whether an artifact of this kind is stored for this exact
// version of the session. It answers without reading the bytes, which is what a
// caller deciding whether to parse a session needs to know.
func (d *DB) HasArtifact(ctx context.Context, s domain.Session, kind string) bool {
	var one int
	return d.db.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE plugin_id=? AND session_id=? AND fingerprint=? AND parser_version=? AND kind=?", s.PluginID, s.SessionID, s.Fingerprint, s.ParserVersion, kind).Scan(&one) == nil
}

// DropSupersededArtifacts deletes the earlier generations of the kinds given. A
// kind is "<name>-v<n>", and a change to what is cached leaves the previous
// generation behind: nothing reads it again, and each bump would otherwise add
// another copy of the whole index.
//
// Only the same name is touched. Deleting every kind that was not named — which
// is what this did — means an older agentcarto sharing this cache silently
// erases a kind it has never heard of. That is survivable for the index, which
// is rebuilt by reparsing, and not survivable for the conversation, which is the
// only copy left once its log is deleted.
func (d *DB) DropSupersededArtifacts(ctx context.Context, kinds ...string) error {
	for _, k := range kinds {
		name, _, ok := strings.Cut(k, "-v")
		if !ok {
			continue // not a versioned kind: there is no generation to compare with
		}
		if _, e := d.db.ExecContext(ctx, "DELETE FROM artifacts WHERE kind LIKE ? AND kind <> ?", name+"-v%", k); e != nil {
			return e
		}
	}
	return nil
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
// Prune drops the rows of sessions that are neither on disk nor readable from
// here. A session whose log was deleted is kept as long as its conversation was
// stored — this is the only copy of it now, and letting it expire would throw
// away the thing the cache is for. One whose log is gone and whose conversation
// never made it here is a title, a time and nothing else, and max_age collects
// those.
//
// Only sessions of a plugin that scanned successfully are considered: a plugin
// that failed to start finds nothing, which must not be read as "the user
// deleted everything".
func (d *DB) Prune(ctx context.Context, sessions []domain.Session, successful map[string]bool, maxAge time.Duration) error {
	seen := map[domain.SessionKey]bool{}
	for _, s := range sessions {
		seen[s.Key()] = true
	}
	rows, e := d.db.QueryContext(ctx, `SELECT plugin_id,session_id,seen FROM sessions s
		WHERE NOT EXISTS (SELECT 1 FROM artifacts a
			WHERE a.plugin_id = s.plugin_id AND a.session_id = s.session_id AND a.kind LIKE 'conversation-%')`)
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
// Size is what the cache occupies on disk right now, write-ahead log included.
// A caller about to store something large asks this first: a store at its limit
// should stop taking conversations rather than make room by deleting one it
// will be asked for again on the next run.
func (d *DB) Size() int64 {
	n, _ := d.sizeOnDisk()
	return n
}

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
		// What can be rebuilt goes first. A conversation of a session that is still
		// on disk is reparsed from the log if it is needed again; the index is
		// reparsed too and is small enough that dropping it frees little. A
		// conversation whose log is gone is the only copy there is, so it is the
		// last thing to go — a session is taken to be gone when the last scan did
		// not see it, which is what its seen time says.
		res, e := d.db.ExecContext(ctx, `DELETE FROM artifacts WHERE rowid IN (
			SELECT a.rowid FROM artifacts a
			LEFT JOIN sessions s ON s.plugin_id = a.plugin_id AND s.session_id = a.session_id
			ORDER BY CASE
				WHEN a.kind LIKE 'conversation-%' AND s.seen >= (SELECT max(seen) FROM sessions) THEN 0
				WHEN a.kind LIKE 'conversation-%' THEN 2
				ELSE 1
			END, a.accessed LIMIT 32)`)
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
