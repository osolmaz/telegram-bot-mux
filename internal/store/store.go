package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/routing"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1

var ErrBacklogFull = errors.New("client update backlog is full")

type Store struct {
	db           *sql.DB
	clientIDs    []string
	maxPending   int
	safetyWindow int
	notifyMu     sync.Mutex
	notify       chan struct{}
}

type Delivery struct {
	UpdateID int64
	Payload  json.RawMessage
}

type BacklogError struct {
	ClientID string
	Count    int
	Limit    int
}

func (e *BacklogError) Error() string {
	return fmt.Sprintf("%s: client %q has %d pending updates (limit %d)", ErrBacklogFull, e.ClientID, e.Count, e.Limit)
}

func (e *BacklogError) Unwrap() error { return ErrBacklogFull }

func Open(ctx context.Context, path string, clientIDs []string, maxPending, safetyWindow int) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("database path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if err := prepareDatabaseFile(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{
		db:           db,
		clientIDs:    slices.Clone(clientIDs),
		maxPending:   maxPending,
		safetyWindow: safetyWindow,
		notify:       make(chan struct{}),
	}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func prepareDatabaseFile(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("database path must be a regular file")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("protect database: %w", err)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect database path: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- explicit trusted configuration path.
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close new database: %w", err)
	}
	return nil
}

func (s *Store) initialize(ctx context.Context) error {
	statements := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
		`CREATE TABLE IF NOT EXISTS schema_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) STRICT`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize database: %w", err)
		}
	}
	return s.migrate(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := storedSchemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if err := migrateSchema(ctx, tx, current); err != nil {
		return err
	}
	if err := reconcileClients(ctx, tx, s.clientIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func storedSchemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	value, err := metaValue(ctx, tx, "schema_version")
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	current, err := strconv.Atoi(value)
	if err != nil {
		return 0, errors.New("stored schema version is invalid")
	}
	return current, nil
}

func migrateSchema(ctx context.Context, tx *sql.Tx, current int) error {
	if current > schemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d", current, schemaVersion)
	}
	if current == schemaVersion {
		return nil
	}
	if err := migrationOne(ctx, tx); err != nil {
		return err
	}
	return setMeta(ctx, tx, "schema_version", strconv.Itoa(schemaVersion))
}

func migrationOne(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE updates (
			update_id INTEGER PRIMARY KEY,
			update_type TEXT NOT NULL,
			payload BLOB NOT NULL,
			created_at TEXT NOT NULL
		) STRICT`,
		`CREATE TABLE clients (
			client_id TEXT PRIMARY KEY,
			ack_offset INTEGER NOT NULL DEFAULT 0,
			last_seen_at TEXT
		) STRICT`,
		`CREATE TABLE deliveries (
			client_id TEXT NOT NULL REFERENCES clients(client_id) ON DELETE CASCADE,
			update_id INTEGER NOT NULL REFERENCES updates(update_id) ON DELETE CASCADE,
			PRIMARY KEY (client_id, update_id)
		) STRICT, WITHOUT ROWID`,
		`CREATE INDEX deliveries_update_id_idx ON deliveries(update_id)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply schema migration: %w", err)
		}
	}
	return nil
}

func reconcileClients(ctx context.Context, tx *sql.Tx, clientIDs []string) error {
	configured := make(map[string]struct{}, len(clientIDs))
	for _, id := range clientIDs {
		configured[id] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO clients(client_id) VALUES (?) ON CONFLICT DO NOTHING`, id); err != nil {
			return fmt.Errorf("register client %q: %w", id, err)
		}
	}
	stored, err := storedClientIDs(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range stored {
		if _, exists := configured[id]; !exists {
			if err := deleteClient(ctx, tx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func storedClientIDs(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT client_id FROM clients`)
	if err != nil {
		return nil, fmt.Errorf("list stored clients: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stored client: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stored clients: %w", err)
	}
	return ids, nil
}

