package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/google/uuid"

	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
)

// lowWatermarkRatio is the cache target after an eviction sweep (60% of max).
const lowWatermarkRatio = 0.60

const table = "local_media_files"

var (
	imageExts = map[string]struct{}{".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".webp": {}, ".bmp": {}}
	videoExts = map[string]struct{}{".mp4": {}, ".mov": {}, ".m4v": {}, ".webm": {}, ".avi": {}, ".mkv": {}}
)

// LimitProvider returns the cache size limit in bytes for one media type
// (<= 0 means unlimited). It is consulted on every operation, so the limit
// can change at runtime.
type LimitProvider func(MediaType) int64

// ConfigLimitBytes is the default LimitProvider: cache.local.<type>_max_mb
// from the process-global configuration, converted to bytes.
func ConfigLimitBytes(mediaType MediaType) int64 {
	limitMB := int64(config.Global().GetInt("cache.local."+string(mediaType)+"_max_mb", 0))
	if limitMB < 0 {
		limitMB = 0
	}
	return limitMB * 1024 * 1024
}

// mediaState is the per-media-type serialisation point and index bookkeeping.
type mediaState struct {
	mu sync.Mutex
	// indexed is true while the SQLite index is known to mirror the files on
	// disk. It is cleared whenever files are written without being indexed
	// (cache limit disabled) so enabling a limit later triggers a rescan.
	indexed atomic.Bool
}

// LocalMediaCacheStore manages local media files and enforces per-type
// cache limits. The SQLite index tracks file sizes and creation timestamps
// so eviction can target the low-watermark target.
//
// The index database is opened once per store (lazily) and reused; call
// Close to release it.
type LocalMediaCacheStore struct {
	image, video mediaState
	limit        LimitProvider
	now          func() time.Time

	dbMu   sync.Mutex
	db     *sql.DB
	dbPath string
}

// NewLocalMediaCacheStore returns a store whose limits come from the global
// configuration (cache.local.image_max_mb / video_max_mb).
func NewLocalMediaCacheStore() *LocalMediaCacheStore {
	return NewLocalMediaCacheStoreWithLimits(ConfigLimitBytes)
}

// NewLocalMediaCacheStoreWithLimits returns a store using the given limit
// provider (nil falls back to the configuration).
func NewLocalMediaCacheStoreWithLimits(limit LimitProvider) *LocalMediaCacheStore {
	if limit == nil {
		limit = ConfigLimitBytes
	}
	return &LocalMediaCacheStore{limit: limit, now: platform.Now}
}

// Close releases the index database. The store stays usable: the next
// operation that needs the index reopens it.
func (s *LocalMediaCacheStore) Close() error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	s.image.indexed.Store(false)
	s.video.indexed.Store(false)
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db, s.dbPath = nil, ""
	return err
}

// SaveImage persists an image and returns the stable local file ID.
// The extension is derived from the MIME type (.png for PNG, .jpg otherwise).
func (s *LocalMediaCacheStore) SaveImage(raw []byte, mime, fileID string) (string, error) {
	ext := ".jpg"
	if strings.Contains(strings.ToLower(mime), "png") {
		ext = ".png"
	}
	if _, err := s.save(MediaImage, fileID, raw, ext); err != nil {
		return "", err
	}
	return fileID, nil
}

// SaveVideo persists a video file and returns the final file path.
func (s *LocalMediaCacheStore) SaveVideo(raw []byte, fileID string) (string, error) {
	return s.save(MediaVideo, fileID, raw, ".mp4")
}

// Rebuild rebuilds the on-disk index for one media type and enforces limits.
// With no limit configured nothing is indexed (and the index is flagged stale
// so a limit enabled later is picked up by the next save or Rebuild).
func (s *LocalMediaCacheStore) Rebuild(mediaType MediaType) error {
	st := s.state(mediaType)
	if s.limitBytes(mediaType) <= 0 {
		st.indexed.Store(false)
		return nil
	}
	release := s.guard(mediaType)
	defer release()
	db, err := s.database()
	if err != nil {
		return err
	}
	return s.reindex(db, mediaType)
}

// Delete removes one local media file by name and updates the index.
func (s *LocalMediaCacheStore) Delete(mediaType MediaType, name string) (bool, error) {
	safe, err := s.validateName(mediaType, name)
	if err != nil {
		return false, err
	}
	dir, err := s.mediaDir(mediaType)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, safe)
	release := s.guard(mediaType)
	defer release()
	existed := fileExists(path)
	if existed {
		_ = os.Remove(path)
	}
	if err := s.deleteIndexRows(mediaType, safe); err != nil {
		return existed, err
	}
	return existed, nil
}

