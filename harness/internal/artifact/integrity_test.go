package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
)

type task4ObjectStore struct {
	backend string
	objects map[string][]byte

	putInfo  func(expected ObjectInfo) *ObjectInfo
	putErr   error
	get      func(string) (io.ReadCloser, error)
	download func(string, time.Duration) (*DownloadURL, error)

	deleteErr error

	putKeys      []string
	getKeys      []string
	deleteKeys   []string
	downloadKeys []string
}

func newTask4ObjectStore() *task4ObjectStore {
	return &task4ObjectStore{
		backend: "task4-memory",
		objects: make(map[string][]byte),
	}
}

func (s *task4ObjectStore) Backend() string {
	return s.backend
}

func (s *task4ObjectStore) Put(_ context.Context, key string, r io.Reader) (*ObjectInfo, error) {
	s.putKeys = append(s.putKeys, key)
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if s.putErr != nil {
		return nil, s.putErr
	}
	s.objects[key] = append([]byte(nil), data...)
	expected := ObjectInfo{Backend: s.Backend(), Key: key, Size: int64(len(data))}
	if s.putInfo != nil {
		return s.putInfo(expected), nil
	}
	return &expected, nil
}

func (s *task4ObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.getKeys = append(s.getKeys, key)
	if s.get != nil {
		return s.get(key)
	}
	data, ok := s.objects[key]
	if !ok {
		return nil, &Error{Code: ErrNotFound, Message: "object not found"}
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (s *task4ObjectStore) Delete(_ context.Context, key string) error {
	s.deleteKeys = append(s.deleteKeys, key)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.objects, key)
	return nil
}

func (s *task4ObjectStore) CreateDownloadURL(_ context.Context, key string, ttl time.Duration) (*DownloadURL, error) {
	s.downloadKeys = append(s.downloadKeys, key)
	if s.download != nil {
		return s.download(key, ttl)
	}
	return &DownloadURL{URL: "task4://" + key, ExpiresAt: time.Now().Add(ttl)}, nil
}

type task4MetadataStore struct {
	MetadataStore
	create              func(context.Context, ArtifactMeta, string) (*ArtifactMeta, error)
	getByRef            func(context.Context, string) (*ArtifactMeta, error)
	getByIdempotencyKey func(context.Context, string) (*ArtifactMeta, error)
	list                func(context.Context, ListQuery) ([]ArtifactMeta, error)
	markDeleted         func(context.Context, string, DeleteReason, time.Time) (*ArtifactMeta, error)
}

func (s *task4MetadataStore) Create(ctx context.Context, meta ArtifactMeta, key string) (*ArtifactMeta, error) {
	if s.create != nil {
		return s.create(ctx, meta, key)
	}
	return s.MetadataStore.Create(ctx, meta, key)
}

func (s *task4MetadataStore) GetByRef(ctx context.Context, ref string) (*ArtifactMeta, error) {
	if s.getByRef != nil {
		return s.getByRef(ctx, ref)
	}
	return s.MetadataStore.GetByRef(ctx, ref)
}

func (s *task4MetadataStore) GetByIdempotencyKey(ctx context.Context, key string) (*ArtifactMeta, error) {
	if s.getByIdempotencyKey != nil {
		return s.getByIdempotencyKey(ctx, key)
	}
	return s.MetadataStore.GetByIdempotencyKey(ctx, key)
}

func (s *task4MetadataStore) List(ctx context.Context, query ListQuery) ([]ArtifactMeta, error) {
	if s.list != nil {
		return s.list(ctx, query)
	}
	return s.MetadataStore.List(ctx, query)
}

func (s *task4MetadataStore) MarkDeleted(ctx context.Context, ref string, reason DeleteReason, at time.Time) (*ArtifactMeta, error) {
	if s.markDeleted != nil {
		return s.markDeleted(ctx, ref, reason, at)
	}
	return s.MetadataStore.MarkDeleted(ctx, ref, reason, at)
}

func newTask4Store(objects *task4ObjectStore, metadata MetadataStore) *Store {
	return NewStore(StoreConfig{
		ObjectStore:    objects,
		MetadataStore:  metadata,
		Clock:          fixedClock{now: time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)},
		MaxObjectBytes: 1 << 20,
	})
}

func task4Put(t *testing.T, objects *task4ObjectStore, metadata MetadataStore, mutate func(*PutArtifactRequest)) (*ArtifactMeta, error) {
	t.Helper()
	req := validPutRequest()
	if mutate != nil {
		mutate(&req)
	}
	return newTask4Store(objects, metadata).Put(runtimeContext(), req)
}

func assertTask4SingleCompensation(t *testing.T, objects *task4ObjectStore) {
	t.Helper()
	if len(objects.putKeys) != 1 {
		t.Fatalf("put keys = %v, want exactly one", objects.putKeys)
	}
	if !reflect.DeepEqual(objects.deleteKeys, objects.putKeys) {
		t.Fatalf("delete keys = %v, want exact uploaded key %v", objects.deleteKeys, objects.putKeys)
	}
}

func seedTask4Artifact(t *testing.T) (*task4ObjectStore, *metastore.MemoryMetadataStore, *ArtifactMeta) {
	t.Helper()
	objects := newTask4ObjectStore()
	metadata := metastore.NewMemory()
	meta, err := task4Put(t, objects, metadata, nil)
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	return objects, metadata, meta
}

type task4ReadCloser struct {
	io.Reader
	closeErr   error
	closeCalls int
}