func deleteClient(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM clients WHERE client_id = ?`, id); err != nil {
		return fmt.Errorf("remove stale client %q: %w", id, err)
	}
	return nil
}

func metaValue(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (string, error) {
	var value string
	err := queryer.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&value)
	return value, err
}

func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO schema_meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set metadata %q: %w", key, err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UpstreamOffset(ctx context.Context) (int64, error) {
	value, err := metaValue(ctx, s.db, "upstream_offset")
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read upstream offset: %w", err)
	}
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		return 0, errors.New("stored upstream offset is invalid")
	}
	return offset, nil
}

func (s *Store) Ingest(ctx context.Context, updates []routing.Update, targets func(routing.Update) []string) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update ingest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.ingestTransaction(ctx, tx, updates, targets); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit update ingest: %w", err)
	}
	s.signal()
	return nil
}

func (s *Store) ingestTransaction(ctx context.Context, tx *sql.Tx, updates []routing.Update, targets func(routing.Update) []string) error {
	maxOffset, err := ingestUpdates(ctx, tx, updates, targets)
	if err != nil {
		return err
	}
	if err := enforceBacklogLimit(ctx, tx, s.clientIDs, s.maxPending); err != nil {
		return err
	}
	currentOffset, err := transactionOffset(ctx, tx)
	if err != nil {
		return err
	}
	if maxOffset <= currentOffset {
		return nil
	}
	return setMeta(ctx, tx, "upstream_offset", strconv.FormatInt(maxOffset, 10))
}

func ingestUpdates(ctx context.Context, tx *sql.Tx, updates []routing.Update, targets func(routing.Update) []string) (int64, error) {
	var maxOffset int64
	for _, update := range updates {
		inserted, err := insertUpdate(ctx, tx, update)
		if err != nil {
			return 0, err
		}
		if inserted {
			if err := insertDeliveries(ctx, tx, update.ID, targets(update)); err != nil {
				return 0, err
			}
		}
		nextOffset, err := incrementUpdateID(update.ID)
		if err != nil {
			return 0, err
		}
		maxOffset = max(maxOffset, nextOffset)
	}
	return maxOffset, nil
}

func insertUpdate(ctx context.Context, tx *sql.Tx, update routing.Update) (bool, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO updates(update_id, update_type, payload, created_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(update_id) DO NOTHING`, update.ID, update.Type, []byte(update.Raw), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("store update %d: %w", update.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect update %d insert: %w", update.ID, err)
	}
	return inserted > 0, nil
}

func insertDeliveries(ctx context.Context, tx *sql.Tx, updateID int64, clientIDs []string) error {
	for _, clientID := range clientIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(client_id, update_id) VALUES (?, ?)`, clientID, updateID); err != nil {
			return fmt.Errorf("route update %d to client %q: %w", updateID, clientID, err)
		}
	}
	return nil
}

func incrementUpdateID(updateID int64) (int64, error) {
	if updateID == int64(^uint64(0)>>1) {
		return 0, errors.New("update_id cannot be incremented")
	}
	return updateID + 1, nil
}

func enforceBacklogLimit(ctx context.Context, tx *sql.Tx, clientIDs []string, limit int) error {
	for _, clientID := range clientIDs {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE client_id = ?`, clientID).Scan(&count); err != nil {
			return fmt.Errorf("count client %q backlog: %w", clientID, err)
		}
		if count > limit {
			return &BacklogError{ClientID: clientID, Count: count, Limit: limit}
		}
	}
	return nil
}

func transactionOffset(ctx context.Context, tx *sql.Tx) (int64, error) {
	value, err := metaValue(ctx, tx, "upstream_offset")
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read transaction offset: %w", err)
	}
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.New("stored upstream offset is invalid")
	}
	return offset, nil
}

func (s *Store) GetUpdates(ctx context.Context, clientID string, offset int64, limit int) ([]Delivery, error) {
	if !slices.Contains(s.clientIDs, clientID) {
		return nil, fmt.Errorf("unknown client %q", clientID)
	}
	offset, err := s.prepareOffset(ctx, clientID, offset)
	if err != nil {
		return nil, err
	}
	updates, err := s.queryUpdates(ctx, clientID, offset, normalizeLimit(limit))
	if err != nil {
		return nil, err
	}
	if err := s.recordClientActivity(ctx, clientID); err != nil {
		return nil, err
	}
	return updates, nil
}

