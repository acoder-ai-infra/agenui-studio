package storagewrite

import (
	"context"
	"errors"
	"testing"
)

func TestMemorySinkRecordsCopies(t *testing.T) {
	sink := NewMemorySink()
	payload := map[string]any{"k": "v"}
	_, err := sink.Write(context.Background(), Write{
		Store:     StoreArtifact,
		Operation: OperationPut,
		Ref:       "artifact:1",
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	records := sink.Records()
	records[0].Ref = "mutated"
	again := sink.Records()
	if again[0].Ref != "artifact:1" {
		t.Fatalf("records should be copied: %#v", again)
	}
}

func TestMemorySinkFailureIsOperationScoped(t *testing.T) {
	sink := NewMemorySink()
	sink.Fail(OperationAppend, errors.New("append failed"))
	if _, err := sink.Write(context.Background(), Write{Store: StoreEvent, Operation: OperationAppend, Ref: "event"}); err == nil {
		t.Fatal("expected append failure")
	}
	if _, err := sink.Write(context.Background(), Write{Store: StoreRun, Operation: OperationUpdate, Ref: "run"}); err != nil {
		t.Fatalf("update should not fail: %v", err)
	}
}