type task4CountingReader struct {
	data      []byte
	offset    int
	readBytes int
}

func (r *task4CountingReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	r.readBytes += n
	return n, nil
}

type task4FailingReader struct {
	err error
}

func (r task4FailingReader) Read([]byte) (int, error) {
	return 0, r.err
}

func (r *task4ReadCloser) Close() error {
	r.closeCalls++
	return r.closeErr
}

func TestStorePutRejectsNilObjectInfoAndCleansExpectedKey(t *testing.T) {
	objects := newTask4ObjectStore()
	objects.putInfo = func(ObjectInfo) *ObjectInfo { return nil }

	meta, err := task4Put(t, objects, metastore.NewMemory(), nil)
	if meta != nil {
		t.Fatalf("meta = %#v, want nil", meta)
	}
	if !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
	assertTask4SingleCompensation(t, objects)
}

func TestStorePutRejectsMismatchedObjectInfoAndCleansExpectedKey(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ObjectInfo)
	}{
		{name: "backend", mutate: func(info *ObjectInfo) { info.Backend = "unexpected" }},
		{name: "key", mutate: func(info *ObjectInfo) { info.Key = "tenants/wrong/sessions/wrong/runs/wrong/art_wrong" }},
		{name: "size", mutate: func(info *ObjectInfo) { info.Size++ }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := newTask4ObjectStore()
			var returnedKey string
			objects.putInfo = func(expected ObjectInfo) *ObjectInfo {
				got := expected
				tc.mutate(&got)
				returnedKey = got.Key
				return &got
			}

			meta, err := task4Put(t, objects, metastore.NewMemory(), nil)
			if meta != nil {
				t.Fatalf("meta = %#v, want nil", meta)
			}
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want invalid_argument", err)
			}
			assertTask4SingleCompensation(t, objects)
			if tc.name == "key" && objects.deleteKeys[0] == returnedKey {
				t.Fatalf("deleted adapter-returned wrong key %q", returnedKey)
			}
		})
	}
}

func TestStorePutRejectsNilOrMismatchedCreatedMetaAndCleansExpectedKey(t *testing.T) {
	tests := []struct {
		name     string
		create   func(ArtifactMeta) *ArtifactMeta
		wantCode ErrorCode
	}{
		{
			name:     "nil metadata",
			create:   func(ArtifactMeta) *ArtifactMeta { return nil },
			wantCode: ErrInvalidArgument,
		},
		{
			name: "normalized field",
			create: func(meta ArtifactMeta) *ArtifactMeta {
				meta.OwnerID = "different-owner"
				return &meta
			},
			wantCode: ErrConflict,
		},
		{
			name: "storage key",
			create: func(meta ArtifactMeta) *ArtifactMeta {
				meta.StorageKey = "tenants/other/sessions/sess-1/runs/run-1/" + meta.ArtifactID
				return &meta
			},
			wantCode: ErrConflict,
		},
		{
			name: "schema version",
			create: func(meta ArtifactMeta) *ArtifactMeta {
				meta.SchemaVersion = "harness.artifact_meta.v0"
				return &meta
			},
			wantCode: ErrInvalidArgument,
		},
		{
			name: "status",
			create: func(meta ArtifactMeta) *ArtifactMeta {
				meta.Status = ArtifactStatusDeleted
				meta.DeletedAt = meta.CreatedAt
				meta.DeleteReason = DeleteReasonCleanup
				return &meta
			},
			wantCode: ErrConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := newTask4ObjectStore()
			base := metastore.NewMemory()
			metadata := &task4MetadataStore{
				MetadataStore: base,
				create: func(_ context.Context, meta ArtifactMeta, _ string) (*ArtifactMeta, error) {
					return tc.create(meta), nil
				},
			}

			meta, err := task4Put(t, objects, metadata, nil)
			if meta != nil {
				t.Fatalf("meta = %#v, want nil", meta)
			}
			if !IsErrorCode(err, tc.wantCode) {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
			assertTask4SingleCompensation(t, objects)
		})
	}
}