func normalizeLimit(limit int) int {
	if limit < 1 || limit > 100 {
		return 100
	}
	return limit
}

func (s *Store) prepareOffset(ctx context.Context, clientID string, offset int64) (int64, error) {
	if offset < 0 {
		resolved, err := s.negativeOffset(ctx, clientID, -offset)
		if err != nil {
			return 0, err
		}
		offset = resolved
	}
	if offset > 0 {
		if err := s.acknowledge(ctx, clientID, offset); err != nil {
			return 0, err
		}
	}
	return offset, nil
}

func (s *Store) queryUpdates(ctx context.Context, clientID string, offset int64, limit int) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.update_id, u.payload
		FROM deliveries d JOIN updates u ON u.update_id = d.update_id
		WHERE d.client_id = ? AND (? = 0 OR d.update_id >= ?)
		ORDER BY d.update_id LIMIT ?`, clientID, offset, offset, limit)
	if err != nil {
		return nil, fmt.Errorf("query client updates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	updates := make([]Delivery, 0, limit)
	for rows.Next() {
		var delivery Delivery
		var payload []byte
		if err := rows.Scan(&delivery.UpdateID, &payload); err != nil {
			return nil, fmt.Errorf("scan client update: %w", err)
		}
		delivery.Payload = slices.Clone(payload)
		updates = append(updates, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate client updates: %w", err)
	}
	return updates, nil
}

func (s *Store) recordClientActivity(ctx context.Context, clientID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clients SET last_seen_at = ? WHERE client_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), clientID)
	if err != nil {
		return fmt.Errorf("record client activity: %w", err)
	}
	return nil
}

func (s *Store) negativeOffset(ctx context.Context, clientID string, count int64) (int64, error) {
	if count > 100 {
		count = 100
	}
	var threshold sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(update_id) FROM (
		SELECT update_id FROM deliveries WHERE client_id = ? ORDER BY update_id DESC LIMIT ?
	)`, clientID, count).Scan(&threshold)
	if err != nil {
		return 0, fmt.Errorf("resolve negative offset: %w", err)
	}
	if !threshold.Valid {
		return 0, nil
	}
	return threshold.Int64, nil
}

func (s *Store) acknowledge(ctx context.Context, clientID string, offset int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin acknowledgment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE client_id = ? AND update_id < ?`, clientID, offset); err != nil {
		return fmt.Errorf("acknowledge client updates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE clients SET ack_offset = MAX(ack_offset, ?), last_seen_at = ? WHERE client_id = ?`, offset, time.Now().UTC().Format(time.RFC3339Nano), clientID); err != nil {
		return fmt.Errorf("record client offset: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM updates
		WHERE update_id <= COALESCE((SELECT MAX(update_id) FROM updates), 0) - ?
		AND NOT EXISTS (SELECT 1 FROM deliveries WHERE deliveries.update_id = updates.update_id)`, s.safetyWindow); err != nil {
		return fmt.Errorf("prune acknowledged updates: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit acknowledgment: %w", err)
	}
	return nil
}

func (s *Store) PendingCount(ctx context.Context, clientID string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE client_id = ?`, clientID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count pending updates: %w", err)
	}
	return count, nil
}

func (s *Store) Changes() <-chan struct{} {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notify
}

func (s *Store) signal() {
	s.notifyMu.Lock()
	close(s.notify)
	s.notify = make(chan struct{})
	s.notifyMu.Unlock()
}

func (s *Store) IntegrityCheck(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("run integrity check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("read integrity check: %w", err)
		}
		if result != "ok" {
			return fmt.Errorf("database integrity check failed: %s", result)
		}
	}
	return rows.Err()
}

func (s *Store) Backup(ctx context.Context, destination string) error {
	if !filepath.IsAbs(destination) {
		return errors.New("backup destination must be absolute")
	}
	if _, err := os.Stat(destination); err == nil {
		return errors.New("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	quoted := strings.ReplaceAll(destination, "'", "''")
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil { // #nosec G202 -- single quotes are escaped above.
		return fmt.Errorf("backup database: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("protect backup: %w", err)
	}
	return nil
}
