package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"emulator-aws-sqs/internal/storage"
)

func (s *Store) ListRunningMoveTasks(ctx context.Context) ([]storage.MoveTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_handle, source_queue_id, destination_queue_id, status, max_messages_per_second, approx_messages_moved, approx_messages_to_move, failure_reason, started_at, updated_at, completed_at FROM message_move_tasks WHERE status IN ('RUNNING','CANCELLING') ORDER BY started_at`)
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

func (s *Store) InsertMessage(ctx context.Context, message storage.Message) (storage.Message, error) {
	rawAttrs, err := json.Marshal(message.MessageAttributes)
	if err != nil {
		return storage.Message{}, err
	}
	rawSystem, err := json.Marshal(message.SystemAttributes)
	if err != nil {
		return storage.Message{}, err
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO messages(
			queue_id, message_id, body, body_md5, attributes_json, system_attributes_json,
			sent_at, original_sent_at, first_received_at, receive_count, available_at,
			visibility_deadline, retention_deadline, deleted_at, group_id, dedup_id,
			sequence_number, dead_letter_source_arn, encryption_key_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		message.QueueID,
		message.MessageID,
		message.Body,
		message.BodyMD5,
		string(rawAttrs),
		string(rawSystem),
		message.SentAt.UnixMilli(),
		message.OriginalSentAt.UnixMilli(),
		nullUnixMilli(message.FirstReceivedAt),
		message.ReceiveCount,
		message.AvailableAt.UnixMilli(),
		nullUnixMilli(message.VisibilityDeadline),
		message.RetentionDeadline.UnixMilli(),
		nullUnixMilli(message.DeletedAt),
		message.GroupID,
		message.DedupID,
		message.SequenceNumber,
		message.DeadLetterSourceARN,
		message.EncryptionKeyID,
	)
	if err != nil {
		return storage.Message{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return storage.Message{}, err
	}
	message.RowID = id
	return message, nil
}

func (s *Store) UpdateMessage(ctx context.Context, message storage.Message) error {
	rawAttrs, err := json.Marshal(message.MessageAttributes)
	if err != nil {
		return err
	}
	rawSystem, err := json.Marshal(message.SystemAttributes)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE messages
		SET queue_id = ?, message_id = ?, body = ?, body_md5 = ?, attributes_json = ?, system_attributes_json = ?,
			sent_at = ?, original_sent_at = ?, first_received_at = ?, receive_count = ?, available_at = ?,
			visibility_deadline = ?, retention_deadline = ?, deleted_at = ?, group_id = ?, dedup_id = ?,
			sequence_number = ?, dead_letter_source_arn = ?, encryption_key_id = ?
		WHERE id = ?
	`,
		message.QueueID,
		message.MessageID,
		message.Body,
		message.BodyMD5,
		string(rawAttrs),
		string(rawSystem),
		message.SentAt.UnixMilli(),
		message.OriginalSentAt.UnixMilli(),
		nullUnixMilli(message.FirstReceivedAt),
		message.ReceiveCount,
		message.AvailableAt.UnixMilli(),
		nullUnixMilli(message.VisibilityDeadline),
		message.RetentionDeadline.UnixMilli(),
		nullUnixMilli(message.DeletedAt),
		message.GroupID,
		message.DedupID,
		message.SequenceNumber,
		message.DeadLetterSourceARN,
		message.EncryptionKeyID,
		message.RowID,
	)
	return err
}