func TestStorePutRejectsDifferentCreatedRefWithoutIdempotencyKey(t *testing.T) {
	objects := newTask4ObjectStore()
	metadata := &task4MetadataStore{
		MetadataStore: metastore.NewMemory(),
		create: func(_ context.Context, meta ArtifactMeta, _ string) (*ArtifactMeta, error) {
			meta.ArtifactID = "art_alternate"
			meta.ArtifactRef = BuildRef(meta.TenantID, meta.SessionID, meta.RunID, meta.ArtifactID)
			meta.StorageKey = "tenants/" + meta.TenantID + "/sessions/" + meta.SessionID + "/runs/" + meta.RunID + "/" + meta.ArtifactID
			return &meta, nil
		},
	}

	meta, err := task4Put(t, objects, metadata, nil)
	if meta != nil {
		t.Fatalf("meta = %#v, want nil", meta)
	}
	if !IsErrorCode(err, ErrConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	assertTask4SingleCompensation(t, objects)
}

func TestStorePutRejectsCreateMutationThroughAliasedCollectionsAndCleansExpectedKey(t *testing.T) {
	objects := newTask4ObjectStore()
	metadata := &task4MetadataStore{
		MetadataStore: metastore.NewMemory(),
		create: func(_ context.Context, meta ArtifactMeta, _ string) (*ArtifactMeta, error) {
			meta.Metadata["source"] = "adapter mutation"
			keys := meta.Preview.Fields["keys"].([]string)
			keys[0] = "tampered"
			return &meta, nil
		},
	}

	meta, err := task4Put(t, objects, metadata, func(req *PutArtifactRequest) {
		req.Metadata = map[string]string{"source": "request"}
	})
	if meta != nil {
		t.Fatalf("meta = %#v, want nil", meta)
	}
	if !IsErrorCode(err, ErrConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	assertTask4SingleCompensation(t, objects)
}

func TestStorePutCreateIsolationPreservesNonNilEmptyPreviewKeys(t *testing.T) {
	objects := newTask4ObjectStore()
	meta, err := task4Put(t, objects, metastore.NewMemory(), func(req *PutArtifactRequest) {
		req.Content = strings.NewReader(`{}`)
	})
	if err != nil {
		t.Fatalf("Put(): %v", err)
	}
	keys, ok := meta.Preview.Fields["keys"].([]string)
	if !ok || keys == nil || len(keys) != 0 {
		t.Fatalf("preview keys = %#v, want non-nil empty []string", meta.Preview.Fields["keys"])
	}
}

func TestStorePutCleansExactExpectedKeyWhenMetadataCreateFails(t *testing.T) {
	objects := newTask4ObjectStore()
	primary := errors.New("metadata write failed")
	metadata := &task4MetadataStore{
		MetadataStore: metastore.NewMemory(),
		create: func(context.Context, ArtifactMeta, string) (*ArtifactMeta, error) {
			return nil, primary
		},
	}

	meta, err := task4Put(t, objects, metadata, nil)
	if meta != nil {
		t.Fatalf("meta = %#v, want nil", meta)
	}
	if !errors.Is(err, primary) {
		t.Fatalf("err = %v, want metadata cause", err)
	}
	assertTask4SingleCompensation(t, objects)
}

func TestStorePutJoinsPrimaryAndCleanupErrors(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*task4ObjectStore, *task4MetadataStore) error
		wantCode  ErrorCode
	}{
		{
			name: "metadata conflict",
			configure: func(_ *task4ObjectStore, metadata *task4MetadataStore) error {
				cause := errors.New("metadata conflict cause")
				metadata.create = func(context.Context, ArtifactMeta, string) (*ArtifactMeta, error) {
					return nil, &Error{Code: ErrConflict, Message: "metadata conflict", Err: cause}
				}
				return cause
			},
			wantCode: ErrConflict,
		},
		{
			name: "invalid object info",
			configure: func(objects *task4ObjectStore, _ *task4MetadataStore) error {
				objects.putInfo = func(ObjectInfo) *ObjectInfo { return nil }
				return nil
			},
			wantCode: ErrInvalidArgument,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := newTask4ObjectStore()
			cleanupCause := errors.New("object cleanup failed")
			objects.deleteErr = cleanupCause
			metadata := &task4MetadataStore{MetadataStore: metastore.NewMemory()}
			primaryCause := tc.configure(objects, metadata)

			meta, err := task4Put(t, objects, metadata, nil)
			if meta != nil {
				t.Fatalf("meta = %#v, want nil", meta)
			}
			if !IsErrorCode(err, tc.wantCode) {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
			if primaryCause != nil && !errors.Is(err, primaryCause) {
				t.Fatalf("err = %v, want primary cause %v", err, primaryCause)
			}
			if !errors.Is(err, cleanupCause) {
				t.Fatalf("err = %v, want cleanup cause", err)
			}
			assertTask4SingleCompensation(t, objects)
		})
	}
}

func TestStoreRejectsNilMetadataSuccess(t *testing.T) {
	ref := BuildRef("tenant-a", "sess-1", "run-1", "art_nil")
	tests := []struct {
		name string
		call func(*Store) error
	}{
		{
			name: "get",
			call: func(store *Store) error {
				_, err := store.Get(runtimeContext(), ref, GetOptions{Purpose: PurposeView})
				return err
			},
		},
		{
			name: "head",
			call: func(store *Store) error {
				_, err := store.Head(runtimeContext(), ref)
				return err
			},
		},
		{
			name: "delete",
			call: func(store *Store) error {
				return store.Delete(runtimeContext(), ref, DeleteReasonUser)
			},
		},
		{
			name: "download URL",
			call: func(store *Store) error {
				_, err := store.CreateDownloadURL(runtimeContext(), ref, DownloadURLOptions{})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := newTask4ObjectStore()
			metadata := &task4MetadataStore{
				MetadataStore: metastore.NewMemory(),
				getByRef: func(context.Context, string) (*ArtifactMeta, error) {
					return nil, nil
				},
			}
			store := newTask4Store(objects, metadata)

			err, panicValue := callWithoutPanic(func() error { return tc.call(store) })
			if panicValue != nil {
				t.Fatalf("call panicked: %v", panicValue)
			}
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want invalid_argument", err)
			}
			if len(objects.getKeys) != 0 || len(objects.deleteKeys) != 0 || len(objects.downloadKeys) != 0 {
				t.Fatalf("object calls after malformed metadata: get=%v delete=%v download=%v", objects.getKeys, objects.deleteKeys, objects.downloadKeys)
			}
		})
	}
}

