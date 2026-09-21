package artifact_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
)

func TestConcurrentExplicitArtifactIDNeverOverwritesOrDeletesWinnerObject(t *testing.T) {
	objects := newTrackingObjectStore()
	metadata := newBarrierRefMissMetadataStore(metastore.NewMemory(), 2)
	store := NewStore(StoreConfig{
		ObjectStore: objects, MetadataStore: metadata,
		Clock: fixedClock{now: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)}, MaxObjectBytes: 1 << 20,
	})

	type result struct {
		meta    *ArtifactMeta
		err     error
		content string
	}
	results := []result{{content: "content-a"}, {content: "content-b"}}
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := validPutRequest()
			req.ArtifactID = "artifact-explicit"
			req.IdempotencyKey = "artifact-explicit-attempt-" + string(rune('a'+i))
			req.Content = strings.NewReader(results[i].content)
			results[i].meta, results[i].err = store.Put(runtimeContext(), req)
		}(i)
	}
	wg.Wait()

	var winner *result
	var conflicts int
	for i := range results {
		if results[i].err == nil {
			winner = &results[i]
			continue
		}
		if !IsErrorCode(results[i].err, ErrConflict) {
			t.Fatalf("Put() loser error = %v, want conflict", results[i].err)
		}
		conflicts++
	}
	if winner == nil || conflicts != 1 {
		t.Fatalf("results = %#v, want one winner and one conflict", results)
	}
	putKeys := objects.PutKeys()
	if len(putKeys) != 2 || putKeys[0] == putKeys[1] {
		t.Fatalf("put keys = %#v, want unique attempt keys", putKeys)
	}
	if !task3ContainsKey(objects.LiveKeys(), winner.meta.StorageKey) {
		t.Fatalf("winner storage key %q is not live; live=%#v", winner.meta.StorageKey, objects.LiveKeys())
	}
	object, err := store.Get(runtimeContext(), winner.meta.ArtifactRef, GetOptions{Purpose: PurposeReplay})
	if err != nil {
		t.Fatalf("Get(winner) error = %v", err)
	}
	data, readErr := io.ReadAll(object.Content)
	closeErr := object.Content.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read winner = (%v, %v)", readErr, closeErr)
	}
	if string(data) != winner.content {
		t.Fatalf("winner object = %q, metadata winner content = %q", data, winner.content)
	}
}

type barrierRefMissMetadataStore struct {
	MetadataStore
	mu       sync.Mutex
	want     int
	arrived  int
	released chan struct{}
	once     sync.Once
}

func newBarrierRefMissMetadataStore(store MetadataStore, want int) *barrierRefMissMetadataStore {
	return &barrierRefMissMetadataStore{MetadataStore: store, want: want, released: make(chan struct{})}
}

func (s *barrierRefMissMetadataStore) GetByRef(ctx context.Context, ref string) (*ArtifactMeta, error) {
	s.mu.Lock()
	if s.arrived >= s.want {
		s.mu.Unlock()
		return s.MetadataStore.GetByRef(ctx, ref)
	}
	s.arrived++
	if s.arrived == s.want {
		s.once.Do(func() { close(s.released) })
	}
	released := s.released
	s.mu.Unlock()
	select {
	case <-released:
		return nil, &Error{Code: ErrNotFound, Message: "simulated concurrent ref miss"}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
