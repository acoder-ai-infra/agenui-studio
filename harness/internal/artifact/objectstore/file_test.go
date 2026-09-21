package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const task4FileKey = "tenants/tenant-a/sessions/sess-1/runs/run-1/art_file"

type task4FailAfterReader struct {
	data []byte
	err  error
}

func (r *task4FailAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func newTask4FileStore(t *testing.T) (*FileObjectStore, string) {
	t.Helper()
	root := t.TempDir()
	store, err := NewFile(root, "http://artifact.test")
	if err != nil {
		t.Fatalf("new file object store: %v", err)
	}
	return store, root
}

func task4AssertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".artifact-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func task4FilesystemSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			contents, err := os.ReadFile(current)
			if err != nil {
				return err
			}
			value += "\x00" + string(contents)
		}
		snapshot[relative] = value
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %q: %v", root, err)
	}
	return snapshot
}

func task4SeedRejectedKeyCandidate(t *testing.T, base, root, key string) string {
	t.Helper()
	if strings.IndexByte(key, 0) >= 0 {
		return ""
	}
	candidate := filepath.Clean(filepath.Join(root, filepath.FromSlash(key)))
	if candidate == base || candidate == root || filepath.IsAbs(key) && candidate == filepath.Clean(key) {
		return ""
	}
	relative, err := filepath.Rel(base, candidate)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatalf("mkdir rejected-key candidate parent: %v", err)
	}
	if err := os.WriteFile(candidate, []byte("target preservation sentinel"), 0o600); err != nil {
		t.Fatalf("write rejected-key candidate sentinel: %v", err)
	}
	return candidate
}

func TestFileObjectStoreFailedPutLeavesNoFinalOrTemp(t *testing.T) {
	store, root := newTask4FileStore(t)
	cause := errors.New("source read failed")
	_, err := store.Put(context.Background(), task4FileKey, &task4FailAfterReader{
		data: []byte("partial"),
		err:  cause,
	})
	if !errors.Is(err, cause) {
		t.Fatalf("Put() error = %v, want source cause", err)
	}

	finalPath := filepath.Join(root, filepath.FromSlash(task4FileKey))
	if _, err := os.Lstat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("final path exists after failed Put: %v", err)
	}
	task4AssertNoTempFiles(t, filepath.Dir(finalPath))
}

func TestFileObjectStoreFailedOverwritePreservesFinal(t *testing.T) {
	store, root := newTask4FileStore(t)
	want := []byte("existing artifact bytes")
	if _, err := store.Put(context.Background(), task4FileKey, bytes.NewReader(want)); err != nil {
		t.Fatalf("initial Put(): %v", err)
	}

	cause := errors.New("source read failed")
	_, err := store.Put(context.Background(), task4FileKey, &task4FailAfterReader{
		data: []byte("partial replacement"),
		err:  cause,
	})
	if !errors.Is(err, cause) {
		t.Fatalf("overwrite error = %v, want source cause", err)
	}

	finalPath := filepath.Join(root, filepath.FromSlash(task4FileKey))
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read original final: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("final bytes = %q, want original %q", got, want)
	}
	task4AssertNoTempFiles(t, filepath.Dir(finalPath))
}