func (s *Store) MessageByRowID(ctx context.Context, rowID int64) (storage.Message, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, queue_id, message_id, body, body_md5, attributes_json, system_attributes_json, sent_at, original_sent_at, first_received_at, receive_count, available_at, visibility_deadline, retention_deadline, deleted_at, group_id, dedup_id, sequence_number, dead_letter_source_arn, encryption_key_id FROM messages WHERE id = ?`, rowID)
	return scanMessage(row.Scan)
}

func (s *Store) ListMessagesByQueue(ctx context.Context, queueID int64) ([]storage.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, queue_id, message_id, body, body_md5, attributes_json, system_attributes_json, sent_at, original_sent_at, first_received_at, receive_count, available_at, visibility_deadline, retention_deadline, deleted_at, group_id, dedup_id, sequence_number, dead_letter_source_arn, encryption_key_id FROM messages WHERE queue_id = ? ORDER BY sent_at, id`, queueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.Message
	for rows.Next() {
		message, _, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMessagesByQueueBefore(ctx context.Context, queueID int64, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET deleted_at = ? WHERE queue_id = ? AND deleted_at IS NULL AND sent_at <= ?`, cutoff.UnixMilli(), queueID, cutoff.UnixMilli())
	return err
}

func (s *Store) DeleteMessagesByQueue(ctx context.Context, queueID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET deleted_at = ? WHERE queue_id = ? AND deleted_at IS NULL`, time.Now().UTC().UnixMilli(), queueID)
	return err
}

func (s *Store) InsertReceipt(ctx context.Context, receipt storage.Receipt) (storage.Receipt, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO receipts(queue_id, message_row_id, handle, issued_at, visibility_deadline, active, receive_attempt_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		receipt.QueueID,
		receipt.MessageRowID,
		receipt.Handle,
		receipt.IssuedAt.UnixMilli(),
		receipt.VisibilityDeadline.UnixMilli(),
		boolToInt(receipt.Active),
		receipt.ReceiveAttemptID,
	)
	if err != nil {
		return storage.Receipt{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return storage.Receipt{}, err
	}
	receipt.ID = id
	return receipt, nil
}

func (s *Store) UpdateReceipt(ctx context.Context, receipt storage.Receipt) error {
	_, err := s.db.ExecContext(ctx, `UPDATE receipts SET queue_id = ?, message_row_id = ?, handle = ?, issued_at = ?, visibility_deadline = ?, active = ?, receive_attempt_id = ? WHERE id = ?`,
		receipt.QueueID,
		receipt.MessageRowID,
		receipt.Handle,
		receipt.IssuedAt.UnixMilli(),
		receipt.VisibilityDeadline.UnixMilli(),
		boolToInt(receipt.Active),
		receipt.ReceiveAttemptID,
		receipt.ID,
	)
	return err
}

func (s *Store) ReceiptByHandle(ctx context.Context, handle string) (storage.Receipt, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, queue_id, message_row_id, handle, issued_at, visibility_deadline, active, receive_attempt_id FROM receipts WHERE handle = ?`, handle)
	return scanReceipt(row.Scan)
}

func (s *Store) DeactivateReceiptsByMessage(ctx context.Context, messageRowID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE receipts SET active = 0 WHERE message_row_id = ? AND active = 1`, messageRowID)
	return err
}

func (s *Store) InsertDedupEntry(ctx context.Context, entry storage.DedupEntry) (storage.DedupEntry, error) {
	raw, err := json.Marshal(entry.Response)
	if err != nil {
		return storage.DedupEntry{}, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO dedup_ledger(queue_id, scope_key, dedup_id, response_json, expires_at) VALUES (?, ?, ?, ?, ?)`,
		entry.QueueID, entry.ScopeKey, entry.DedupID, string(raw), entry.ExpiresAt.UnixMilli())
	if err != nil {
		return storage.DedupEntry{}, err
	}
	id, _ := result.LastInsertId()
	entry.ID = id
	return entry, nil
}

func (s *Store) DedupEntry(ctx context.Context, queueID int64, scopeKey, dedupID string, now time.Time) (storage.DedupEntry, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, queue_id, scope_key, dedup_id, response_json, expires_at FROM dedup_ledger WHERE queue_id = ? AND scope_key = ? AND dedup_id = ? AND expires_at > ?`, queueID, scopeKey, dedupID, now.UnixMilli())
	var entry storage.DedupEntry
	var raw string
	var expiresAt int64
	err := row.Scan(&entry.ID, &entry.QueueID, &entry.ScopeKey, &entry.DedupID, &raw, &expiresAt)
	if err == sql.ErrNoRows {
		return storage.DedupEntry{}, false, nil
	}
	if err != nil {
		return storage.DedupEntry{}, false, err
	}
	if err := json.Unmarshal([]byte(raw), &entry.Response); err != nil {
		return storage.DedupEntry{}, false, err
	}
	entry.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	return entry, true, nil
}

func (s *Store) DeleteExpiredDedupEntries(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dedup_ledger WHERE expires_at <= ?`, now.UnixMilli())
	return err
}

func (s *Store) InsertReceiveAttempt(ctx context.Context, attempt storage.ReceiveAttempt) (storage.ReceiveAttempt, error) {
	raw, err := json.Marshal(attempt.Response)
	if err != nil {
		return storage.ReceiveAttempt{}, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO receive_attempts(queue_id, attempt_id, response_json, expires_at, invalidated_at) VALUES (?, ?, ?, ?, ?)`,
		attempt.QueueID, attempt.AttemptID, string(raw), attempt.ExpiresAt.UnixMilli(), nullUnixMilli(attempt.InvalidatedAt))
	if err != nil {
		return storage.ReceiveAttempt{}, err
	}
	id, _ := result.LastInsertId()
	attempt.ID = id
	return attempt, nil
}

