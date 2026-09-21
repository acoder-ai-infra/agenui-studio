package context

import "errors"

var (
	ErrSessionNotFound    = errors.New("session not found")
	ErrSessionIDMissing   = errors.New("session_id required")
	ErrStatePathNotFound  = errors.New("state path not found")
	ErrStatePathForbidden = errors.New("state path write forbidden for this writer")
	ErrStateValidation    = errors.New("state value validation failed")
	ErrTimelineEmpty      = errors.New("timeline is empty")
	ErrMemoryNotFound     = errors.New("memory item not found")
	ErrSourceCollect      = errors.New("source collect failed")
	ErrBuilderBuild       = errors.New("builder build failed")
	ErrBudgetExceeded     = errors.New("token budget exceeded")
	ErrMessageIDMissing   = errors.New("message_id required")
	ErrMessageConflict    = errors.New("message idempotency conflict")
	ErrSnapshotNotFound   = errors.New("context snapshot not found")
	ErrHotContextMiss     = errors.New("hot context cache miss")
	ErrHotContextConflict = errors.New("hot context cache version conflict")
	ErrToolPairingInvalid = errors.New("tool call and result pairing invalid")
)