func TestFileObjectStoreAtomicPut(t *testing.T) {
	store, root := newTask4FileStore(t)
	tests := [][]byte{
		[]byte("first artifact"),
		[]byte("replacement"),
	}
	for _, want := range tests {
		info, err := store.Put(context.Background(), task4FileKey, bytes.NewReader(want))
		if err != nil {
			t.Fatalf("Put(%q): %v", want, err)
		}
		wantInfo := &artifact.ObjectInfo{Backend: "file", Key: task4FileKey, Size: int64(len(want))}
		if !reflect.DeepEqual(info, wantInfo) {
			t.Fatalf("ObjectInfo = %#v, want %#v", info, wantInfo)
		}

		finalPath := filepath.Join(root, filepath.FromSlash(task4FileKey))
		got, err := os.ReadFile(finalPath)
		if err != nil {
			t.Fatalf("read final: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("final bytes = %q, want %q", got, want)
		}
		stat, err := os.Stat(finalPath)
		if err != nil {
			t.Fatalf("stat final: %v", err)
		}
		if gotMode := stat.Mode().Perm(); gotMode != 0o600 {
			t.Fatalf("final mode = %04o, want 0600", gotMode)
		}
		task4AssertNoTempFiles(t, filepath.Dir(finalPath))
	}
}

func TestFileObjectStoreRejectsEscapingKey(t *testing.T) {
	tests := []struct {
		name             string
		key              string
		platformAbsolute bool
	}{
		{name: "empty"},
		{name: "absolute POSIX", key: "/absolute/object"},
		{name: "absolute platform", platformAbsolute: true},
		{name: "leading dot-dot", key: "../outside"},
		{name: "dot-dot segment", key: "a/../object"},
		{name: "dot segment", key: "a/./object"},
		{name: "duplicate slash", key: "a//object"},
		{name: "backslash", key: "a\\object"},
		{name: "NUL", key: "a/\x00/object"},
		{name: "dot", key: "."},
		{name: "dot-dot", key: ".."},
		{name: "trailing slash", key: "a/"},
	}
	operations := []struct {
		name       string
		resultName string
		call       func(*FileObjectStore, string) (bool, error)
	}{
		{
			name:       "put",
			resultName: "object info",
			call: func(store *FileObjectStore, key string) (bool, error) {
				info, err := store.Put(context.Background(), key, strings.NewReader("data"))
				return info != nil, err
			},
		},
		{
			name:       "get",
			resultName: "reader",
			call: func(store *FileObjectStore, key string) (bool, error) {
				reader, err := store.Get(context.Background(), key)
				if reader == nil {
					return false, err
				}
				_ = reader.Close()
				return true, err
			},
		},
		{
			name: "delete",
			call: func(store *FileObjectStore, key string) (bool, error) {
				return false, store.Delete(context.Background(), key)
			},
		},
		{
			name:       "create download URL",
			resultName: "download URL",
			call: func(store *FileObjectStore, key string) (bool, error) {
				url, err := store.CreateDownloadURL(context.Background(), key, time.Minute)
				return url != nil, err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, operation := range operations {
				t.Run(operation.name, func(t *testing.T) {
					base := t.TempDir()
					root := filepath.Join(base, "root")
					store, err := NewFile(root, "http://artifact.test")
					if err != nil {
						t.Fatalf("new file object store: %v", err)
					}
					key := tc.key
					platformAbsolute := filepath.Join(base, "platform-absolute")
					if tc.platformAbsolute {
						key = platformAbsolute
					}
					preservationPath := filepath.Join(root, "preservation-sentinel")
					if err := os.WriteFile(preservationPath, []byte("preserve"), 0o600); err != nil {
						t.Fatalf("write preservation sentinel: %v", err)
					}
					targetPath := task4SeedRejectedKeyCandidate(t, base, root, key)
					wantRoot := task4FilesystemSnapshot(t, root)
					wantBase := task4FilesystemSnapshot(t, base)

					resultNonNil, err := operation.call(store, key)
					if operation.resultName != "" && resultNonNil {
						t.Errorf("%s(%q) returned non-nil %s", operation.name, key, operation.resultName)
					}
					if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
						t.Errorf("%s(%q) error = %v, want invalid_argument", operation.name, key, err)
					}
					if contents, err := os.ReadFile(preservationPath); err != nil || string(contents) != "preserve" {
						t.Errorf("%s(%q) preservation sentinel = %q, %v", operation.name, key, contents, err)
					}
					if targetPath != "" {
						if contents, err := os.ReadFile(targetPath); err != nil || string(contents) != "target preservation sentinel" {
							t.Errorf("%s(%q) target sentinel = %q, %v", operation.name, key, contents, err)
						}
					}
					if gotRoot := task4FilesystemSnapshot(t, root); !reflect.DeepEqual(gotRoot, wantRoot) {
						t.Errorf("%s(%q) root snapshot = %#v, want %#v", operation.name, key, gotRoot, wantRoot)
					}
					if gotBase := task4FilesystemSnapshot(t, base); !reflect.DeepEqual(gotBase, wantBase) {
						t.Errorf("%s(%q) base snapshot = %#v, want %#v", operation.name, key, gotBase, wantBase)
					}
					if _, err := os.Lstat(platformAbsolute); !os.IsNotExist(err) {
						t.Errorf("%s(%q) created platform-absolute path: %v", operation.name, key, err)
					}
				})
			}
		})
	}
}