// Clear removes all tracked files for one media type.
func (s *LocalMediaCacheStore) Clear(mediaType MediaType) (int, error) {
	dir, err := s.mediaDir(mediaType)
	if err != nil {
		return 0, err
	}
	allowed := allowedExts(mediaType)
	release := s.guard(mediaType)
	defer release()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if _, ok := allowed[ext]; !ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
			removed++
		}
	}
	if err := s.deleteIndexRows(mediaType, ""); err != nil {
		return removed, err
	}
	return removed, nil
}

// --- save path ---

func (s *LocalMediaCacheStore) save(mediaType MediaType, fileID string, raw []byte, ext string) (string, error) {
	dir, err := s.mediaDir(mediaType)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fileID+ext)
	if s.limitBytes(mediaType) <= 0 {
		// No limit: skip the index, but remember it no longer mirrors the
		// directory so enabling a limit later rescans first.
		s.state(mediaType).indexed.Store(false)
		if err := writeIfMissing(path, raw); err != nil {
			return "", err
		}
		return path, nil
	}
	release := s.guard(mediaType)
	defer release()
	db, err := s.database()
	if err != nil {
		return "", err
	}
	if !s.state(mediaType).indexed.Load() {
		if err := s.reindex(db, mediaType); err != nil {
			return "", err
		}
	}
	if fileExists(path) {
		if err := s.upsertExistingRow(db, mediaType, path); err != nil {
			return "", err
		}
	} else {
		if err := atomicWrite(path, raw); err != nil {
			return "", err
		}
		if err := s.upsertNewRow(db, mediaType, path); err != nil {
			return "", err
		}
	}
	protected := map[string]struct{}{filepath.Base(path): {}}
	if err := s.enforceLimit(db, mediaType, protected); err != nil {
		return "", err
	}
	return path, nil
}

// --- limits and paths ---

func (s *LocalMediaCacheStore) limitBytes(mediaType MediaType) int64 {
	if n := s.limit(mediaType); n > 0 {
		return n
	}
	return 0
}

func (s *LocalMediaCacheStore) targetBytes(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return 0
	}
	return int64(float64(maxBytes) * lowWatermarkRatio)
}

func (s *LocalMediaCacheStore) mediaDir(mediaType MediaType) (string, error) {
	if mediaType == MediaImage {
		return ImageFilesDir()
	}
	return VideoFilesDir()
}

func allowedExts(mediaType MediaType) map[string]struct{} {
	if mediaType == MediaImage {
		return imageExts
	}
	return videoExts
}

func (s *LocalMediaCacheStore) validateName(mediaType MediaType, name string) (string, error) {
	value := strings.TrimSpace(name)
	if value == "" {
		return "", fmt.Errorf("missing file name")
	}
	if filepath.Base(value) != value {
		return "", fmt.Errorf("invalid file name")
	}
	ext := strings.ToLower(filepath.Ext(value))
	allowed := allowedExts(mediaType)
	if _, ok := allowed[ext]; !ok {
		return "", fmt.Errorf("unsupported file type")
	}
	return value, nil
}

func writeIfMissing(path string, raw []byte) error {
	if fileExists(path) {
		return nil
	}
	return atomicWrite(path, raw)
}

