package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteBusyTimeoutMS = 5000

// SQLiteCheckpointStore is a durable, process-shared checkpoint store backed by
// one SQLite database file. It implements lease acquisition, renewal, fencing,
// and checkpoint writes on the same database row so ownership checks and state
// publication share SQLite's transactional boundary.
//
// The store is intended for multiple processes that can safely open the same
// local SQLite database file. It does not claim cross-host coordination over a
// network filesystem.
type SQLiteCheckpointStore struct {
	db            *sql.DB
	leaseDuration time.Duration
}

// NewSQLiteCheckpointStore opens or creates a SQLite database at path.
// leaseDuration controls ownership expiry; active leases are renewed at one
// third of this duration when the Runtime uses this store.
func NewSQLiteCheckpointStore(path string, leaseDuration time.Duration) (*SQLiteCheckpointStore, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: sqlite path is required", ErrInvalidRequest)
	}
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("%w: lease duration must be positive", ErrInvalidRequest)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsnURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := dsnURL.Query()
	query.Set("_busy_timeout", fmt.Sprint(sqliteBusyTimeoutMS))
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "FULL")
	dsnURL.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		return nil, err
	}
	store := &SQLiteCheckpointStore{db: db, leaseDuration: leaseDuration}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteCheckpointStore) initialize(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_executions (
    execution_id TEXT PRIMARY KEY,
    checkpoint_json BLOB,
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    fencing_token INTEGER NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    lease_expires_unix_ns INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`)
	return err
}

// Close releases database resources owned by the store. Active execution leases
// remain rows in SQLite and expire normally; Close does not force-release them.
func (s *SQLiteCheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteCheckpointStore) AcquireExecutionLease(ctx context.Context, executionID string) (uint64, func(), error) {
	if err := checkSQLiteStoreContext(ctx); err != nil {
		return 0, nil, err
	}
	if executionID == "" {
		return 0, nil, fmt.Errorf("%w: execution ID is required", ErrInvalidRequest)
	}
	now := time.Now().UnixNano()
	expiresAt, err := sqliteLeaseExpiry(now, s.leaseDuration)
	if err != nil {
		return 0, nil, err
	}

	var token int64
	err = s.db.QueryRowContext(ctx, `
INSERT INTO harness_executions (
    execution_id, checkpoint_json, revision, fencing_token, lease_expires_unix_ns
) VALUES (?, NULL, 0, 1, ?)
ON CONFLICT(execution_id) DO UPDATE SET
    fencing_token = harness_executions.fencing_token + 1,
    lease_expires_unix_ns = excluded.lease_expires_unix_ns
WHERE harness_executions.lease_expires_unix_ns <= ?
  AND harness_executions.fencing_token < ?
RETURNING fencing_token`, executionID, expiresAt, now, int64(math.MaxInt64)).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return s.classifyLeaseAcquisition(ctx, executionID, now)
	}
	if err != nil {
		return 0, nil, err
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), checkpointWriteTimeout)
			defer cancel()
			_, _ = s.db.ExecContext(releaseCtx, `
UPDATE harness_executions
SET lease_expires_unix_ns = 0
WHERE execution_id = ? AND fencing_token = ?`, executionID, token)
		})
	}
	return uint64(token), release, nil
}

func (s *SQLiteCheckpointStore) classifyLeaseAcquisition(ctx context.Context, executionID string, now int64) (uint64, func(), error) {
	var token, expiresAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT fencing_token, lease_expires_unix_ns
FROM harness_executions
WHERE execution_id = ?`, executionID).Scan(&token, &expiresAt)
	if err != nil {
		return 0, nil, err
	}
	if expiresAt > now {
		return 0, nil, ErrExecutionBusy
	}
	if token == math.MaxInt64 {
		return 0, nil, fmt.Errorf("%w: execution %q fencing token exhausted", ErrExecutionFenced, executionID)
	}
	return 0, nil, ErrExecutionBusy
}

func (s *SQLiteCheckpointStore) ExecutionLeaseRenewalInterval() time.Duration {
	interval := s.leaseDuration / 3
	if interval <= 0 {
		return s.leaseDuration
	}
	return interval
}