func TestStoreRejectsEmptyRequestedRefBeforeMetadataLookup(t *testing.T) {
	for _, op := range refOperations {
		t.Run(op.name, func(t *testing.T) {
			objects, base, seeded := seedTask4Artifact(t)
			getByRefCalls := 0
			markDeletedCalls := 0
			metadata := &task4MetadataStore{
				MetadataStore: base,
				getByRef: func(context.Context, string) (*ArtifactMeta, error) {
					getByRefCalls++
					copyMeta := *seeded
					return &copyMeta, nil
				},
				markDeleted: func(context.Context, string, DeleteReason, time.Time) (*ArtifactMeta, error) {
					markDeletedCalls++
					return nil, nil
				},
			}
			err := op.call(newTask4Store(objects, metadata), actorContext(), "")
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("error=%v want invalid_argument", err)
			}
			if getByRefCalls != 0 || markDeletedCalls != 0 || len(objects.getKeys) != 0 ||
				len(objects.deleteKeys) != 0 || len(objects.downloadKeys) != 0 {
				t.Fatalf("calls after empty ref: getByRef=%d mark=%d object=%#v", getByRefCalls, markDeletedCalls, objects)
			}
		})
	}
}

func TestStoreRejectsMalformedStoredLifecycleTuple(t *testing.T) {
	at := time.Date(2026, time.July, 11, 13, 0, 0, 0, time.UTC)
	mutations := []struct {
		name   string
		mutate func(*ArtifactMeta)
	}{
		{name: "deleted zero time", mutate: func(meta *ArtifactMeta) {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, DeleteReasonUser, time.Time{}
		}},
		{name: "deleted empty reason", mutate: func(meta *ArtifactMeta) {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, "", at
		}},
		{name: "deleted unknown reason", mutate: func(meta *ArtifactMeta) {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, DeleteReason("unknown"), at
		}},
		{name: "ready nonzero time", mutate: func(meta *ArtifactMeta) {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusReady, "", at
		}},
		{name: "ready reason", mutate: func(meta *ArtifactMeta) {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusReady, DeleteReasonUser, time.Time{}
		}},
	}

	for _, tc := range mutations {
		t.Run(tc.name+"/head", func(t *testing.T) {
			objects, base, seeded := seedTask4Artifact(t)
			bad := *seeded
			tc.mutate(&bad)
			metadata := &task4MetadataStore{
				MetadataStore: base,
				getByRef: func(context.Context, string) (*ArtifactMeta, error) {
					return &bad, nil
				},
			}
			meta, err := newTask4Store(objects, metadata).Head(runtimeContext(), seeded.ArtifactRef)
			if meta != nil || !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("meta=%#v err=%v", meta, err)
			}
		})

		t.Run(tc.name+"/list", func(t *testing.T) {
			objects, base, seeded := seedTask4Artifact(t)
			bad := *seeded
			tc.mutate(&bad)
			metadata := &task4MetadataStore{
				MetadataStore: base,
				list: func(context.Context, ListQuery) ([]ArtifactMeta, error) {
					return []ArtifactMeta{bad}, nil
				},
			}
			rows, err := newTask4Store(objects, metadata).List(runtimeContext(), ListQuery{IncludeDeleted: true})
			if rows != nil || !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("rows=%#v err=%v", rows, err)
			}
		})
	}

	t.Run("ready deletion field stops object Get", func(t *testing.T) {
		objects, base, seeded := seedTask4Artifact(t)
		bad := *seeded
		bad.DeletedAt = at
		metadata := &task4MetadataStore{
			MetadataStore: base,
			getByRef: func(context.Context, string) (*ArtifactMeta, error) {
				return &bad, nil
			},
		}
		getCallsBefore := len(objects.getKeys)
		obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
		if obj != nil || !IsErrorCode(err, ErrInvalidArgument) {
			t.Fatalf("obj=%#v err=%v", obj, err)
		}
		if len(objects.getKeys) != getCallsBefore {
			t.Fatalf("ObjectStore.Get called: %v", objects.getKeys[getCallsBefore:])
		}
	})

	t.Run("malformed idempotency hit stops object Put", func(t *testing.T) {
		objects, base, seeded := seedTask4Artifact(t)
		bad := *seeded
		bad.DeleteReason = DeleteReasonUser
		metadata := &task4MetadataStore{
			MetadataStore: base,
			getByIdempotencyKey: func(context.Context, string) (*ArtifactMeta, error) {
				return &bad, nil
			},
		}
		req := validPutRequest()
		req.IdempotencyKey = "lifecycle-tuple-hit"
		putCallsBefore := len(objects.putKeys)
		meta, err := newTask4Store(objects, metadata).Put(runtimeContext(), req)
		if meta != nil || !IsErrorCode(err, ErrInvalidArgument) {
			t.Fatalf("meta=%#v err=%v", meta, err)
		}
		if len(objects.putKeys) != putCallsBefore {
			t.Fatalf("ObjectStore.Put called: %v", objects.putKeys[putCallsBefore:])
		}
	})
}

