package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

func TestStoreUsesHarnessArtifactsAsOnlyRunManifest(t *testing.T) {
	client := newMemoryArtifactClient()
	store, err := NewStore(client)
	if err != nil {
		t.Fatal(err)
	}
	identity := artifactIdentity()
	pointer, err := store.Save(t.Context(), identity, StepDesign, `{"root":"card"}`)
	if err != nil {
		t.Fatal(err)
	}
	if pointer.Ref == "" {
		t.Fatal("missing artifact ref")
	}
	replay, err := store.Save(t.Context(), identity, StepDesign, `{"root":"card"}`)
	if err != nil || replay != pointer {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := store.Save(t.Context(), identity, StepDesign, `{"root":"other"}`); !errors.Is(err, ErrImmutableStepConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	loaded, err := store.Load(t.Context(), identity, StepDesign)
	if err != nil || loaded != `{"root":"card"}` {
		t.Fatalf("loaded=%q err=%v", loaded, err)
	}
	manifest, err := store.Manifest(t.Context(), identity)
	if err != nil || manifest.Validate() != nil || manifest.Design == nil || manifest.Design.Ref != pointer.Ref {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
}

func artifactIdentity() harness.Identity {
	return harness.Identity{TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run"}
}

type memoryArtifactClient struct {
	mu          sync.Mutex
	objects     map[string]memoryArtifact
	idempotency map[string]string
}
type memoryArtifact struct {
	info    harness.ArtifactInfo
	content []byte
}

func newMemoryArtifactClient() *memoryArtifactClient {
	return &memoryArtifactClient{objects: map[string]memoryArtifact{}, idempotency: map[string]string{}}
}

func (m *memoryArtifactClient) Put(_ context.Context, request harness.PutArtifactRequest) (harness.ArtifactInfo, error) {
	raw, err := io.ReadAll(request.Content)
	if err != nil {
		return harness.ArtifactInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref := m.idempotency[request.IdempotencyKey]; ref != "" {
		object := m.objects[ref]
		if !bytes.Equal(object.content, raw) {
			return harness.ArtifactInfo{}, errors.New("idempotency conflict")
		}
		return object.info, nil
	}
	sum := sha256.Sum256(raw)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	ref := "artifact://test/" + request.Identity.RunID + "/" + request.Name
	info := harness.ArtifactInfo{Ref: ref, ArtifactRef: harness.ArtifactRef{ID: request.Name, TenantID: request.Identity.TenantID, SessionID: request.Identity.SessionID, RunID: request.Identity.RunID, MIME: request.MIME, Hash: hash, Size: int64(len(raw))}, Name: request.Name, MIME: request.MIME, Kind: string(request.Kind), SizeBytes: int64(len(raw)), Hash: hash, CreatedAt: time.Now()}
	m.objects[ref] = memoryArtifact{info: info, content: raw}
	m.idempotency[request.IdempotencyKey] = ref
	return info, nil
}
func (m *memoryArtifactClient) Get(_ context.Context, request harness.GetArtifactRequest) (*harness.ArtifactContent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[request.Ref]
	if !ok {
		return nil, errors.New("not found")
	}
	return &harness.ArtifactContent{Info: object.info, Content: io.NopCloser(bytes.NewReader(object.content))}, nil
}
func (m *memoryArtifactClient) Head(_ context.Context, request harness.GetArtifactRequest) (harness.ArtifactInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[request.Ref]
	if !ok {
		return harness.ArtifactInfo{}, errors.New("not found")
	}
	return object.info, nil
}
func (m *memoryArtifactClient) List(_ context.Context, request harness.ListArtifactsRequest) (harness.ArtifactPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []harness.ArtifactInfo
	for _, object := range m.objects {
		if request.Identity.RunID == "" || object.info.ArtifactRef.RunID == request.Identity.RunID {
			items = append(items, object.info)
		}
	}
	return harness.ArtifactPage{Items: items}, nil
}