func (s *SQLiteCheckpointStore) RenewExecutionLease(ctx context.Context, executionID string, fencingToken uint64) error {
	if err := checkSQLiteStoreContext(ctx); err != nil {
		return err
	}
	if executionID == "" || fencingToken == 0 {
		return fmt.Errorf("%w: execution ID and fencing token are required", ErrExecutionLeaseRequired)
	}
	token, err := sqliteInt64(fencingToken, "fencing token")
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	expiresAt, err := sqliteLeaseExpiry(now, s.leaseDuration)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE harness_executions
SET lease_expires_unix_ns = ?
WHERE execution_id = ?
  AND fencing_token = ?
  AND lease_expires_unix_ns > ?`, expiresAt, executionID, token, now)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("%w: execution %q token %d", ErrExecutionFenced, executionID, fencingToken)
	}
	return nil
}

func (s *SQLiteCheckpointStore) Create(ctx context.Context, _ Checkpoint) error {
	if err := checkSQLiteStoreContext(ctx); err != nil {
		return err
	}
	return ErrExecutionLeaseRequired
}

func (s *SQLiteCheckpointStore) Save(ctx context.Context, _ Checkpoint) error {
	if err := checkSQLiteStoreContext(ctx); err != nil {
		return err
	}
	return ErrExecutionLeaseRequired
}

func (s *SQLiteCheckpointStore) CreateFenced(ctx context.Context, checkpoint Checkpoint, fencingToken uint64) error {
	return s.writeFenced(ctx, checkpoint, fencingToken, true)
}

func (s *SQLiteCheckpointStore) SaveFenced(ctx context.Context, checkpoint Checkpoint, fencingToken uint64) error {
	return s.writeFenced(ctx, checkpoint, fencingToken, false)
}

func (s *SQLiteCheckpointStore) Load(ctx context.Context, executionID string) (Checkpoint, error) {
	if err := checkSQLiteStoreContext(ctx); err != nil {
		return Checkpoint{}, err
	}
	if executionID == "" {
		return Checkpoint{}, fmt.Errorf("%w: execution ID is required", ErrInvalidCheckpoint)
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `
SELECT checkpoint_json
FROM harness_executions
WHERE execution_id = ? AND checkpoint_json IS NOT NULL`, executionID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, fmt.Errorf("%w: %q", ErrExecutionNotFound, executionID)
	}
	if err != nil {
		return Checkpoint{}, err
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %w", ErrInvalidCheckpoint, err)
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return Checkpoint{}, err
	}
	if checkpoint.ExecutionID != executionID {
		return Checkpoint{}, fmt.Errorf("%w: execution ID mismatch", ErrInvalidCheckpoint)
	}
	return checkpoint, nil
}

func (s *SQLiteCheckpointStore) writeFenced(ctx context.Context, checkpoint Checkpoint, fencingToken uint64, create bool) error {
	if err := checkSQLiteStoreContext(ctx); err != nil {
		return err
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if fencingToken == 0 {
		return ErrExecutionLeaseRequired
	}
	token, err := sqliteInt64(fencingToken, "fencing token")
	if err != nil {
		return err
	}
	revision, err := sqliteInt64(checkpoint.Revision, "checkpoint revision")
	if err != nil {
		return err
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()

	var result sql.Result
	if create {
		result, err = s.db.ExecContext(ctx, `
UPDATE harness_executions
SET checkpoint_json = ?, revision = ?
WHERE execution_id = ?
  AND checkpoint_json IS NULL
  AND fencing_token = ?
  AND lease_expires_unix_ns > ?`, data, revision, checkpoint.ExecutionID, token, now)
	} else {
		result, err = s.db.ExecContext(ctx, `
UPDATE harness_executions
SET checkpoint_json = ?, revision = ?
WHERE execution_id = ?
  AND checkpoint_json IS NOT NULL
  AND fencing_token = ?
  AND lease_expires_unix_ns > ?
  AND (revision = 0 OR ? > revision)`, data, revision, checkpoint.ExecutionID, token, now, revision)
	}
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 1 {
		return nil
	}
	return s.classifyFencedWrite(ctx, checkpoint, token, now, create)
}

func (s *SQLiteCheckpointStore) classifyFencedWrite(ctx context.Context, checkpoint Checkpoint, token, now int64, create bool) error {
	var hasCheckpoint bool
	var revision, currentToken, expiresAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT checkpoint_json IS NOT NULL, revision, fencing_token, lease_expires_unix_ns
FROM harness_executions
WHERE execution_id = ?`, checkpoint.ExecutionID).Scan(&hasCheckpoint, &revision, &currentToken, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: execution %q token %d", ErrExecutionFenced, checkpoint.ExecutionID, token)
	}
	if err != nil {
		return err
	}
	if currentToken != token || expiresAt <= now {
		return fmt.Errorf("%w: execution %q token %d", ErrExecutionFenced, checkpoint.ExecutionID, token)
	}
	if create {
		if hasCheckpoint {
			return fmt.Errorf("%w: %q", ErrExecutionExists, checkpoint.ExecutionID)
		}
		return fmt.Errorf("%w: execution %q create was not applied", ErrCheckpointConflict, checkpoint.ExecutionID)
	}
	if !hasCheckpoint {
		return fmt.Errorf("%w: %q", ErrExecutionNotFound, checkpoint.ExecutionID)
	}
	return fmt.Errorf("%w: execution %q has revision %d, attempted %d", ErrCheckpointConflict, checkpoint.ExecutionID, revision, checkpoint.Revision)
}

func checkSQLiteStoreContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	return ctx.Err()
}

func sqliteInt64(value uint64, name string) (int64, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %s exceeds sqlite integer range", ErrInvalidCheckpoint, name)
	}
	return int64(value), nil
}

func sqliteLeaseExpiry(now int64, duration time.Duration) (int64, error) {
	delta := int64(duration)
	if delta <= 0 || now > math.MaxInt64-delta {
		return 0, fmt.Errorf("%w: invalid lease deadline", ErrInvalidRequest)
	}
	return now + delta, nil
}