func (s *Store) ReceiveAttempt(ctx context.Context, queueID int64, attemptID string, now time.Time) (storage.ReceiveAttempt, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, queue_id, attempt_id, response_json, expires_at, invalidated_at FROM receive_attempts WHERE queue_id = ? AND attempt_id = ? AND expires_at > ? AND invalidated_at IS NULL`, queueID, attemptID, now.UnixMilli())
	var attempt storage.ReceiveAttempt
	var raw string
	var expiresAt int64
	var invalidatedAt sql.NullInt64
	err := row.Scan(&attempt.ID, &attempt.QueueID, &attempt.AttemptID, &raw, &expiresAt, &invalidatedAt)
	if err == sql.ErrNoRows {
		return storage.ReceiveAttempt{}, false, nil
	}
	if err != nil {
		return storage.ReceiveAttempt{}, false, err
	}
	if err := json.Unmarshal([]byte(raw), &attempt.Response); err != nil {
		return storage.ReceiveAttempt{}, false, err
	}
	attempt.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	attempt.InvalidatedAt = nullableTimeMillis(invalidatedAt)
	return attempt, true, nil
}

func (s *Store) InvalidateReceiveAttempts(ctx context.Context, queueID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE receive_attempts SET invalidated_at = ? WHERE queue_id = ? AND invalidated_at IS NULL`, time.Now().UTC().UnixMilli(), queueID)
	return err
}

func scanMessage(scan func(dest ...any) error) (storage.Message, bool, error) {
	var message storage.Message
	var rawAttrs string
	var rawSystem string
	var sentAt, originalSentAt, availableAt, retentionDeadline int64
	var firstReceivedAt, visibilityDeadline, deletedAt sql.NullInt64
	err := scan(
		&message.RowID,
		&message.QueueID,
		&message.MessageID,
		&message.Body,
		&message.BodyMD5,
		&rawAttrs,
		&rawSystem,
		&sentAt,
		&originalSentAt,
		&firstReceivedAt,
		&message.ReceiveCount,
		&availableAt,
		&visibilityDeadline,
		&retentionDeadline,
		&deletedAt,
		&message.GroupID,
		&message.DedupID,
		&message.SequenceNumber,
		&message.DeadLetterSourceARN,
		&message.EncryptionKeyID,
	)
	if err == sql.ErrNoRows {
		return storage.Message{}, false, nil
	}
	if err != nil {
		return storage.Message{}, false, err
	}
	if err := json.Unmarshal([]byte(rawAttrs), &message.MessageAttributes); err != nil {
		return storage.Message{}, false, err
	}
	if err := json.Unmarshal([]byte(rawSystem), &message.SystemAttributes); err != nil {
		return storage.Message{}, false, err
	}
	message.SentAt = time.UnixMilli(sentAt).UTC()
	message.OriginalSentAt = time.UnixMilli(originalSentAt).UTC()
	message.FirstReceivedAt = nullableTimeMillis(firstReceivedAt)
	message.AvailableAt = time.UnixMilli(availableAt).UTC()
	message.VisibilityDeadline = nullableTimeMillis(visibilityDeadline)
	message.RetentionDeadline = time.UnixMilli(retentionDeadline).UTC()
	message.DeletedAt = nullableTimeMillis(deletedAt)
	return message, true, nil
}

func scanReceipt(scan func(dest ...any) error) (storage.Receipt, bool, error) {
	var receipt storage.Receipt
	var issuedAt, visibilityDeadline int64
	var active int64
	err := scan(&receipt.ID, &receipt.QueueID, &receipt.MessageRowID, &receipt.Handle, &issuedAt, &visibilityDeadline, &active, &receipt.ReceiveAttemptID)
	if err == sql.ErrNoRows {
		return storage.Receipt{}, false, nil
	}
	if err != nil {
		return storage.Receipt{}, false, err
	}
	receipt.IssuedAt = time.UnixMilli(issuedAt).UTC()
	receipt.VisibilityDeadline = time.UnixMilli(visibilityDeadline).UTC()
	receipt.Active = active == 1
	return receipt, true, nil
}

func nullableTimeMillis(raw sql.NullInt64) *time.Time {
	if !raw.Valid {
		return nil
	}
	value := time.UnixMilli(raw.Int64).UTC()
	return &value
}

func nullUnixMilli(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().UnixMilli()
}
