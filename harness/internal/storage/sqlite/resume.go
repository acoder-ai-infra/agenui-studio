package sqlite

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const resumeControlStatusAnswered = "answered"

type resumeStore struct {
	db     *sql.DB
	events *eventStore
}

func (s *resumeStore) Wait(ctx context.Context, cmd storage.ResumeWaitCommand) error {
	if err := validateSQLiteResumeWait(cmd); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tenantID, status string
	if err := tx.QueryRowContext(ctx, "SELECT tenant_id,status FROM runs WHERE run_id=?", cmd.Control.RunID).Scan(&tenantID, &status); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.Control.RunID)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenantID); err != nil {
		return err
	}
	var checkpointRunID, checkpointTenantID string
	if err := tx.QueryRowContext(ctx, "SELECT run_id,tenant_id FROM checkpoint_meta WHERE checkpoint_id=?", cmd.Control.CheckpointID).Scan(&checkpointRunID, &checkpointTenantID); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "checkpoint not found: "+cmd.Control.CheckpointID)
	} else if err != nil {
		return err
	}
	if checkpointRunID != cmd.Control.RunID || checkpointTenantID != tenantID || cmd.Control.TenantID != tenantID {
		return storage.NewError(storage.ErrResumeBindingMismatch, "control checkpoint binding mismatch")
	}
	var existingRunID, existingCheckpointID, existingStatus, existingTokenHash string
	err = tx.QueryRowContext(ctx, `SELECT run_id,checkpoint_id,status,resume_token_hash FROM control_requests WHERE request_id=?`, cmd.Control.RequestID).
		Scan(&existingRunID, &existingCheckpointID, &existingStatus, &existingTokenHash)
	if err == nil {
		if storage.RunStatus(status) == storage.RunStatusWaitingControl && existingRunID == cmd.Control.RunID &&
			existingCheckpointID == cmd.Control.CheckpointID && existingStatus == cmd.Control.Status &&
			sqliteConstantTimeEqual(existingTokenHash, cmd.Control.ResumeTokenHash) {
			return tx.Commit()
		}
		return storage.NewError(storage.ErrConflict, "control request already exists with different binding")
	}
	if err != sql.ErrNoRows {
		return err
	}
	if storage.RunStatus(status) != storage.RunStatusRunning {
		return storage.NewError(storage.ErrIllegalTransition, "run is not running")
	}
	if cmd.Control.CreatedAt.IsZero() {
		cmd.Control.CreatedAt = time.Now()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control_requests
		(request_id,run_id,tenant_id,checkpoint_id,type,status,resume_token_hash,tool_use_id,prompt_preview,response_ref,schema_version,created_at,expires_at,version)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,1)`, cmd.Control.RequestID, cmd.Control.RunID, tenantID, cmd.Control.CheckpointID, cmd.Control.Type, cmd.Control.Status,
		nullStr(cmd.Control.ResumeTokenHash), nullStr(cmd.Control.ToolUseID), nullStr(cmd.Control.PromptPreview), nullStr(cmd.Control.ResponseRef), cmd.Control.SchemaVersion, tsVal(cmd.Control.CreatedAt), tsVal(cmd.Control.ExpiresAt))
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET status=?,version=version+1 WHERE run_id=?", string(storage.RunStatusWaitingControl), cmd.Control.RunID); err != nil {
		return err
	}
	if _, err = s.events.appendTx(ctx, tx, tenantID, cmd.Event); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *resumeStore) Claim(ctx context.Context, cmd storage.ResumeClaimCommand) error {
	if err := validateSQLiteResumeClaim(cmd); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var sessionID, tenantID, status string
	var owner sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT session_id,tenant_id,status,resume_attempt_id
		FROM runs WHERE run_id=?`, cmd.RunID).Scan(&sessionID, &tenantID, &status, &owner); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.RunID)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenantID); err != nil {
		return err
	}
	var controlRunID, controlTenantID, controlStatus string
	var checkpointID, tokenHash sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT run_id,tenant_id,checkpoint_id,status,resume_token_hash
		FROM control_requests WHERE request_id=?`, cmd.ControlRequestID).
		Scan(&controlRunID, &controlTenantID, &checkpointID, &controlStatus, &tokenHash); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "control request not found: "+cmd.ControlRequestID)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(controlTenantID); err != nil {
		return err
	}
	if sessionID != cmd.SessionID || tenantID != controlTenantID || controlRunID != cmd.RunID ||
		checkpointID.String != cmd.CheckpointID || controlStatus != resumeControlStatusAnswered ||
		!sqliteConstantTimeEqual(tokenHash.String, cmd.ResumeTokenHash) {
		return storage.NewError(storage.ErrResumeBindingMismatch, "resume credentials do not match persisted control request")
	}

	if storage.RunStatus(status) == storage.RunStatusResuming {
		if owner.String == cmd.AttemptID {
			if existing, ok, getErr := findSQLiteResumeEvent(ctx, tx, s.events, tenantID, cmd.Event); getErr != nil {
				return getErr
			} else if ok {
				if storage.SameResumeEvent(existing, cmd.Event, cmd.AttemptID) {
					return tx.Commit()
				}
				return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
			}
		}
		return storage.NewError(storage.ErrResumeClaimHeld, "resume claim is held by another attempt")
	}
	if storage.RunStatus(status) != storage.RunStatusWaitingControl {
		return storage.NewError(storage.ErrConflict, "run is not resumable: "+status)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,resume_attempt_id=?,version=version+1 WHERE run_id=?`,
		string(storage.RunStatusResuming), cmd.AttemptID, cmd.RunID); err != nil {
		return err
	}
	result, err := s.events.appendTx(ctx, tx, tenantID, cmd.Event)
	if err != nil {
		return err
	}
	if result.Idempotent && !storage.SameResumeEvent(result.Event, cmd.Event, cmd.AttemptID) {
		return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
	}
	return tx.Commit()
}