func TestStoreRejectsEmptyObjectStoreBackendAgainstAuthoritativeMetadata(t *testing.T) {
	t.Run("get delete and URL", func(t *testing.T) {
		tests := []struct {
			name string
			call func(*Store, string) error
		}{
			{
				name: "get",
				call: func(store *Store, ref string) error {
					_, err := store.Get(runtimeContext(), ref, GetOptions{Purpose: PurposeView})
					return err
				},
			},
			{name: "delete", call: func(store *Store, ref string) error { return store.Delete(runtimeContext(), ref, DeleteReasonUser) }},
			{
				name: "download URL",
				call: func(store *Store, ref string) error {
					_, err := store.CreateDownloadURL(runtimeContext(), ref, DownloadURLOptions{})
					return err
				},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				objects, metadata, seeded := seedTask4Artifact(t)
				objects.backend = ""
				err := tc.call(newTask4Store(objects, metadata), seeded.ArtifactRef)
				if !IsErrorCode(err, ErrConflict) {
					t.Fatalf("err = %v, want conflict", err)
				}
				if len(objects.getKeys) != 0 || len(objects.deleteKeys) != 0 || len(objects.downloadKeys) != 0 {
					t.Fatalf("downstream calls after backend mismatch: get=%v delete=%v download=%v",
						objects.getKeys, objects.deleteKeys, objects.downloadKeys)
				}
			})
		}
	})

	t.Run("idempotency hit", func(t *testing.T) {
		objects, base, seeded := seedTask4Artifact(t)
		objects.backend = ""
		metadata := &task4MetadataStore{
			MetadataStore: base,
			getByIdempotencyKey: func(context.Context, string) (*ArtifactMeta, error) {
				copyMeta := *seeded
				return &copyMeta, nil
			},
		}
		putCallsBefore := len(objects.putKeys)
		_, err := task4Put(t, objects, metadata, func(req *PutArtifactRequest) { req.IdempotencyKey = "same" })
		if !IsErrorCode(err, ErrConflict) {
			t.Fatalf("err = %v, want conflict", err)
		}
		if len(objects.putKeys) != putCallsBefore {
			t.Fatalf("object Put called after backend mismatch")
		}
	})

	t.Run("lineage", func(t *testing.T) {
		objects, metadata, seeded := seedTask4Artifact(t)
		objects.backend = ""
		putCallsBefore := len(objects.putKeys)
		_, err := task4Put(t, objects, metadata, func(req *PutArtifactRequest) {
			req.DerivedFrom = []ArtifactLineage{{ArtifactRef: seeded.ArtifactRef, Relation: LineageReferenced}}
		})
		if !IsErrorCode(err, ErrConflict) {
			t.Fatalf("err = %v, want conflict", err)
		}
		if len(objects.putKeys) != putCallsBefore {
			t.Fatalf("object Put called after lineage backend mismatch")
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		objects, base, seeded := seedTask4Artifact(t)
		objects.backend = ""
		expired := *seeded
		expired.ExpiresAt = time.Date(2026, time.July, 10, 12, 0, 0, 0, time.UTC)
		markDeletedCalls := 0
		metadata := &task4MetadataStore{
			MetadataStore: base,
			list: func(context.Context, ListQuery) ([]ArtifactMeta, error) {
				return []ArtifactMeta{expired}, nil
			},
			markDeleted: func(_ context.Context, _ string, reason DeleteReason, at time.Time) (*ArtifactMeta, error) {
				markDeletedCalls++
				deleted := expired
				deleted.Status, deleted.DeleteReason, deleted.DeletedAt = ArtifactStatusDeleted, reason, at
				return &deleted, nil
			},
		}
		auditCtx := ContextWithActor(context.Background(), Actor{
			TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorAudit,
		})
		result, err := newTask4Store(objects, metadata).CleanupExpired(auditCtx, time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC))
		if result == nil || !IsErrorCode(err, ErrConflict) {
			t.Fatalf("result = %#v, err = %v, want conflict", result, err)
		}
		if markDeletedCalls != 0 || len(objects.deleteKeys) != 0 {
			t.Fatalf("cleanup mutated state after backend mismatch: mark=%d delete=%v", markDeletedCalls, objects.deleteKeys)
		}
	})
}

func TestStoreRejectsMalformedIdempotencySuccess(t *testing.T) {
	tests := []struct {
		name   string
		result func(t *testing.T) (*ArtifactMeta, *task4ObjectStore)
	}{
		{
			name: "nil",
			result: func(t *testing.T) (*ArtifactMeta, *task4ObjectStore) {
				return nil, newTask4ObjectStore()
			},
		},
		{
			name: "malformed before semantic comparison",
			result: func(t *testing.T) (*ArtifactMeta, *task4ObjectStore) {
				objects, _, meta := seedTask4Artifact(t)
				bad := *meta
				bad.Hash = "not-a-sha256"
				bad.OwnerID = "different-owner"
				return &bad, objects
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, objects := tc.result(t)
			putCallsBefore := len(objects.putKeys)
			metadata := &task4MetadataStore{
				MetadataStore: metastore.NewMemory(),
				getByIdempotencyKey: func(context.Context, string) (*ArtifactMeta, error) {
					return result, nil
				},
			}

			_, err, panicValue := func() (*ArtifactMeta, error, any) {
				var meta *ArtifactMeta
				var callErr error
				_, panicValue := callWithoutPanic(func() error {
					var err error
					meta, err = task4Put(t, objects, metadata, func(req *PutArtifactRequest) {
						req.IdempotencyKey = "same-key"
					})
					callErr = err
					return err
				})
				return meta, callErr, panicValue
			}()
			if panicValue != nil {
				t.Fatalf("put panicked: %v", panicValue)
			}
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want invalid_argument", err)
			}
			if len(objects.putKeys) != putCallsBefore {
				t.Fatalf("object Put called on malformed idempotency hit: before=%d after=%d", putCallsBefore, len(objects.putKeys))
			}
		})
	}
}