func atomicWrite(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if fileExists(path) {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+uuid.NewString()+".part")
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if fileExists(path) {
		return nil
	}
	return os.Rename(tmp, path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- locking and database handle ---

func (s *LocalMediaCacheStore) state(mediaType MediaType) *mediaState {
	if mediaType == MediaVideo {
		return &s.video
	}
	return &s.image
}

// guard acquires the per-media-type thread mutex (no flock — this is a
// single-process Go binary; cross-process locking is unnecessary).
func (s *LocalMediaCacheStore) guard(mediaType MediaType) (release func()) {
	mu := &s.state(mediaType).mu
	mu.Lock()
	return mu.Unlock
}

// database returns the shared index handle, opening it on first use. The
// handle is reopened if DATA_DIR (and with it the index path) changed.
func (s *LocalMediaCacheStore) database() (*sql.DB, error) {
	dbPath, err := LocalMediaCacheDBPath()
	if err != nil {
		return nil, err
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if s.db != nil && s.dbPath == dbPath {
		return s.db, nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db, s.dbPath = nil, ""
		s.image.indexed.Store(false)
		s.video.indexed.Store(false)
	}
	db, err := openIndex(dbPath)
	if err != nil {
		return nil, err
	}
	s.db, s.dbPath = db, dbPath
	return db, nil
}

// existingDatabase returns the index handle without creating the database:
// nil when it has never been created.
func (s *LocalMediaCacheStore) existingDatabase() (*sql.DB, error) {
	dbPath, err := LocalMediaCacheDBPath()
	if err != nil {
		return nil, err
	}
	s.dbMu.Lock()
	open := s.db != nil && s.dbPath == dbPath
	s.dbMu.Unlock()
	if !open && !fileExists(dbPath) {
		return nil, nil
	}
	return s.database()
}

func openIndex(dbPath string) (*sql.DB, error) {
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: eviction drains its cursor before issuing DELETEs
	// because an open Rows would otherwise hold the only connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS ` + table + ` (
    media_type    TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    size_bytes    INTEGER NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    PRIMARY KEY (media_type, name)
);
CREATE INDEX IF NOT EXISTS idx_local_media_order
    ON ` + table + ` (media_type, created_at_ns, name);
`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// --- index maintenance ---

// indexedFile is one media file found on disk during a rescan.
type indexedFile struct {
	name  string
	size  int64
	mtime int64 // UnixNano
}

// scanMediaDir lists the media files of one type in dir.
func scanMediaDir(dir string, mediaType MediaType) ([]indexedFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	allowed := allowedExts(mediaType)
	files := make([]indexedFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, ok := allowed[strings.ToLower(filepath.Ext(entry.Name()))]; !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, indexedFile{name: entry.Name(), size: info.Size(), mtime: info.ModTime().UnixNano()})
	}
	return files, nil
}

// reindex replaces the index rows of one media type with the files currently
// on disk and enforces the limit. Files are write-once, so a file's mtime is
// its creation time and eviction order survives restarts. Caller holds the
// media guard.
func (s *LocalMediaCacheStore) reindex(db *sql.DB, mediaType MediaType) error {
	dir, err := s.mediaDir(mediaType)
	if err != nil {
		return err
	}
	files, err := scanMediaDir(dir, mediaType)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM "+table+" WHERE media_type = ?", string(mediaType)); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec(
			"INSERT INTO "+table+" (media_type, name, size_bytes, created_at_ns, updated_at_ns) VALUES (?, ?, ?, ?, ?)",
			string(mediaType), f.name, f.size, f.mtime, f.mtime,
		); err != nil {
			return err
		}
	}
	if err := s.enforceLimit(tx, mediaType, nil); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.state(mediaType).indexed.Store(true)
	return nil
}

func (s *LocalMediaCacheStore) upsertExistingRow(db *sql.DB, mediaType MediaType, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	createdAt, err := s.lookupCreatedAtNS(db, mediaType, filepath.Base(path), info.ModTime().UnixNano())
	if err != nil {
		return err
	}
	_, err = db.Exec(`
INSERT INTO `+table+` (media_type, name, size_bytes, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(media_type, name) DO UPDATE SET
    size_bytes = excluded.size_bytes,
    updated_at_ns = excluded.updated_at_ns`,
		string(mediaType), filepath.Base(path), info.Size(), createdAt, info.ModTime().UnixNano(),
	)
	return err
}

func (s *LocalMediaCacheStore) upsertNewRow(db *sql.DB, mediaType MediaType, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	nowNs := s.now().UnixNano()
	_, err = db.Exec(`
INSERT INTO `+table+` (media_type, name, size_bytes, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(media_type, name) DO UPDATE SET
    size_bytes = excluded.size_bytes,
    created_at_ns = excluded.created_at_ns,
    updated_at_ns = excluded.updated_at_ns`,
		string(mediaType), filepath.Base(path), info.Size(), nowNs, nowNs,
	)
	return err
}

func (s *LocalMediaCacheStore) lookupCreatedAtNS(db *sql.DB, mediaType MediaType, name string, fallback int64) (int64, error) {
	row := db.QueryRow(`SELECT created_at_ns FROM `+table+` WHERE media_type = ? AND name = ?`,
		string(mediaType), name)
	var v int64
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return fallback, nil
		}
		return 0, err
	}
	return v, nil
}

// deleteIndexRows removes the index row of one file, or of the whole media
// type when name is empty. It never creates the database.
func (s *LocalMediaCacheStore) deleteIndexRows(mediaType MediaType, name string) error {
	db, err := s.existingDatabase()
	if err != nil || db == nil {
		return err
	}
	if name == "" {
		_, err = db.Exec(`DELETE FROM `+table+` WHERE media_type = ?`, string(mediaType))
		return err
	}
	_, err = db.Exec(`DELETE FROM `+table+` WHERE media_type = ? AND name = ?`, string(mediaType), name)
	return err
}

// --- eviction ---

// dbExec is the minimal subset of *sql.DB / *sql.Tx used by the eviction
// logic. Both types satisfy this interface.
type dbExec interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// evictionCandidate is one indexed file, listed oldest first.
type evictionCandidate struct {
	name string
	size int64
}

// enforceLimit trims the cache down to the low-watermark target by evicting
// the oldest files. The newest file and every name in protected are kept.
// Caller holds the media guard.
func (s *LocalMediaCacheStore) enforceLimit(db dbExec, mediaType MediaType, protected map[string]struct{}) error {
	maxBytes := s.limitBytes(mediaType)
	if maxBytes <= 0 {
		return nil
	}
	usage, err := s.usageBytes(db, mediaType)
	if err != nil {
		return err
	}
	if usage <= maxBytes {
		return nil
	}
	keep := map[string]struct{}{}
	for n := range protected {
		keep[n] = struct{}{}
	}
	newest, err := s.newestName(db, mediaType)
	if err != nil {
		return err
	}
	if newest != "" {
		keep[newest] = struct{}{}
	}
	candidates, err := s.evictionCandidates(db, mediaType)
	if err != nil {
		return err
	}
	dir, err := s.mediaDir(mediaType)
	if err != nil {
		return err
	}
	target := s.targetBytes(maxBytes)
	removed, usage, err := evict(db, mediaType, dir, candidates, keep, usage, target)
	if removed > 0 {
		logger.Infof("local media cache trimmed: media_type=%s removed=%d usage_bytes=%d limit_bytes=%d target_bytes=%d",
			mediaType, removed, usage, maxBytes, target)
	}
	return err
}

// evictionCandidates lists the indexed files of one type, oldest first. The
// cursor is fully drained and closed before returning: with a single
// connection an open Rows would block any DELETE issued while iterating.
func (s *LocalMediaCacheStore) evictionCandidates(db dbExec, mediaType MediaType) ([]evictionCandidate, error) {
	rows, err := db.Query(`SELECT name, size_bytes FROM `+table+` WHERE media_type = ? ORDER BY created_at_ns ASC, name ASC`,
		string(mediaType))
	if err != nil {
		return nil, err
	}
	var out []evictionCandidate
	for rows.Next() {
		var c evictionCandidate
		if err := rows.Scan(&c.name, &c.size); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// evict removes candidates (skipping keep) from disk and index until usage
// drops to target. It returns the number removed and the remaining usage.
func evict(db dbExec, mediaType MediaType, dir string, candidates []evictionCandidate,
	keep map[string]struct{}, usage, target int64) (removed int, remaining int64, err error) {
	for _, c := range candidates {
		if usage <= target {
			break
		}
		if _, ok := keep[c.name]; ok {
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, c.name)); rmErr != nil {
			if !os.IsNotExist(rmErr) {
				logger.Warnf("local media cache delete failed: media_type=%s name=%s error=%v", mediaType, c.name, rmErr)
			}
			continue
		}
		if _, err := db.Exec(`DELETE FROM `+table+` WHERE media_type = ? AND name = ?`,
			string(mediaType), c.name); err != nil {
			return removed, usage, err
		}
		usage -= c.size
		if usage < 0 {
			usage = 0
		}
		removed++
	}
	return removed, usage, nil
}

func (s *LocalMediaCacheStore) usageBytes(db dbExec, mediaType MediaType) (int64, error) {
	row := db.QueryRow(`SELECT COALESCE(SUM(size_bytes), 0) AS total FROM `+table+` WHERE media_type = ?`,
		string(mediaType))
	var v int64
	if err := row.Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

func (s *LocalMediaCacheStore) newestName(db dbExec, mediaType MediaType) (string, error) {
	row := db.QueryRow(`SELECT name FROM `+table+` WHERE media_type = ? ORDER BY created_at_ns DESC, name DESC LIMIT 1`,
		string(mediaType))
	var name string
	if err := row.Scan(&name); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return name, nil
}