func (s *resumeStore) Fail(ctx context.Context, cmd storage.ResumeFailureCommand) error {
	if err := validateSQLiteResumeFailure(cmd); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var persistedTenantID, status string
	var owner sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id,status,resume_attempt_id FROM runs WHERE run_id=?`, cmd.RunID).
		Scan(&persistedTenantID, &status, &owner); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.RunID)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(persistedTenantID); err != nil {
		return err
	}
	if existing, ok, getErr := findSQLiteResumeEvent(ctx, tx, s.events, persistedTenantID, cmd.Event); getErr != nil {
		return getErr
	} else if ok {
		if !storage.SameResumeEvent(existing, cmd.Event, cmd.AttemptID) {
			return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
		}
		return tx.Commit()
	}
	if storage.RunStatus(status) != storage.RunStatusResuming || owner.String != cmd.AttemptID {
		return storage.NewError(storage.ErrResumeClaimLost, "resume claim ownership lost")
	}

	to := storage.RunStatusFailed
	endedAt := tsVal(time.Now())
	errorCode, errorMessage := cmd.ErrorCode, cmd.ErrorMessage
	if cmd.Retryable {
		to, endedAt, errorCode, errorMessage = storage.RunStatusWaitingControl, 0, "", ""
	}
	// 三个终态字段按本次结果整体覆盖；可重试失败显式归零，不能保留历史错误。
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,resume_attempt_id=NULL,
		ended_at=?,error_code=?,error_message=?,version=version+1 WHERE run_id=?`,
		string(to), endedAt, nullStr(errorCode), nullStr(errorMessage), cmd.RunID); err != nil {
		return err
	}
	result, err := s.events.appendTx(ctx, tx, persistedTenantID, cmd.Event)
	if err != nil {
		return err
	}
	if result.Idempotent && !storage.SameResumeEvent(result.Event, cmd.Event, cmd.AttemptID) {
		return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
	}
	return tx.Commit()
}