func TestStoreRejectsMismatchedMetadata(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*ArtifactMeta)
		wantCode ErrorCode
	}{
		{name: "artifact ref", mutate: func(meta *ArtifactMeta) {
			meta.ArtifactRef = BuildRef(meta.TenantID, meta.SessionID, meta.RunID, "art_other")
		}, wantCode: ErrConflict},
		{name: "artifact id", mutate: func(meta *ArtifactMeta) { meta.ArtifactID = "art_other" }, wantCode: ErrConflict},
		{name: "tenant", mutate: func(meta *ArtifactMeta) { meta.TenantID = "tenant-other" }, wantCode: ErrConflict},
		{name: "session", mutate: func(meta *ArtifactMeta) { meta.SessionID = "sess-other" }, wantCode: ErrConflict},
		{name: "run", mutate: func(meta *ArtifactMeta) { meta.RunID = "run-other" }, wantCode: ErrConflict},
		{name: "schema", mutate: func(meta *ArtifactMeta) { meta.SchemaVersion = "v0" }, wantCode: ErrInvalidArgument},
		{name: "enum", mutate: func(meta *ArtifactMeta) { meta.OwnerModule = OwnerModule("unknown") }, wantCode: ErrInvalidArgument},
		{name: "mime", mutate: func(meta *ArtifactMeta) { meta.MimeType = "not a mime" }, wantCode: ErrInvalidArgument},
		{name: "hash", mutate: func(meta *ArtifactMeta) { meta.Hash = "sha256:short" }, wantCode: ErrInvalidArgument},
		{name: "size", mutate: func(meta *ArtifactMeta) { meta.SizeBytes = -1 }, wantCode: ErrInvalidArgument},
		{name: "storage key", mutate: func(meta *ArtifactMeta) { meta.StorageKey += "/other" }, wantCode: ErrConflict},
		{name: "storage backend", mutate: func(meta *ArtifactMeta) { meta.StorageBackend = "other" }, wantCode: ErrConflict},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects, base, seeded := seedTask4Artifact(t)
			bad := *seeded
			tc.mutate(&bad)
			metadata := &task4MetadataStore{
				MetadataStore: base,
				getByRef: func(context.Context, string) (*ArtifactMeta, error) {
					return &bad, nil
				},
			}
			getCallsBefore := len(objects.getKeys)

			_, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
			if !IsErrorCode(err, tc.wantCode) {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
			if len(objects.getKeys) != getCallsBefore {
				t.Fatalf("object Get called after malformed metadata")
			}
		})
	}
}

