package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"emulator-aws-sqs/internal/storage"
)

type Store struct {
	db *sql.DB
}

func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS queues (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL,
			region TEXT NOT NULL,
			name TEXT NOT NULL,
			url TEXT NOT NULL UNIQUE,
			arn TEXT NOT NULL UNIQUE,
			fifo INTEGER NOT NULL,
			attributes_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			available_at INTEGER NOT NULL,
			last_modified_at INTEGER NOT NULL,
			deleted_at INTEGER,
			reuse_blocked_till INTEGER,
			purge_requested_at INTEGER,
			purge_expires_at INTEGER,
			UNIQUE(account_id, region, name)
		)`,
		`CREATE TABLE IF NOT EXISTS pending_queue_attributes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_id INTEGER NOT NULL,
			attributes_json TEXT NOT NULL,
			effective_at INTEGER NOT NULL,
			submitted_at INTEGER NOT NULL,
			FOREIGN KEY(queue_id) REFERENCES queues(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS queue_tags (
			queue_id INTEGER NOT NULL,
			tag_key TEXT NOT NULL,
			tag_value TEXT NOT NULL,
			PRIMARY KEY(queue_id, tag_key),
			FOREIGN KEY(queue_id) REFERENCES queues(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS message_move_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_handle TEXT NOT NULL UNIQUE,
			source_queue_id INTEGER NOT NULL,
			destination_queue_id INTEGER,
			status TEXT NOT NULL,
			max_messages_per_second INTEGER,
			approx_messages_moved INTEGER NOT NULL DEFAULT 0,
			approx_messages_to_move INTEGER,
			failure_reason TEXT NOT NULL DEFAULT '',
			started_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			completed_at INTEGER,
			FOREIGN KEY(source_queue_id) REFERENCES queues(id) ON DELETE CASCADE,
			FOREIGN KEY(destination_queue_id) REFERENCES queues(id) ON DELETE SET NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_id INTEGER NOT NULL,
			message_id TEXT NOT NULL UNIQUE,
			body TEXT NOT NULL,
			body_md5 TEXT NOT NULL,
			attributes_json TEXT NOT NULL,
			system_attributes_json TEXT NOT NULL,
			sent_at INTEGER NOT NULL,
			original_sent_at INTEGER NOT NULL,
			first_received_at INTEGER,
			receive_count INTEGER NOT NULL DEFAULT 0,
			available_at INTEGER NOT NULL,
			visibility_deadline INTEGER,
			retention_deadline INTEGER NOT NULL,
			deleted_at INTEGER,
			group_id TEXT NOT NULL DEFAULT '',
			dedup_id TEXT NOT NULL DEFAULT '',
			sequence_number TEXT NOT NULL DEFAULT '',
			dead_letter_source_arn TEXT NOT NULL DEFAULT '',
			encryption_key_id TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(queue_id) REFERENCES queues(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_queue_visible ON messages(queue_id, deleted_at, available_at, visibility_deadline, sent_at)`,
		`CREATE TABLE IF NOT EXISTS receipts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_id INTEGER NOT NULL,
			message_row_id INTEGER NOT NULL,
			handle TEXT NOT NULL UNIQUE,
			issued_at INTEGER NOT NULL,
			visibility_deadline INTEGER NOT NULL,
			active INTEGER NOT NULL,
			receive_attempt_id TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(queue_id) REFERENCES queues(id) ON DELETE CASCADE,
			FOREIGN KEY(message_row_id) REFERENCES messages(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS dedup_ledger (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_id INTEGER NOT NULL,
			scope_key TEXT NOT NULL,
			dedup_id TEXT NOT NULL,
			response_json TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			UNIQUE(queue_id, scope_key, dedup_id),
			FOREIGN KEY(queue_id) REFERENCES queues(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS receive_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_id INTEGER NOT NULL,
			attempt_id TEXT NOT NULL,
			response_json TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			invalidated_at INTEGER,
			UNIQUE(queue_id, attempt_id),
			FOREIGN KEY(queue_id) REFERENCES queues(id) ON DELETE CASCADE
		)`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ApplyDueQueueMutations(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id, queue_id, attributes_json FROM pending_queue_attributes WHERE effective_at <= ? ORDER BY effective_at, id`, now.Unix())
	if err != nil {
		return err
	}
	defer rows.Close()

	type duePatch struct {
		id      int64
		queueID int64
		attrs   map[string]string
	}
	var patches []duePatch
	for rows.Next() {
		var patch duePatch
		var raw string
		if err := rows.Scan(&patch.id, &patch.queueID, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &patch.attrs); err != nil {
			return err
		}
		patches = append(patches, patch)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, patch := range patches {
		queue, ok, err := s.queueByIDTx(ctx, tx, patch.queueID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for key, value := range patch.attrs {
			queue.Attributes[key] = value
		}
		queue.LastModifiedAt = now.UTC()
		if err := updateQueueTx(ctx, tx, queue); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_queue_attributes WHERE id = ?`, patch.id); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) InsertQueue(ctx context.Context, queue storage.Queue) (storage.Queue, error) {
	rawAttrs, err := json.Marshal(queue.Attributes)
	if err != nil {
		return storage.Queue{}, err
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO queues (
			account_id, region, name, url, arn, fifo, attributes_json,
			created_at, available_at, last_modified_at, deleted_at, reuse_blocked_till,
			purge_requested_at, purge_expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		queue.AccountID,
		queue.Region,
		queue.Name,
		queue.URL,
		queue.ARN,
		boolToInt(queue.FIFO),
		string(rawAttrs),
		queue.CreatedAt.Unix(),
		queue.AvailableAt.Unix(),
		queue.LastModifiedAt.Unix(),
		nullUnix(queue.DeletedAt),
		nullUnix(queue.ReuseBlockedTill),
		nullUnix(queue.PurgeRequestedAt),
		nullUnix(queue.PurgeExpiresAt),
	)
	if err != nil {
		return storage.Queue{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return storage.Queue{}, err
	}
	queue.ID = id
	return queue, nil
}

func (s *Store) UpdateQueue(ctx context.Context, queue storage.Queue) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := updateQueueTx(ctx, tx, queue); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteQueueRow(ctx context.Context, queueID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM queues WHERE id = ?`, queueID)
	return err
}

func updateQueueTx(ctx context.Context, tx *sql.Tx, queue storage.Queue) error {
	rawAttrs, err := json.Marshal(queue.Attributes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE queues
		SET account_id = ?, region = ?, name = ?, url = ?, arn = ?, fifo = ?, attributes_json = ?,
			created_at = ?, available_at = ?, last_modified_at = ?, deleted_at = ?, reuse_blocked_till = ?,
			purge_requested_at = ?, purge_expires_at = ?
		WHERE id = ?
	`,
		queue.AccountID,
		queue.Region,
		queue.Name,
		queue.URL,
		queue.ARN,
		boolToInt(queue.FIFO),
		string(rawAttrs),
		queue.CreatedAt.Unix(),
		queue.AvailableAt.Unix(),
		queue.LastModifiedAt.Unix(),
		nullUnix(queue.DeletedAt),
		nullUnix(queue.ReuseBlockedTill),
		nullUnix(queue.PurgeRequestedAt),
		nullUnix(queue.PurgeExpiresAt),
		queue.ID,
	)
	return err
}

func (s *Store) QueueByName(ctx context.Context, accountID string, region string, name string) (storage.Queue, bool, error) {
	return s.scanSingleQueue(ctx, `SELECT * FROM queues WHERE account_id = ? AND region = ? AND name = ?`, accountID, region, name)
}

func (s *Store) QueueByID(ctx context.Context, id int64) (storage.Queue, bool, error) {
	return s.scanSingleQueue(ctx, `SELECT * FROM queues WHERE id = ?`, id)
}

func (s *Store) QueueByURL(ctx context.Context, queueURL string) (storage.Queue, bool, error) {
	return s.scanSingleQueue(ctx, `SELECT * FROM queues WHERE url = ?`, queueURL)
}

func (s *Store) QueueByARN(ctx context.Context, arn string) (storage.Queue, bool, error) {
	return s.scanSingleQueue(ctx, `SELECT * FROM queues WHERE arn = ?`, arn)
}

func (s *Store) queueByIDTx(ctx context.Context, tx *sql.Tx, id int64) (storage.Queue, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, account_id, region, name, url, arn, fifo, attributes_json, created_at, available_at, last_modified_at, deleted_at, reuse_blocked_till, purge_requested_at, purge_expires_at FROM queues WHERE id = ?`, id)
	queue, ok, err := scanQueueRow(row.Scan)
	return queue, ok, err
}

func (s *Store) scanSingleQueue(ctx context.Context, query string, args ...any) (storage.Queue, bool, error) {
	row := s.db.QueryRowContext(ctx, query, args...)
	return scanQueueRow(row.Scan)
}

func (s *Store) ListQueues(ctx context.Context, accountID string, region string, prefix string) ([]storage.Queue, error) {
	query := `SELECT id, account_id, region, name, url, arn, fifo, attributes_json, created_at, available_at, last_modified_at, deleted_at, reuse_blocked_till, purge_requested_at, purge_expires_at FROM queues WHERE account_id = ? AND region = ?`
	args := []any{accountID, region}
	if prefix != "" {
		query += ` AND name LIKE ?`
		args = append(args, prefix+"%")
	}
	query += ` ORDER BY name`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.Queue
	for rows.Next() {
		queue, _, err := scanQueueRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, queue)
	}
	return out, rows.Err()
}

func (s *Store) SavePendingAttributes(ctx context.Context, queueID int64, attrs map[string]string, effectiveAt time.Time, submittedAt time.Time) error {
	raw, err := json.Marshal(attrs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO pending_queue_attributes(queue_id, attributes_json, effective_at, submitted_at) VALUES (?, ?, ?, ?)`,
		queueID, string(raw), effectiveAt.Unix(), submittedAt.Unix())
	return err
}

func (s *Store) ListPendingAttributes(ctx context.Context, queueID int64) ([]storage.PendingAttributePatch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, attributes_json, effective_at, submitted_at FROM pending_queue_attributes WHERE queue_id = ? ORDER BY effective_at, id`, queueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.PendingAttributePatch
	for rows.Next() {
		var patch storage.PendingAttributePatch
		var raw string
		var effectiveAt int64
		var submittedAt int64
		if err := rows.Scan(&patch.ID, &raw, &effectiveAt, &submittedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &patch.Attributes); err != nil {
			return nil, err
		}
		patch.QueueID = queueID
		patch.EffectiveAt = time.Unix(effectiveAt, 0).UTC()
		patch.SubmittedAt = time.Unix(submittedAt, 0).UTC()
		out = append(out, patch)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceTags(ctx context.Context, queueID int64, tags map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM queue_tags WHERE queue_id = ?`, queueID); err != nil {
		return err
	}
	for key, value := range tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO queue_tags(queue_id, tag_key, tag_value) VALUES (?, ?, ?)`, queueID, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteTags(ctx context.Context, queueID int64, keys []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM queue_tags WHERE queue_id = ? AND tag_key = ?`, queueID, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListTags(ctx context.Context, queueID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag_key, tag_value FROM queue_tags WHERE queue_id = ? ORDER BY tag_key`, queueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key string
		var value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

func (s *Store) ListQueuesByRedriveTarget(ctx context.Context, targetARN string) ([]storage.Queue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, region, name, url, arn, fifo, attributes_json, created_at, available_at, last_modified_at, deleted_at, reuse_blocked_till, purge_requested_at, purge_expires_at FROM queues`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.Queue
	for rows.Next() {
		queue, _, err := scanQueueRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		if queue.Attributes["RedrivePolicy"] == "" || !stringsContains(queue.Attributes["RedrivePolicy"], targetARN) {
			continue
		}
		out = append(out, queue)
	}
	return out, rows.Err()
}

func (s *Store) InsertMoveTask(ctx context.Context, task storage.MoveTask) (storage.MoveTask, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO message_move_tasks(task_handle, source_queue_id, destination_queue_id, status, max_messages_per_second, approx_messages_moved, approx_messages_to_move, failure_reason, started_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		task.TaskHandle,
		task.SourceQueueID,
		nullInt64(task.DestinationQueueID),
		task.Status,
		nullInt64(task.MaxMessagesPerSecond),
		task.ApproximateNumberOfMessagesMoved,
		nullInt64(task.ApproximateNumberOfMessagesToMove),
		task.FailureReason,
		task.StartedAt.Unix(),
		task.UpdatedAt.Unix(),
		nullUnix(task.CompletedAt),
	)
	if err != nil {
		return storage.MoveTask{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return storage.MoveTask{}, err
	}
	task.ID = id
	return task, nil
}

func (s *Store) UpdateMoveTask(ctx context.Context, task storage.MoveTask) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE message_move_tasks
		SET destination_queue_id = ?, status = ?, max_messages_per_second = ?, approx_messages_moved = ?, approx_messages_to_move = ?, failure_reason = ?, started_at = ?, updated_at = ?, completed_at = ?
		WHERE id = ?
	`,
		nullInt64(task.DestinationQueueID),
		task.Status,
		nullInt64(task.MaxMessagesPerSecond),
		task.ApproximateNumberOfMessagesMoved,
		nullInt64(task.ApproximateNumberOfMessagesToMove),
		task.FailureReason,
		task.StartedAt.Unix(),
		task.UpdatedAt.Unix(),
		nullUnix(task.CompletedAt),
		task.ID,
	)
	return err
}

func (s *Store) ListMoveTasksBySourceQueue(ctx context.Context, queueID int64, limit int) ([]storage.MoveTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_handle, source_queue_id, destination_queue_id, status, max_messages_per_second, approx_messages_moved, approx_messages_to_move, failure_reason, started_at, updated_at, completed_at FROM message_move_tasks WHERE source_queue_id = ? ORDER BY started_at DESC LIMIT ?`, queueID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.MoveTask
	for rows.Next() {
		task, err := scanMoveTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

func (s *Store) FindRunningMoveTask(ctx context.Context, queueID int64) (storage.MoveTask, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_handle, source_queue_id, destination_queue_id, status, max_messages_per_second, approx_messages_moved, approx_messages_to_move, failure_reason, started_at, updated_at, completed_at FROM message_move_tasks WHERE source_queue_id = ? AND status IN ('RUNNING','CANCELLING') ORDER BY started_at DESC LIMIT 1`, queueID)
	if err != nil {
		return storage.MoveTask{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return storage.MoveTask{}, false, nil
	}
	task, err := scanMoveTask(rows)
	return task, err == nil, err
}

func scanQueueRow(scan func(dest ...any) error) (storage.Queue, bool, error) {
	var queue storage.Queue
	var rawAttrs string
	var createdAt, availableAt, lastModifiedAt int64
	var deletedAt, reuseBlockedTill, purgeRequestedAt, purgeExpiresAt sql.NullInt64
	var fifo int64
	err := scan(
		&queue.ID,
		&queue.AccountID,
		&queue.Region,
		&queue.Name,
		&queue.URL,
		&queue.ARN,
		&fifo,
		&rawAttrs,
		&createdAt,
		&availableAt,
		&lastModifiedAt,
		&deletedAt,
		&reuseBlockedTill,
		&purgeRequestedAt,
		&purgeExpiresAt,
	)
	if err == sql.ErrNoRows {
		return storage.Queue{}, false, nil
	}
	if err != nil {
		return storage.Queue{}, false, err
	}
	queue.FIFO = fifo == 1
	if err := json.Unmarshal([]byte(rawAttrs), &queue.Attributes); err != nil {
		return storage.Queue{}, false, err
	}
	queue.CreatedAt = time.Unix(createdAt, 0).UTC()
	queue.AvailableAt = time.Unix(availableAt, 0).UTC()
	queue.LastModifiedAt = time.Unix(lastModifiedAt, 0).UTC()
	queue.DeletedAt = nullableTime(deletedAt)
	queue.ReuseBlockedTill = nullableTime(reuseBlockedTill)
	queue.PurgeRequestedAt = nullableTime(purgeRequestedAt)
	queue.PurgeExpiresAt = nullableTime(purgeExpiresAt)
	return queue, true, nil
}

func scanMoveTask(scanner interface{ Scan(dest ...any) error }) (storage.MoveTask, error) {
	var task storage.MoveTask
	var destinationQueueID, maxMessagesPerSecond, approxToMove, completedAt sql.NullInt64
	var startedAt, updatedAt int64
	if err := scanner.Scan(
		&task.ID,
		&task.TaskHandle,
		&task.SourceQueueID,
		&destinationQueueID,
		&task.Status,
		&maxMessagesPerSecond,
		&task.ApproximateNumberOfMessagesMoved,
		&approxToMove,
		&task.FailureReason,
		&startedAt,
		&updatedAt,
		&completedAt,
	); err != nil {
		return storage.MoveTask{}, err
	}
	task.DestinationQueueID = nullableInt64(destinationQueueID)
	task.MaxMessagesPerSecond = nullableInt64(maxMessagesPerSecond)
	task.ApproximateNumberOfMessagesToMove = nullableInt64(approxToMove)
	task.StartedAt = time.Unix(startedAt, 0).UTC()
	task.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	task.CompletedAt = nullableTime(completedAt)
	return task, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableTime(raw sql.NullInt64) *time.Time {
	if !raw.Valid {
		return nil
	}
	value := time.Unix(raw.Int64, 0).UTC()
	return &value
}

func nullableInt64(raw sql.NullInt64) *int64 {
	if !raw.Valid {
		return nil
	}
	value := raw.Int64
	return &value
}

func nullUnix(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Unix()
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func stringsContains(raw string, needle string) bool {
	return len(raw) != 0 && len(needle) != 0 && strings.Contains(raw, needle)
}