func (s *resumeStore) Activate(ctx context.Context, cmd storage.ResumeActivationCommand) error {
	if cmd.RunID == "" || cmd.AttemptID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "run_id and attempt_id required")
	}
	if err := storage.ValidateResumeAttemptIdentifiers(cmd.RunID, cmd.AttemptID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tenantID, status string
	var owner sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id,status,resume_attempt_id FROM runs WHERE run_id=?`, cmd.RunID).
		Scan(&tenantID, &status, &owner); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.RunID)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenantID); err != nil {
		return err
	}
	if storage.RunStatus(status) != storage.RunStatusResuming || owner.String != cmd.AttemptID {
		return storage.NewError(storage.ErrResumeClaimLost, "resume claim ownership lost")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,resume_attempt_id=NULL,version=version+1 WHERE run_id=?`,
		string(storage.RunStatusRunning), cmd.RunID); err != nil {
		return err
	}
	return tx.Commit()
}

func validateSQLiteResumeClaim(cmd storage.ResumeClaimCommand) error {
	if cmd.RunID == "" || cmd.SessionID == "" || cmd.CheckpointID == "" || cmd.ControlRequestID == "" ||
		cmd.ResumeTokenHash == "" || cmd.AttemptID == "" || cmd.Event.EventID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "incomplete resume claim")
	}
	if err := storage.ValidateResumeClaimIdentifiers(cmd); err != nil {
		return err
	}
	if err := storage.ValidateResumeEvent(cmd.Event, cmd.RunID, cmd.AttemptID, observability.EventResumeAccepted); err != nil {
		return err
	}
	return validateSQLiteEvent(cmd.Event)
}

func validateSQLiteResumeWait(cmd storage.ResumeWaitCommand) error {
	if cmd.Control == nil || cmd.Control.RequestID == "" || cmd.Control.RunID == "" || cmd.Control.TenantID == "" || cmd.Control.CheckpointID == "" || cmd.Control.Type == "" || cmd.Control.ResumeTokenHash == "" || cmd.Event.EventID == "" || cmd.Control.Status != "pending" || cmd.Control.SchemaVersion != storage.ControlRequestSchemaVersion || cmd.Event.RunID != cmd.Control.RunID || cmd.Event.EventType != observability.EventControlRequestCreated {
		return storage.NewError(storage.ErrInvalidArgument, "invalid resume wait")
	}
	if err := storage.ValidateResumeWaitIdentifiers(cmd); err != nil {
		return err
	}
	return validateSQLiteEvent(cmd.Event)
}

func validateSQLiteResumeFailure(cmd storage.ResumeFailureCommand) error {
	if cmd.RunID == "" || cmd.AttemptID == "" || cmd.Event.EventID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "incomplete resume failure")
	}
	if err := storage.ValidateResumeFailureIdentifiers(cmd); err != nil {
		return err
	}
	if err := storage.ValidateResumeEvent(cmd.Event, cmd.RunID, cmd.AttemptID, observability.EventResumeFailed); err != nil {
		return err
	}
	return validateSQLiteEvent(cmd.Event)
}

func sqliteConstantTimeEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func findSQLiteResumeEvent(
	ctx context.Context,
	tx *sql.Tx,
	events *eventStore,
	tenantID string,
	desired observability.AgentEvent,
) (observability.AgentEvent, bool, error) {
	if existing, ok, err := events.getBy(ctx, tx, "event_id=?", desired.EventID); err != nil || ok {
		return existing, ok, err
	}
	if desired.IdempotencyKey == "" {
		return observability.AgentEvent{}, false, nil
	}
	return events.getBy(ctx, tx, "tenant_id=? AND idempotency_key=?", tenantID, desired.IdempotencyKey)
}

var _ storage.ResumeStore = (*resumeStore)(nil)