func TestStoreRejectsMalformedListRows(t *testing.T) {
	objects, base, seeded := seedTask4Artifact(t)
	bad := *seeded
	bad.Hash = "invalid"
	metadata := &task4MetadataStore{
		MetadataStore: base,
		list: func(context.Context, ListQuery) ([]ArtifactMeta, error) {
			return []ArtifactMeta{bad}, nil
		},
	}

	metas, err := newTask4Store(objects, metadata).List(runtimeContext(), ListQuery{})
	if metas != nil {
		t.Fatalf("metas = %#v, want nil", metas)
	}
	if !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

func TestStoreAllowsNilMetadataListAsEmpty(t *testing.T) {
	objects := newTask4ObjectStore()
	metadata := &task4MetadataStore{
		MetadataStore: metastore.NewMemory(),
		list: func(context.Context, ListQuery) ([]ArtifactMeta, error) {
			return nil, nil
		},
	}

	metas, err := newTask4Store(objects, metadata).List(runtimeContext(), ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("metas = %#v, want empty", metas)
	}
}

func TestStoreRejectsNilObjectReaderSuccess(t *testing.T) {
	objects, metadata, seeded := seedTask4Artifact(t)
	objects.get = func(string) (io.ReadCloser, error) { return nil, nil }

	obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
	if obj != nil {
		t.Fatalf("object = %#v, want nil", obj)
	}
	if !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

func TestStoreGetClosesReaderReturnedWithAdapterError(t *testing.T) {
	objects, metadata, seeded := seedTask4Artifact(t)
	adapterCause := errors.New("object get failed")
	closeCause := errors.New("reader close failed")
	reader := &task4ReadCloser{Reader: bytes.NewReader(nil), closeErr: closeCause}
	objects.get = func(string) (io.ReadCloser, error) { return reader, adapterCause }

	obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
	if obj != nil {
		t.Fatalf("object = %#v, want nil", obj)
	}
	if !errors.Is(err, adapterCause) || !errors.Is(err, closeCause) {
		t.Fatalf("err = %v, want adapter and close causes", err)
	}
	if reader.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", reader.closeCalls)
	}
}

func TestStoreRejectsNilDownloadURLSuccess(t *testing.T) {
	objects, metadata, seeded := seedTask4Artifact(t)
	objects.download = func(string, time.Duration) (*DownloadURL, error) { return nil, nil }

	url, err := newTask4Store(objects, metadata).CreateDownloadURL(runtimeContext(), seeded.ArtifactRef, DownloadURLOptions{})
	if url != nil {
		t.Fatalf("url = %#v, want nil", url)
	}
	if !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

func TestStoreRejectsMalformedDeleteResult(t *testing.T) {
	tests := []struct {
		name     string
		result   func(ArtifactMeta, DeleteReason, time.Time) *ArtifactMeta
		wantCode ErrorCode
	}{
		{name: "nil", result: func(ArtifactMeta, DeleteReason, time.Time) *ArtifactMeta { return nil }, wantCode: ErrInvalidArgument},
		{name: "ref", result: func(meta ArtifactMeta, reason DeleteReason, at time.Time) *ArtifactMeta {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, reason, at
			meta.ArtifactRef = BuildRef(meta.TenantID, meta.SessionID, meta.RunID, "art_other")
			return &meta
		}, wantCode: ErrConflict},
		{name: "storage", result: func(meta ArtifactMeta, reason DeleteReason, at time.Time) *ArtifactMeta {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, reason, at
			meta.StorageKey += "/other"
			return &meta
		}, wantCode: ErrConflict},
		{name: "status", result: func(meta ArtifactMeta, reason DeleteReason, at time.Time) *ArtifactMeta {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusExpired, reason, at
			return &meta
		}, wantCode: ErrConflict},
		{name: "reason", result: func(meta ArtifactMeta, _ DeleteReason, at time.Time) *ArtifactMeta {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, DeleteReasonTTL, at
			return &meta
		}, wantCode: ErrConflict},
		{name: "deleted at", result: func(meta ArtifactMeta, reason DeleteReason, at time.Time) *ArtifactMeta {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, reason, at.Add(time.Second)
			return &meta
		}, wantCode: ErrConflict},
		{name: "immutable field", result: func(meta ArtifactMeta, reason DeleteReason, at time.Time) *ArtifactMeta {
			meta.Status, meta.DeleteReason, meta.DeletedAt = ArtifactStatusDeleted, reason, at
			meta.OwnerID = "other-owner"
			return &meta
		}, wantCode: ErrConflict},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects, base, seeded := seedTask4Artifact(t)
			deleteCallsBefore := len(objects.deleteKeys)
			metadata := &task4MetadataStore{
				MetadataStore: base,
				markDeleted: func(_ context.Context, _ string, reason DeleteReason, at time.Time) (*ArtifactMeta, error) {
					return tc.result(*seeded, reason, at), nil
				},
			}

			err, panicValue := callWithoutPanic(func() error {
				return newTask4Store(objects, metadata).Delete(runtimeContext(), seeded.ArtifactRef, DeleteReasonUser)
			})
			if panicValue != nil {
				t.Fatalf("delete panicked: %v", panicValue)
			}
			if !IsErrorCode(err, tc.wantCode) {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
			if len(objects.deleteKeys) != deleteCallsBefore {
				t.Fatalf("object Delete calls = %v, want no call for malformed result", objects.deleteKeys[deleteCallsBefore:])
			}
		})
	}
}

func TestStoreRejectsMarkDeletedMutationThroughAliasedMetadata(t *testing.T) {
	objects, base, seeded := seedTask4Artifact(t)
	shared := *seeded
	metadata := &task4MetadataStore{
		MetadataStore: base,
		getByRef: func(context.Context, string) (*ArtifactMeta, error) {
			return &shared, nil
		},
		markDeleted: func(_ context.Context, _ string, reason DeleteReason, at time.Time) (*ArtifactMeta, error) {
			shared.OwnerID = "adapter-mutated-owner"
			shared.Status, shared.DeleteReason, shared.DeletedAt = ArtifactStatusDeleted, reason, at
			return &shared, nil
		},
	}
	deleteCallsBefore := len(objects.deleteKeys)

	err := newTask4Store(objects, metadata).Delete(runtimeContext(), seeded.ArtifactRef, DeleteReasonUser)
	if !IsErrorCode(err, ErrConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	if len(objects.deleteKeys) != deleteCallsBefore {
		t.Fatalf("object Delete called after aliased metadata mutation: %v", objects.deleteKeys[deleteCallsBefore:])
	}
}

func TestStoreGetRejectsTamperedContent(t *testing.T) {
	objects, metadata, seeded := seedTask4Artifact(t)
	objects.objects[seeded.StorageKey] = bytes.Repeat([]byte("x"), int(seeded.SizeBytes))

	obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
	if obj != nil {
		t.Fatalf("object = %#v, want nil", obj)
	}
	if !IsErrorCode(err, ErrConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestStoreGetRejectsObjectSizeMismatch(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*task4ObjectStore, *ArtifactMeta) (*task4ReadCloser, *task4CountingReader)
	}{
		{
			name: "shorter",
			configure: func(objects *task4ObjectStore, meta *ArtifactMeta) (*task4ReadCloser, *task4CountingReader) {
				objects.objects[meta.StorageKey] = bytes.Repeat([]byte("s"), int(meta.SizeBytes-1))
				return nil, nil
			},
		},
		{
			name: "longer reads at most expected plus one",
			configure: func(objects *task4ObjectStore, meta *ArtifactMeta) (*task4ReadCloser, *task4CountingReader) {
				counter := &task4CountingReader{data: bytes.Repeat([]byte("l"), int(meta.SizeBytes)+1024)}
				reader := &task4ReadCloser{Reader: counter}
				objects.get = func(string) (io.ReadCloser, error) { return reader, nil }
				return reader, counter
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects, metadata, seeded := seedTask4Artifact(t)
			reader, counter := tc.configure(objects, seeded)

			obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
			if obj != nil {
				t.Fatalf("object = %#v, want nil", obj)
			}
			if !IsErrorCode(err, ErrConflict) {
				t.Fatalf("err = %v, want conflict", err)
			}
			if reader != nil && reader.closeCalls != 1 {
				t.Fatalf("backend close calls = %d, want 1", reader.closeCalls)
			}
			if counter != nil && counter.readBytes > int(seeded.SizeBytes+1) {
				t.Fatalf("read bytes = %d, want at most %d", counter.readBytes, seeded.SizeBytes+1)
			}
		})
	}
}

func TestStoreGetPreservesReadAndCloseErrors(t *testing.T) {
	tests := []struct {
		name      string
		closeErr  error
		wantClose bool
	}{
		{name: "read error closes", closeErr: nil, wantClose: false},
		{name: "read and close errors join", closeErr: errors.New("close failed"), wantClose: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects, metadata, seeded := seedTask4Artifact(t)
			readCause := errors.New("read failed")
			reader := &task4ReadCloser{Reader: task4FailingReader{err: readCause}, closeErr: tc.closeErr}
			objects.get = func(string) (io.ReadCloser, error) { return reader, nil }

			obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
			if obj != nil {
				t.Fatalf("object = %#v, want nil", obj)
			}
			if !errors.Is(err, readCause) {
				t.Fatalf("err = %v, want read cause", err)
			}
			if tc.wantClose && !errors.Is(err, tc.closeErr) {
				t.Fatalf("err = %v, want close cause", err)
			}
			if reader.closeCalls != 1 {
				t.Fatalf("close calls = %d, want 1", reader.closeCalls)
			}
		})
	}
}

func TestStoreGetRejectsCloseError(t *testing.T) {
	objects, metadata, seeded := seedTask4Artifact(t)
	closeCause := errors.New("close failed")
	reader := &task4ReadCloser{
		Reader:   bytes.NewReader(append([]byte(nil), objects.objects[seeded.StorageKey]...)),
		closeErr: closeCause,
	}
	objects.get = func(string) (io.ReadCloser, error) { return reader, nil }

	obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
	if obj != nil {
		t.Fatalf("object = %#v, want nil", obj)
	}
	if !errors.Is(err, closeCause) {
		t.Fatalf("err = %v, want close cause", err)
	}
	if reader.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", reader.closeCalls)
	}
}