func TestFileObjectStoreRejectsSymlinkEscape(t *testing.T) {
	t.Run("parent symlink", func(t *testing.T) {
		operations := []struct {
			name string
			call func(*FileObjectStore, string) error
		}{
			{
				name: "put",
				call: func(store *FileObjectStore, key string) error {
					_, err := store.Put(context.Background(), key, strings.NewReader("replacement"))
					return err
				},
			},
			{
				name: "get",
				call: func(store *FileObjectStore, key string) error {
					reader, err := store.Get(context.Background(), key)
					if reader != nil {
						_ = reader.Close()
					}
					return err
				},
			},
			{
				name: "delete",
				call: func(store *FileObjectStore, key string) error {
					return store.Delete(context.Background(), key)
				},
			},
		}

		for _, operation := range operations {
			t.Run(operation.name, func(t *testing.T) {
				store, root := newTask4FileStore(t)
				external := t.TempDir()
				sentinelPath := filepath.Join(external, "object")
				want := []byte("external sentinel")
				if err := os.WriteFile(sentinelPath, want, 0o600); err != nil {
					t.Fatalf("write sentinel: %v", err)
				}
				if err := os.Symlink(external, filepath.Join(root, "escape")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}

				err := operation.call(store, "escape/object")
				if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
					t.Fatalf("%s error = %v, want invalid_argument", operation.name, err)
				}
				got, err := os.ReadFile(sentinelPath)
				if err != nil {
					t.Fatalf("external sentinel removed: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("external sentinel = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("final symlink", func(t *testing.T) {
		operations := []struct {
			name string
			call func(*FileObjectStore, string) error
		}{
			{
				name: "get",
				call: func(store *FileObjectStore, key string) error {
					reader, err := store.Get(context.Background(), key)
					if reader != nil {
						_ = reader.Close()
					}
					return err
				},
			},
			{
				name: "delete",
				call: func(store *FileObjectStore, key string) error {
					return store.Delete(context.Background(), key)
				},
			},
		}

		for _, operation := range operations {
			t.Run(operation.name, func(t *testing.T) {
				store, root := newTask4FileStore(t)
				external := t.TempDir()
				sentinelPath := filepath.Join(external, "sentinel")
				want := []byte("external sentinel")
				if err := os.WriteFile(sentinelPath, want, 0o600); err != nil {
					t.Fatalf("write sentinel: %v", err)
				}
				parent := filepath.Join(root, "safe")
				if err := os.MkdirAll(parent, 0o755); err != nil {
					t.Fatalf("mkdir parent: %v", err)
				}
				if err := os.Symlink(sentinelPath, filepath.Join(parent, "object")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}

				err := operation.call(store, "safe/object")
				if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
					t.Fatalf("%s error = %v, want invalid_argument", operation.name, err)
				}
				got, err := os.ReadFile(sentinelPath)
				if err != nil {
					t.Fatalf("external sentinel removed: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("external sentinel = %q, want %q", got, want)
				}
			})
		}
	})
}

func TestFileObjectStoreRejectsNonRegularFinalNode(t *testing.T) {
	tests := []struct {
		name   string
		create func(t *testing.T, path string) func()
	}{
		{
			name: "directory",
			create: func(t *testing.T, path string) func() {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir final node: %v", err)
				}
				return func() {}
			},
		},
		{
			name: "FIFO",
			create: func(t *testing.T, path string) func() {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Skipf("FIFO unavailable: %v", err)
				}
				guard, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatalf("open FIFO guard: %v", err)
				}
				return func() { _ = guard.Close() }
			},
		},
	}

	for _, tc := range tests {
		for _, operation := range []string{"get", "delete"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				store, root := newTask4FileStore(t)
				finalPath := filepath.Join(root, "safe", "object")
				if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
					t.Fatalf("mkdir parent: %v", err)
				}
				cleanup := tc.create(t, finalPath)
				defer cleanup()

				var err error
				if operation == "get" {
					var reader io.ReadCloser
					reader, err = store.Get(context.Background(), "safe/object")
					if reader != nil {
						_ = reader.Close()
					}
				} else {
					err = store.Delete(context.Background(), "safe/object")
				}
				if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
					t.Fatalf("%s error = %v, want invalid_argument", operation, err)
				}
				if _, err := os.Lstat(finalPath); err != nil {
					t.Fatalf("non-regular final node was removed: %v", err)
				}
			})
		}
	}
}

func TestFileObjectStoreDeleteMissingContainedKeyIsIdempotent(t *testing.T) {
	store, _ := newTask4FileStore(t)
	if err := store.Delete(context.Background(), task4FileKey); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}
}

func TestFileObjectStorePersistsAcrossInstances(t *testing.T) {
	root := t.TempDir()
	first, err := NewFile(root, "")
	if err != nil {
		t.Fatalf("new first store: %v", err)
	}
	want := []byte("persistent artifact")
	if _, err := first.Put(context.Background(), task4FileKey, bytes.NewReader(want)); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	second, err := NewFile(root, "")
	if err != nil {
		t.Fatalf("new second store: %v", err)
	}
	reader, err := second.Get(context.Background(), task4FileKey)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read persisted object: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("persisted bytes = %q, want %q", got, want)
	}
}

