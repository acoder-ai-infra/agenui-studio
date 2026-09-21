package protocol

import "context"

// HotStreamBuffer is the realtime channel (★ moved into Protocol from storage;
// storage-portability §3 owner = Protocol). It is short-TTL and NOT a source of
// truth: once expired, the EventStore's agent_text_delta + final_response back it
// up. Required capabilities (portability §3 HotBufferStore): ttl, ordered_append.
// P0 is an in-memory implementation; P1 is Redis.
type HotStreamBuffer interface {
	Push(ctx context.Context, runID string, delta HotDelta) error
	// Read returns deltas with DeltaSeq > afterDeltaSeq (ascending, up to limit).
	// expired is true when the buffer for runID has aged out (TTL) or the
	// requested cursor precedes what is still retained; the caller then falls
	// back to the coarse-grained EventStore after_sequence replay.
	Read(ctx context.Context, runID string, afterDeltaSeq int64, limit int) (deltas []HotDelta, expired bool, err error)
}