func TestStoreGetClosesBackendReaderOnSuccess(t *testing.T) {
	objects, metadata, seeded := seedTask4Artifact(t)
	want := append([]byte(nil), objects.objects[seeded.StorageKey]...)
	reader := &task4ReadCloser{Reader: bytes.NewReader(want)}
	objects.get = func(string) (io.ReadCloser, error) { return reader, nil }

	obj, err := newTask4Store(objects, metadata).Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reader.closeCalls != 1 {
		t.Fatalf("backend close calls at return = %d, want 1", reader.closeCalls)
	}
	got, err := io.ReadAll(obj.Content)
	if err != nil {
		t.Fatalf("read returned buffer: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if err := obj.Content.Close(); err != nil {
		t.Fatalf("close returned buffer: %v", err)
	}
	if reader.closeCalls != 1 {
		t.Fatalf("backend close calls after returned close = %d, want 1", reader.closeCalls)
	}
}

func TestStoreGetRejectsMetadataSizeOverConfiguredLimit(t *testing.T) {
	t.Run("configured bound", func(t *testing.T) {
		objects, metadata, seeded := seedTask4Artifact(t)
		store := NewStore(StoreConfig{
			ObjectStore:    objects,
			MetadataStore:  metadata,
			Clock:          fixedClock{now: time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)},
			MaxObjectBytes: seeded.SizeBytes - 1,
		})

		obj, err := store.Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
		if obj != nil {
			t.Fatalf("object = %#v, want nil", obj)
		}
		if !IsErrorCode(err, ErrTooLarge) {
			t.Fatalf("err = %v, want too_large", err)
		}
		if len(objects.getKeys) != 0 {
			t.Fatalf("object Get calls = %v, want none", objects.getKeys)
		}
	})

	t.Run("max int cannot overflow read limit", func(t *testing.T) {
		objects, base, seeded := seedTask4Artifact(t)
		oversized := *seeded
		oversized.SizeBytes = math.MaxInt64
		metadata := &task4MetadataStore{
			MetadataStore: base,
			getByRef: func(context.Context, string) (*ArtifactMeta, error) {
				return &oversized, nil
			},
		}
		store := NewStore(StoreConfig{
			ObjectStore:    objects,
			MetadataStore:  metadata,
			Clock:          fixedClock{now: time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)},
			MaxObjectBytes: math.MaxInt64,
		})

		obj, err := store.Get(runtimeContext(), seeded.ArtifactRef, GetOptions{Purpose: PurposeView})
		if obj != nil {
			t.Fatalf("object = %#v, want nil", obj)
		}
		if !IsErrorCode(err, ErrTooLarge) {
			t.Fatalf("err = %v, want too_large", err)
		}
		if len(objects.getKeys) != 0 {
			t.Fatalf("object Get calls = %v, want none", objects.getKeys)
		}
	})
}

func TestStorePutRejectsOverflowingConfiguredReadLimit(t *testing.T) {
	objects := newTask4ObjectStore()
	store := NewStore(StoreConfig{
		ObjectStore:    objects,
		MetadataStore:  metastore.NewMemory(),
		Clock:          fixedClock{now: time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)},
		MaxObjectBytes: math.MaxInt64,
	})
	req := validPutRequest()

	meta, err := store.Put(runtimeContext(), req)
	if meta != nil {
		t.Fatalf("meta = %#v, want nil", meta)
	}
	if !IsErrorCode(err, ErrTooLarge) {
		t.Fatalf("err = %v, want too_large", err)
	}
	if len(objects.putKeys) != 0 {
		t.Fatalf("object Put calls = %v, want none", objects.putKeys)
	}
}