func TestFileDownloadURLRequiresObjectExistence(t *testing.T) {
	store, _ := newTask4FileStore(t)
	if url, err := store.CreateDownloadURL(context.Background(), task6ObjectKey, time.Minute); url != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("missing object URL = %#v, error = %v; want not_found", url, err)
	}
	if _, err := store.Put(context.Background(), task6ObjectKey, strings.NewReader("data")); err != nil {
		t.Fatalf("Put(): %v", err)
	}
	if url, err := store.CreateDownloadURL(context.Background(), task6ObjectKey, time.Minute); err != nil || url == nil {
		t.Fatalf("existing object URL = %#v, error = %v", url, err)
	}
}

func TestFileDownloadURLTTLBoundaries(t *testing.T) {
	store, _ := newTask4FileStore(t)
	if _, err := store.Put(context.Background(), task6ObjectKey, strings.NewReader("data")); err != nil {
		t.Fatalf("Put(): %v", err)
	}
	assertTask6DownloadTTLTable(t, func(ttl time.Duration) (*artifact.DownloadURL, error) {
		return store.CreateDownloadURL(context.Background(), task6ObjectKey, ttl)
	})
}

func TestFileDownloadURLTokensAreOpaqueUniqueAndDecodeTo32Bytes(t *testing.T) {
	store, _ := newTask4FileStore(t)
	if _, err := store.Put(context.Background(), task6OpaqueObjectKey, strings.NewReader("data")); err != nil {
		t.Fatalf("Put(): %v", err)
	}
	assertTask6OpaqueDownloadURLs(t, task6OpaqueObjectKey, func() (*artifact.DownloadURL, error) {
		return store.CreateDownloadURL(context.Background(), task6OpaqueObjectKey, time.Minute)
	})
}

func TestFileDownloadURLRejectsUnsafePaths(t *testing.T) {
	t.Run("invalid keys", func(t *testing.T) {
		for _, key := range []string{"", "/absolute", "../traversal", "a/../normalized", `a\backslash`} {
			t.Run(strings.ReplaceAll(key, "/", "_"), func(t *testing.T) {
				store, _ := newTask4FileStore(t)
				if url, err := store.CreateDownloadURL(context.Background(), key, time.Minute); url != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
					t.Fatalf("key %q URL = %#v, error = %v; want invalid_argument", key, url, err)
				}
			})
		}
	})

	t.Run("parent symlink", func(t *testing.T) {
		store, root := newTask4FileStore(t)
		external := t.TempDir()
		if err := os.WriteFile(filepath.Join(external, "object"), []byte("external"), 0o600); err != nil {
			t.Fatalf("write external: %v", err)
		}
		if err := os.Symlink(external, filepath.Join(root, "escape")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if url, err := store.CreateDownloadURL(context.Background(), "escape/object", time.Minute); url != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
			t.Fatalf("URL = %#v, error = %v; want invalid_argument", url, err)
		}
	})

	t.Run("final symlink", func(t *testing.T) {
		store, root := newTask4FileStore(t)
		external := filepath.Join(t.TempDir(), "external")
		if err := os.WriteFile(external, []byte("external"), 0o600); err != nil {
			t.Fatalf("write external: %v", err)
		}
		parent := filepath.Join(root, "safe")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatalf("mkdir parent: %v", err)
		}
		if err := os.Symlink(external, filepath.Join(parent, "object")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if url, err := store.CreateDownloadURL(context.Background(), "safe/object", time.Minute); url != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
			t.Fatalf("URL = %#v, error = %v; want invalid_argument", url, err)
		}
	})

	for _, kind := range []string{"directory", "FIFO"} {
		t.Run(kind, func(t *testing.T) {
			store, root := newTask4FileStore(t)
			parent := filepath.Join(root, "safe")
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatalf("mkdir parent: %v", err)
			}
			finalPath := filepath.Join(parent, "object")
			if kind == "directory" {
				if err := os.Mkdir(finalPath, 0o700); err != nil {
					t.Fatalf("mkdir final: %v", err)
				}
			} else if err := syscall.Mkfifo(finalPath, 0o600); err != nil {
				t.Skipf("FIFO unavailable: %v", err)
			}
			if url, err := store.CreateDownloadURL(context.Background(), "safe/object", time.Minute); url != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("URL = %#v, error = %v; want invalid_argument", url, err)
			}
		})
	}
}
