package metastore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	frameworkmysql "github.com/AGenUI/agenui-studio/harness/framework/mysql"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const (
	mysqlIntegrationHostEnvironment     = "ARTIFACT_MYSQL_TEST_HOST"
	mysqlIntegrationPortEnvironment     = "ARTIFACT_MYSQL_TEST_PORT"
	mysqlIntegrationUserEnvironment     = "ARTIFACT_MYSQL_TEST_USER"
	mysqlIntegrationPasswordEnvironment = "ARTIFACT_MYSQL_TEST_PASSWORD"
	mysqlIntegrationDatabaseEnvironment = "ARTIFACT_MYSQL_TEST_DATABASE"
	mysqlIntegrationCleanupTimeout      = 10 * time.Second
)

var mysqlIntegrationFixedTablesMu sync.Mutex

var errInvalidMySQLIntegrationFixture = errors.New("mysql integration fixture configuration is invalid")

func TestMySQLIntegrationConfigMapsStructuredEnvironment(t *testing.T) {
	config, configured, err := mysqlIntegrationConfig(mysqlIntegrationLookup(map[string]string{
		mysqlIntegrationHostEnvironment:     "127.0.0.1",
		mysqlIntegrationPortEnvironment:     "33306",
		mysqlIntegrationUserEnvironment:     "fixture-user",
		mysqlIntegrationPasswordEnvironment: "fixture-secret",
		mysqlIntegrationDatabaseEnvironment: "artifact_test",
	}))
	if err != nil || !configured {
		t.Fatalf("mysqlIntegrationConfig() = (%#v, %t, %v), want configured fixture", config, configured, err)
	}
	want := frameworkmysql.Config{
		Name:         "artifact-test",
		DBName:       "artifact_test",
		User:         "fixture-user",
		Password:     "fixture-secret",
		Host:         "127.0.0.1",
		Port:         33306,
		Charset:      "utf8mb4",
		MaxOpenConns: 4,
		MaxIdleConns: 4,
	}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("mysqlIntegrationConfig() = %#v, want %#v", config, want)
	}
}

func TestMySQLIntegrationConfigAllowsExplicitEmptyPassword(t *testing.T) {
	config, configured, err := mysqlIntegrationConfig(mysqlIntegrationLookup(map[string]string{
		mysqlIntegrationHostEnvironment:     "127.0.0.1",
		mysqlIntegrationPortEnvironment:     "3306",
		mysqlIntegrationUserEnvironment:     "root",
		mysqlIntegrationPasswordEnvironment: "",
		mysqlIntegrationDatabaseEnvironment: "artifact_test",
	}))
	if err != nil || !configured || config.Password != "" {
		t.Fatalf("mysqlIntegrationConfig(empty password) = (%#v, %t, %v), want configured empty password", config, configured, err)
	}
}

func TestMySQLIntegrationConfigTreatsFullyUnsetEnvironmentAsDisabled(t *testing.T) {
	config, configured, err := mysqlIntegrationConfig(mysqlIntegrationLookup(nil))
	if err != nil || configured || !reflect.DeepEqual(config, frameworkmysql.Config{}) {
		t.Fatalf("mysqlIntegrationConfig(unset) = (%#v, %t, %v), want disabled zero config", config, configured, err)
	}
}

func TestMySQLIntegrationConfigRejectsPartialEnvironmentWithoutLeakingValues(t *testing.T) {
	const secret = "fixture-secret"
	complete := map[string]string{
		mysqlIntegrationHostEnvironment:     "127.0.0.1",
		mysqlIntegrationPortEnvironment:     "3306",
		mysqlIntegrationUserEnvironment:     "root",
		mysqlIntegrationPasswordEnvironment: secret,
		mysqlIntegrationDatabaseEnvironment: "artifact_test",
	}
	for _, missing := range []string{
		mysqlIntegrationHostEnvironment,
		mysqlIntegrationPortEnvironment,
		mysqlIntegrationUserEnvironment,
		mysqlIntegrationPasswordEnvironment,
		mysqlIntegrationDatabaseEnvironment,
	} {
		t.Run(missing, func(t *testing.T) {
			partial := make(map[string]string, len(complete)-1)
			for name, value := range complete {
				if name != missing {
					partial[name] = value
				}
			}
			config, configured, err := mysqlIntegrationConfig(mysqlIntegrationLookup(partial))
			if !reflect.DeepEqual(config, frameworkmysql.Config{}) || configured || !errors.Is(err, errInvalidMySQLIntegrationFixture) {
				t.Fatalf("mysqlIntegrationConfig(partial) = (%#v, %t, %v), want fixed invalid-config error", config, configured, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("mysqlIntegrationConfig(partial) error exposed an environment value")
			}
		})
	}
}

func TestMySQLMetadataStoreIntegration(t *testing.T) {
	t.Run("InitializeMySQLSchemaIsRepeatable", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, db *sql.DB) {
			if err := InitializeMySQLSchema(ctx, db); err != nil {
				t.Fatalf("first repeated InitializeMySQLSchema() error = %v", err)
			}
			if err := InitializeMySQLSchema(ctx, db); err != nil {
				t.Fatalf("second repeated InitializeMySQLSchema() error = %v", err)
			}
		})
	})

	t.Run("ExactGettersRoundTripAcrossIndependentPools", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			if writerDB == readerDB {
				t.Fatal("integration fixture returned the same *sql.DB for writer and reader")
			}

			meta := mysqlGetterMetaFixture()
			meta.ArtifactID = "artifact-id\x00\xff "
			meta.ArtifactRef = "artifact-ref\x00\xff "
			idempotencyKey := "idempotency\x00\xff "
			insertMySQLIntegrationMetadata(t, ctx, writerDB, meta, idempotencyKey)

			store, err := NewMySQLMetadataStore(readerDB)
			if err != nil {
				t.Fatalf("NewMySQLMetadataStore(reader pool) error = %v", err)
			}
			getters := []struct {
				name string
				get  func() (*artifact.ArtifactMeta, error)
			}{
				{name: "ref", get: func() (*artifact.ArtifactMeta, error) { return store.GetByRef(ctx, meta.ArtifactRef) }},
				{name: "id", get: func() (*artifact.ArtifactMeta, error) { return store.GetByID(ctx, meta.ArtifactID) }},
				{name: "idempotency", get: func() (*artifact.ArtifactMeta, error) {
					return store.GetByIdempotencyKey(ctx, idempotencyKey)
				}},
			}
			for _, getter := range getters {
				t.Run(getter.name, func(t *testing.T) {
					got, err := getter.get()
					if err != nil {
						t.Fatalf("getter error = %v", err)
					}
					if !reflect.DeepEqual(got, &meta) {
						t.Fatalf("getter round trip = %#v, want %#v", got, &meta)
					}
				})
			}

			wrongRaw := strings.TrimSuffix(meta.ArtifactRef, " ")
			if got, err := store.GetByRef(ctx, wrongRaw); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
				t.Fatalf("GetByRef(raw without trailing space) = (%#v, %v), want exact not found", got, err)
			}
		})
	})
}

func TestMySQLMetadataStoreCreateIntegration(t *testing.T) {
	t.Run("ConcurrentExactRefFirstWins", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			secondDB := openMySQLIntegrationDB(t, ctx)
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, sha256.Sum256)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, sha256.Sum256)
			first := mysqlCreateIntegrationMeta("ref-first")
			second := mysqlCreateIntegrationMeta("ref-second")
			second.ArtifactRef = first.ArtifactRef

			results := mysqlRunConcurrentCreates(ctx,
				mysqlCreateCall{store: firstStore, meta: first},
				mysqlCreateCall{store: secondStore, meta: second},
			)
			_, loser := mysqlRequireOneCreateWinnerAndConflict(t, results)
			loserMeta := []artifact.ArtifactMeta{first, second}[loser]
			mysqlRequireIntegrationNotFound(t, func() (*artifact.ArtifactMeta, error) {
				return firstStore.GetByID(ctx, loserMeta.ArtifactID)
			}, "same-Ref loser independent ID")
		})
	})

	t.Run("ConcurrentExactIDFirstWinsAfterDifferentRefMisses", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			secondDB := openMySQLIntegrationDB(t, ctx)
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, sha256.Sum256)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, sha256.Sum256)
			barrier := newMySQLCreateBarrier(2)
			firstStore.afterCreateExactMiss = func(kind uint8) {
				if kind == 2 {
					barrier.Wait()
				}
			}
			secondStore.afterCreateExactMiss = func(kind uint8) {
				if kind == 2 {
					barrier.Wait()
				}
			}
			first := mysqlCreateIntegrationMeta("id-first")
			second := mysqlCreateIntegrationMeta("id-second")
			second.ArtifactID = first.ArtifactID

			results := mysqlRunConcurrentCreates(ctx,
				mysqlCreateCall{store: firstStore, meta: first},
				mysqlCreateCall{store: secondStore, meta: second},
			)
			_, loser := mysqlRequireOneCreateWinnerAndConflict(t, results)
			loserMeta := []artifact.ArtifactMeta{first, second}[loser]
			mysqlRequireIntegrationNotFound(t, func() (*artifact.ArtifactMeta, error) {
				return firstStore.GetByRef(ctx, loserMeta.ArtifactRef)
			}, "same-ID loser independent Ref")
		})
	})

	t.Run("ConcurrentIdempotencyConverges", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			secondDB := openMySQLIntegrationDB(t, ctx)
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, sha256.Sum256)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, sha256.Sum256)
			key := "concurrent-idempotency\x00\xff "
			first := mysqlCreateIntegrationMeta("idem-first")
			second := mysqlCreateIntegrationMeta("idem-second")

			results := mysqlRunConcurrentCreates(ctx,
				mysqlCreateCall{store: firstStore, meta: first, key: key},
				mysqlCreateCall{store: secondStore, meta: second, key: key},
			)
			for index, result := range results {
				if result.err != nil || result.meta == nil {
					t.Fatalf("concurrent idempotency result %d = (%#v, %v), want success", index, result.meta, result.err)
				}
			}
			if results[0].meta.ArtifactRef != results[1].meta.ArtifactRef {
				t.Fatalf("concurrent idempotency refs = %q and %q, want one winner", results[0].meta.ArtifactRef, results[1].meta.ArtifactRef)
			}
			winner, err := firstStore.GetByIdempotencyKey(ctx, key)
			if err != nil || winner.ArtifactRef != results[0].meta.ArtifactRef {
				t.Fatalf("GetByIdempotencyKey() = (%#v, %v), want converged winner", winner, err)
			}
			for _, candidate := range []artifact.ArtifactMeta{first, second} {
				if candidate.ArtifactRef == winner.ArtifactRef {
					continue
				}
				mysqlRequireIntegrationNotFound(t, func() (*artifact.ArtifactMeta, error) {
					return firstStore.GetByRef(ctx, candidate.ArtifactRef)
				}, "idempotency loser Ref")
				mysqlRequireIntegrationNotFound(t, func() (*artifact.ArtifactMeta, error) {
					return firstStore.GetByID(ctx, candidate.ArtifactID)
				}, "idempotency loser ID")
			}
			var metadataRows int
			if err := firstDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM artifact_metadata").Scan(&metadataRows); err != nil {
				t.Fatalf("count idempotency metadata rows: %v", err)
			}
			if metadataRows != 1 {
				t.Fatalf("idempotency metadata rows = %d, want 1", metadataRows)
			}
		})
	})

	t.Run("ConflictRollbackReleasesCandidateIdempotency", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			secondDB := openMySQLIntegrationDB(t, ctx)
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, sha256.Sum256)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, sha256.Sum256)
			existing := mysqlCreateIntegrationMeta("rollback-existing")
			if got, err := firstStore.Create(ctx, existing, ""); err != nil || got == nil {
				t.Fatalf("Create(existing) = (%#v, %v)", got, err)
			}
			key := "rollback-reusable-key"
			conflict := mysqlCreateIntegrationMeta("rollback-conflict")
			conflict.ArtifactID = existing.ArtifactID
			if got, err := secondStore.Create(ctx, conflict, key); got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
				t.Fatalf("Create(conflict) = (%#v, %v), want conflict", got, err)
			}
			keyDigest := sha256.Sum256([]byte(key))
			var failedKeyLocks int
			if err := firstDB.QueryRowContext(
				ctx,
				"SELECT COUNT(*) FROM artifact_metadata_key_lock WHERE key_kind = ? AND key_hash = ?",
				int64(1),
				keyDigest[:],
			).Scan(&failedKeyLocks); err != nil {
				t.Fatalf("count rolled-back candidate key lock: %v", err)
			}
			if failedKeyLocks != 0 {
				t.Fatalf("rolled-back candidate idempotency key locks = %d, want 0", failedKeyLocks)
			}
			if got, err := firstStore.GetByIdempotencyKey(ctx, key); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
				t.Fatalf("GetByIdempotencyKey(after rollback) = (%#v, %v), want not found", got, err)
			}
			recovery := mysqlCreateIntegrationMeta("rollback-recovery")
			got, err := secondStore.Create(ctx, recovery, key)
			if err != nil || got == nil || got.ArtifactRef != recovery.ArtifactRef {
				t.Fatalf("Create(reusing rolled-back key) = (%#v, %v), want recovery", got, err)
			}
		})
	})

	t.Run("InjectedDigestCollisionDifferentRawCoexists", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			secondDB := openMySQLIntegrationDB(t, ctx)
			constantDigest := func([]byte) [32]byte { return [32]byte{0xca, 0xfe, 0xba, 0xbe} }
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, constantDigest)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, constantDigest)
			first := mysqlCreateIntegrationMeta("collision-first\x00")
			second := mysqlCreateIntegrationMeta("collision-second\xff ")

			results := mysqlRunConcurrentCreates(ctx,
				mysqlCreateCall{store: firstStore, meta: first, key: "collision-idem-first"},
				mysqlCreateCall{store: secondStore, meta: second, key: "collision-idem-second"},
			)
			for index, result := range results {
				if result.err != nil || result.meta == nil {
					t.Fatalf("digest collision result %d = (%#v, %v), want success", index, result.meta, result.err)
				}
			}
			for _, meta := range []artifact.ArtifactMeta{first, second} {
				got, err := firstStore.GetByRef(ctx, meta.ArtifactRef)
				if err != nil || got.ArtifactID != meta.ArtifactID {
					t.Fatalf("GetByRef(collision raw %q) = (%#v, %v), want exact row", meta.ArtifactRef, got, err)
				}
			}
		})
	})

	t.Run("BinaryNullTimeAndDeepCopyRoundTripAcrossPools", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
			reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
			location := time.FixedZone("integration-non-UTC", 9*60*60+37)
			nullMeta := portableMySQLMetaFixture()
			nullMeta.ArtifactID = "binary-null-id\x00\xff "
			nullMeta.ArtifactRef = "binary-null-ref\x00\xff "
			nullMeta.ExpiresAt = time.Time{}
			nullMeta.DeletedAt = time.Time{}
			nullMeta.DerivedFrom = nil
			nullMeta.Metadata = nil
			emptyMeta := portableMySQLMetaFixture()
			emptyMeta.ArtifactID = "binary-empty-id\x00\xff "
			emptyMeta.ArtifactRef = "binary-empty-ref\x00\xff "
			emptyMeta.CreatedAt = time.Date(2026, time.July, 14, 1, 2, 3, 999_999_999, location)
			emptyMeta.ExpiresAt = time.Date(2026, time.July, 15, 2, 3, 4, 1, location)
			emptyMeta.DeletedAt = time.Date(2026, time.July, 16, 3, 4, 5, 17, location)
			emptyMeta.DerivedFrom = []artifact.ArtifactLineage{}
			emptyMeta.Metadata = map[string]string{}
			metas := []artifact.ArtifactMeta{nullMeta, emptyMeta}

			for index := range metas {
				original := metas[index]
				immediate, err := writer.Create(ctx, original, fmt.Sprintf("round-trip-key-%d\x00\xff ", index))
				if err != nil {
					t.Fatalf("Create(round trip %d) error = %v", index, err)
				}
				if !reflect.DeepEqual(immediate, &original) {
					t.Fatalf("Create(round trip %d) immediate = %#v, want DeepEqual %#v", index, immediate, &original)
				}
				original.Preview.Text = "mutated input"
				if immediate.Preview.Text == original.Preview.Text {
					t.Fatalf("Create(round trip %d) result aliases input", index)
				}

				reread, err := reader.GetByRef(ctx, metas[index].ArtifactRef)
				if err != nil {
					t.Fatalf("GetByRef(round trip %d) error = %v", index, err)
				}
				for _, pair := range []struct {
					name string
					got  time.Time
					want time.Time
				}{
					{name: "created", got: reread.CreatedAt, want: metas[index].CreatedAt},
					{name: "expires", got: reread.ExpiresAt, want: metas[index].ExpiresAt},
					{name: "deleted", got: reread.DeletedAt, want: metas[index].DeletedAt},
				} {
					if !pair.got.Equal(pair.want) {
						t.Fatalf("round trip %d %s = %v, want same instant as %v", index, pair.name, pair.got, pair.want)
					}
					if !pair.got.IsZero() && pair.got.Location() != time.UTC {
						t.Fatalf("round trip %d %s location = %v, want UTC", index, pair.name, pair.got.Location())
					}
				}
				want := metas[index]
				want.CreatedAt = reread.CreatedAt
				want.ExpiresAt = reread.ExpiresAt
				want.DeletedAt = reread.DeletedAt
				if !reflect.DeepEqual(reread, &want) {
					t.Fatalf("round trip %d reread = %#v, want %#v after time normalization", index, reread, &want)
				}
				immediate.Preview.Text = "mutated immediate"
				repeated, err := reader.GetByRef(ctx, metas[index].ArtifactRef)
				if err != nil || repeated.Preview.Text != metas[index].Preview.Text {
					t.Fatalf("round trip %d repeated reread = (%#v, %v), want isolated DB state", index, repeated, err)
				}
			}
		})
	})
}

func TestMySQLMetadataStoreListIntegration(t *testing.T) {
	t.Run("CrossPoolVisibilityAndEveryCallerFilter", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
			reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
			boundary := time.Date(2026, time.July, 14, 18, 19, 20, 765_432_109, time.FixedZone("list-filter", 9*60*60+37))
			matched := mysqlListIntegrationMeta("all-match")
			matched.TenantID = "tenant\x00\xff "
			matched.SessionID = "session\x00\xff "
			matched.RunID = "run\x00\xff "
			matched.OwnerModule = artifact.OwnerModule("owner-module-alias\x00\xff ")
			matched.OwnerID = "owner-id\x00\xff "
			matched.ArtifactType = artifact.ArtifactType("type-alias\x00\xff ")
			matched.Visibility = artifact.Visibility("visibility-alias\x00\xff ")
			matched.ExpiresAt = boundary
			mysqlCreateListIntegrationMeta(t, ctx, writer, matched)

			distractors := make([]artifact.ArtifactMeta, 0, 8)
			for index := 0; index < 8; index++ {
				candidate := matched
				candidate.ArtifactID = fmt.Sprintf("list-filter-distractor-%d", index)
				candidate.ArtifactRef = fmt.Sprintf("list-filter-ref-distractor-%d", index)
				candidate.StorageKey = fmt.Sprintf("list-filter-key-distractor-%d", index)
				switch index {
				case 0:
					candidate.TenantID += "-other"
				case 1:
					candidate.SessionID += "-other"
				case 2:
					candidate.RunID += "-other"
				case 3:
					candidate.OwnerModule += "-other"
				case 4:
					candidate.OwnerID += "-other"
				case 5:
					candidate.ArtifactType += "-other"
				case 6:
					candidate.Visibility += "-other"
				case 7:
					candidate.ExpiresAt = boundary.Add(time.Nanosecond)
				}
				distractors = append(distractors, candidate)
			}
			for _, candidate := range distractors {
				mysqlCreateListIntegrationMeta(t, ctx, writer, candidate)
			}

			got, err := reader.List(ctx, artifact.ListQuery{
				TenantID:          matched.TenantID,
				SessionID:         matched.SessionID,
				RunID:             matched.RunID,
				OwnerModule:       matched.OwnerModule,
				OwnerID:           matched.OwnerID,
				Type:              matched.ArtifactType,
				Visibility:        matched.Visibility,
				ExpiredAtOrBefore: boundary,
			})
			if err != nil || len(got) != 1 || got[0].ArtifactID != matched.ArtifactID {
				t.Fatalf("List(all filters across pools) = (%#v, %v), want one match %q", got, err, matched.ArtifactID)
			}
		})
	})

	t.Run("StatusAndExpiryClosedNanosecondBoundary", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
			reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
			boundary := time.Date(2026, time.July, 14, 21, 22, 23, 456_789_123, time.FixedZone("list-expiry", -7*60*60-19))
			baseCreatedAt := time.Unix(1_720_000_100, 0).UTC()
			specs := []struct {
				suffix string
				status artifact.ArtifactStatus
				expiry time.Time
			}{
				{suffix: "null-ready", status: artifact.ArtifactStatusReady},
				{suffix: "before-ready", status: artifact.ArtifactStatusReady, expiry: boundary.Add(-time.Nanosecond)},
				{suffix: "equal-ready", status: artifact.ArtifactStatusReady, expiry: boundary},
				{suffix: "after-ready", status: artifact.ArtifactStatusReady, expiry: boundary.Add(time.Nanosecond)},
				{suffix: "later-second-ready", status: artifact.ArtifactStatusReady, expiry: boundary.Add(time.Second)},
				{suffix: "before-deleted", status: artifact.ArtifactStatusDeleted, expiry: boundary.Add(-time.Nanosecond)},
				{suffix: "before-expired", status: artifact.ArtifactStatusExpired, expiry: boundary.Add(-time.Second)},
				{suffix: "before-unknown", status: artifact.ArtifactStatus("unknown-status-alias"), expiry: boundary.Add(-time.Second)},
			}
			ids := make(map[string]string, len(specs))
			for index, spec := range specs {
				meta := mysqlListIntegrationMeta(spec.suffix)
				meta.CreatedAt = baseCreatedAt.Add(time.Duration(index) * time.Nanosecond)
				meta.Status = spec.status
				meta.ExpiresAt = spec.expiry
				mysqlCreateListIntegrationMeta(t, ctx, writer, meta)
				ids[spec.suffix] = meta.ArtifactID
			}

			got, err := reader.List(ctx, artifact.ListQuery{ExpiredAtOrBefore: boundary})
			if err != nil {
				t.Fatalf("List(default expiry boundary) error = %v", err)
			}
			mysqlRequireListIntegrationIDs(t, got, ids["before-ready"], ids["equal-ready"], ids["before-expired"], ids["before-unknown"])

			got, err = reader.List(ctx, artifact.ListQuery{ExpiredAtOrBefore: boundary, IncludeDeleted: true})
			if err != nil {
				t.Fatalf("List(include deleted expiry boundary) error = %v", err)
			}
			mysqlRequireListIntegrationIDs(t, got, ids["before-ready"], ids["equal-ready"], ids["before-deleted"], ids["before-expired"], ids["before-unknown"])

			got, err = reader.List(ctx, artifact.ListQuery{})
			if err != nil {
				t.Fatalf("List(zero expiry) error = %v", err)
			}
			mysqlRequireListIntegrationIDs(t, got, ids["null-ready"], ids["before-ready"], ids["equal-ready"], ids["after-ready"], ids["later-second-ready"], ids["before-expired"], ids["before-unknown"])
		})
	})

	t.Run("CreatedAtThenFullRawArtifactIDOrdering", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
			reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
			second := int64(1_720_000_200)
			commonPrefix := strings.Repeat("p", 1100)
			earlySecond := mysqlListIntegrationMeta("order-early-second")
			earlySecond.CreatedAt = time.Unix(second-1, 999_999_999).UTC()
			earlyNanosecond := mysqlListIntegrationMeta("order-early-nanosecond")
			earlyNanosecond.CreatedAt = time.Unix(second, 1).UTC()
			sameTime := []artifact.ArtifactMeta{
				mysqlListIntegrationMeta("order-long-high"),
				mysqlListIntegrationMeta("order-long-middle"),
				mysqlListIntegrationMeta("order-long-low"),
			}
			sameTime[0].ArtifactID = commonPrefix + "\xff"
			sameTime[1].ArtifactID = commonPrefix + "a"
			sameTime[2].ArtifactID = commonPrefix + "\x00"
			for index := range sameTime {
				sameTime[index].CreatedAt = time.Unix(second, 2).UTC()
			}
			for _, meta := range append([]artifact.ArtifactMeta{sameTime[0], sameTime[1], sameTime[2], earlyNanosecond}, earlySecond) {
				mysqlCreateListIntegrationMeta(t, ctx, writer, meta)
			}

			got, err := reader.List(ctx, artifact.ListQuery{IncludeDeleted: true})
			if err != nil {
				t.Fatalf("List(deterministic ordering) error = %v", err)
			}
			mysqlRequireListIntegrationIDs(t, got,
				earlySecond.ArtifactID,
				earlyNanosecond.ArtifactID,
				sameTime[2].ArtifactID,
				sameTime[1].ArtifactID,
				sameTime[0].ArtifactID,
			)
		})
	})

	t.Run("InjectedDigestCollisionAndSelectedScopeOnly", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			constantArray := [sha256.Size]byte{0xca, 0xfe, 0xba, 0xbe}
			constantDigest := func([]byte) [sha256.Size]byte { return constantArray }
			writer := mysqlMustNewIntegrationStore(t, writerDB, constantDigest)
			reader := mysqlMustNewIntegrationStore(t, readerDB, constantDigest)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
			selectedTenant := "selected-tenant\x00\xff "
			selectedFirst := mysqlListIntegrationMeta("collision-selected-first")
			selectedFirst.TenantID = selectedTenant
			selectedFirst.CreatedAt = time.Unix(1_720_000_300, 1).UTC()
			selectedSecond := mysqlListIntegrationMeta("collision-selected-second")
			selectedSecond.TenantID = selectedTenant
			selectedSecond.CreatedAt = time.Unix(1_720_000_300, 2).UTC()
			unselected := mysqlListIntegrationMeta("collision-unselected")
			unselected.TenantID = "different-raw-same-digest"
			unselected.CreatedAt = time.Unix(1_720_000_300, 3).UTC()
			asBytes := func([]byte) []byte { return append([]byte(nil), constantArray[:]...) }
			for index, meta := range []artifact.ArtifactMeta{selectedFirst, selectedSecond, unselected} {
				insertMySQLIntegrationMetadataWithDigest(t, ctx, writerDB, meta, fmt.Sprintf("list-collision-%d", index), asBytes)
			}

			outsideFirst := mysqlListIntegrationMeta("outside-duplicate-first")
			outsideFirst.TenantID = "outside-scope"
			outsideSecond := mysqlListIntegrationMeta("outside-duplicate-second")
			outsideSecond.TenantID = "outside-scope"
			outsideSecond.ArtifactID = outsideFirst.ArtifactID
			outsideSecond.ArtifactRef = outsideFirst.ArtifactRef
			insertMySQLIntegrationMetadataWithDigest(t, ctx, writerDB, outsideFirst, "outside-duplicate-1", asBytes)
			insertMySQLIntegrationMetadataWithDigest(t, ctx, writerDB, outsideSecond, "outside-duplicate-2", asBytes)

			got, err := reader.List(ctx, artifact.ListQuery{TenantID: selectedTenant, IncludeDeleted: true})
			if err != nil {
				t.Fatalf("List(selected raw under constant digest) error = %v", err)
			}
			mysqlRequireListIntegrationIDs(t, got, selectedFirst.ArtifactID, selectedSecond.ArtifactID)
		})
	})

	for _, duplicate := range []string{"ArtifactID", "ArtifactRef"} {
		duplicate := duplicate
		t.Run("DirectDuplicateRaw"+duplicate+"IsInvalidEvenWithDifferentDigests", func(t *testing.T) {
			runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
				readerDB := openMySQLIntegrationDB(t, ctx)
				writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
				reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
				mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
				first := mysqlListIntegrationMeta("direct-duplicate-first-" + duplicate)
				second := mysqlListIntegrationMeta("direct-duplicate-second-" + duplicate)
				if duplicate == "ArtifactID" {
					second.ArtifactID = first.ArtifactID
				} else {
					second.ArtifactRef = first.ArtifactRef
				}
				firstDigest := mysqlIntegrationSaltedDigest(0x11)
				secondDigest := mysqlIntegrationSaltedDigest(0x22)
				insertMySQLIntegrationMetadataWithDigest(t, ctx, writerDB, first, "direct-duplicate-first", firstDigest)
				insertMySQLIntegrationMetadataWithDigest(t, ctx, writerDB, second, "direct-duplicate-second", secondDigest)

				got, err := reader.List(ctx, artifact.ListQuery{IncludeDeleted: true})
				if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
					t.Fatalf("List(direct duplicate raw %s) = (%#v, %v), want nil invalid_argument", duplicate, got, err)
				}
			})
		})
	}

	t.Run("UTCReadbackNilEmptyAndContainerIsolation", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
			reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
			location := time.FixedZone("list-non-UTC", 8*60*60+43)
			base := time.Date(2026, time.July, 14, 1, 2, 3, 123_456_700, location)
			nilContainers := mysqlListIntegrationMeta("isolation-nil")
			nilContainers.CreatedAt = base
			nilContainers.ExpiresAt = time.Time{}
			nilContainers.DeletedAt = time.Time{}
			nilContainers.Preview.Fields = nil
			nilContainers.DerivedFrom = nil
			nilContainers.Metadata = nil
			emptyContainers := mysqlListIntegrationMeta("isolation-empty")
			emptyContainers.CreatedAt = base.Add(time.Nanosecond)
			emptyContainers.ExpiresAt = base.Add(time.Hour)
			emptyContainers.DeletedAt = base.Add(2 * time.Hour)
			emptyContainers.Preview.Fields = map[string]any{}
			emptyContainers.DerivedFrom = []artifact.ArtifactLineage{}
			emptyContainers.Metadata = map[string]string{}
			sharedFirst := mysqlListIntegrationMeta("isolation-shared-first")
			sharedFirst.CreatedAt = base.Add(2 * time.Nanosecond)
			sharedFirst.ExpiresAt = base.Add(3 * time.Hour)
			sharedFirst.Preview.Fields = map[string]any{"nested": []any{[]string{"original"}}}
			sharedFirst.DerivedFrom = []artifact.ArtifactLineage{{ArtifactRef: "source", Relation: artifact.LineageReferenced}}
			sharedFirst.Metadata = map[string]string{"key": "original"}
			sharedSecond := mysqlListIntegrationMeta("isolation-shared-second")
			sharedSecond.CreatedAt = base.Add(3 * time.Nanosecond)
			sharedSecond.ExpiresAt = base.Add(4 * time.Hour)
			sharedSecond.Preview = sharedFirst.Preview
			sharedSecond.DerivedFrom = sharedFirst.DerivedFrom
			sharedSecond.Metadata = sharedFirst.Metadata
			metas := []artifact.ArtifactMeta{nilContainers, emptyContainers, sharedFirst, sharedSecond}
			for _, meta := range metas {
				mysqlCreateListIntegrationMeta(t, ctx, writer, meta)
			}

			got, err := reader.List(ctx, artifact.ListQuery{IncludeDeleted: true})
			if err != nil || len(got) != len(metas) {
				t.Fatalf("List(container isolation) = (%#v, %v), want %d rows", got, err, len(metas))
			}
			want := mysqlNormalizeListIntegrationTimes(metas)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("List(container/time round trip) = %#v, want %#v", got, want)
			}
			for index := range got {
				for _, value := range []time.Time{got[index].CreatedAt, got[index].ExpiresAt, got[index].DeletedAt} {
					if !value.IsZero() && value.Location() != time.UTC {
						t.Fatalf("List row %d time location = %v, want UTC", index, value.Location())
					}
				}
			}
			got[2].Preview.Fields["nested"].([]any)[0].([]string)[0] = "mutated"
			got[2].DerivedFrom[0].ArtifactRef = "mutated"
			got[2].Metadata["key"] = "mutated"
			if got[3].Preview.Fields["nested"].([]any)[0].([]string)[0] != "original" ||
				got[3].DerivedFrom[0].ArtifactRef != "source" || got[3].Metadata["key"] != "original" {
				t.Fatalf("List rows share containers: %#v", got)
			}
			repeated, err := reader.List(ctx, artifact.ListQuery{IncludeDeleted: true})
			if err != nil || !reflect.DeepEqual(repeated, want) {
				t.Fatalf("repeated List() = (%#v, %v), want isolated %#v", repeated, err, want)
			}
		})
	})
}

func TestMySQLMetadataStoreMarkDeletedIntegration(t *testing.T) {
	t.Run("ImmediateDeepEqualCrossPoolUTCAndRepeatedFirstTombstone", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
			reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
			base := mysqlListIntegrationMeta("delete-immediate")
			base.Preview.Fields = map[string]any{"nested": []any{[]string{"original"}}}
			base.DerivedFrom = []artifact.ArtifactLineage{{ArtifactRef: "source", Relation: artifact.LineageReferenced}}
			base.Metadata = map[string]string{"key": "original"}
			mysqlCreateListIntegrationMeta(t, ctx, writer, base)
			at := time.Now()
			reason := artifact.DeleteReason("first-reason\x00\xff ")

			first, err := reader.MarkDeleted(ctx, base.ArtifactRef, reason, at)
			want := base
			want.Status = artifact.ArtifactStatusDeleted
			want.DeletedAt = at
			want.DeleteReason = reason
			want.PurgeStatus = artifact.PurgeStatusPending
			want.PurgedAt = time.Time{}
			if err != nil || !reflect.DeepEqual(first, &want) {
				t.Fatalf("MarkDeleted(first immediate) = (%#v, %v), want DeepEqual %#v", first, err, &want)
			}

			persisted, err := writer.GetByRef(ctx, base.ArtifactRef)
			if err != nil || persisted.Status != artifact.ArtifactStatusDeleted || persisted.DeleteReason != reason || !persisted.DeletedAt.Equal(at) ||
				persisted.PurgeStatus != artifact.PurgeStatusPending || !persisted.PurgedAt.IsZero() {
				t.Fatalf("cross-pool persisted tombstone = (%#v, %v), want first", persisted, err)
			}
			if persisted.DeletedAt.Location() != time.UTC {
				t.Fatalf("persisted DeletedAt location = %v, want UTC", persisted.DeletedAt.Location())
			}
			repeated, err := writer.MarkDeleted(ctx, base.ArtifactRef, artifact.DeleteReasonCleanup, at.Add(time.Hour))
			if err != nil || repeated.DeleteReason != reason || !repeated.DeletedAt.Equal(at) ||
				repeated.PurgeStatus != artifact.PurgeStatusPending || !repeated.PurgedAt.IsZero() {
				t.Fatalf("MarkDeleted(repeat) = (%#v, %v), want first tombstone", repeated, err)
			}
			first.Preview.Fields["nested"].([]any)[0].([]string)[0] = "mutated"
			first.DerivedFrom[0].ArtifactRef = "mutated"
			first.Metadata["key"] = "mutated"
			again, err := reader.MarkDeleted(ctx, base.ArtifactRef, artifact.DeleteReasonUser, at.Add(2*time.Hour))
			if err != nil || again.Preview.Fields["nested"].([]any)[0].([]string)[0] != "original" || again.DerivedFrom[0].ArtifactRef != "source" || again.Metadata["key"] != "original" {
				t.Fatalf("MarkDeleted(repeated containers) = (%#v, %v), want isolated first", again, err)
			}
		})
	})

	t.Run("OrdinaryConcurrentCallsConvergeToOneExactReasonAndInstant", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			secondDB := openMySQLIntegrationDB(t, ctx)
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, sha256.Sum256)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, firstDB, secondDB, firstStore, secondStore)
			base := mysqlListIntegrationMeta("delete-concurrent")
			mysqlCreateListIntegrationMeta(t, ctx, firstStore, base)
			candidates := []struct {
				store  *MySQLMetadataStore
				reason artifact.DeleteReason
				at     time.Time
			}{
				{store: firstStore, reason: artifact.DeleteReason("concurrent-first"), at: time.Unix(1_720_010_000, 11).UTC()},
				{store: secondStore, reason: artifact.DeleteReason("concurrent-second"), at: time.Unix(1_720_010_000, 22).UTC()},
			}
			start := make(chan struct{})
			results := make(chan mysqlDeleteIntegrationResult, 2)
			concurrentCtx, cancelConcurrent := context.WithTimeout(ctx, 8*time.Second)
			exited := []chan struct{}{make(chan struct{}), make(chan struct{})}
			started := make([]atomic.Bool, len(candidates))
			var cleanupOnce sync.Once
			cleanupConcurrentWorkers := func() {
				cleanupOnce.Do(func() {
					cancelConcurrent()
					for index := range candidates {
						mysqlReapDeleteIntegrationGoroutine(t, exited[index], &started[index], "ordinary concurrent delete goroutine")
					}
				})
			}
			defer cleanupConcurrentWorkers()
			t.Cleanup(cleanupConcurrentWorkers)
			for index, candidate := range candidates {
				index := index
				candidate := candidate
				started[index].Store(true)
				go func() {
					defer close(exited[index])
					select {
					case <-start:
					case <-concurrentCtx.Done():
						return
					}
					meta, err := candidate.store.MarkDeleted(concurrentCtx, base.ArtifactRef, candidate.reason, candidate.at)
					results <- mysqlDeleteIntegrationResult{meta: meta, err: err}
				}()
			}
			close(start)
			got := []mysqlDeleteIntegrationResult{
				mysqlAwaitDeleteIntegrationResult(t, concurrentCtx, results, "first ordinary delete result"),
				mysqlAwaitDeleteIntegrationResult(t, concurrentCtx, results, "second ordinary delete result"),
			}
			for index, result := range got {
				if result.err != nil || result.meta == nil {
					t.Fatalf("concurrent MarkDeleted result %d = (%#v, %v), want success", index, result.meta, result.err)
				}
			}
			if got[0].meta.DeleteReason != got[1].meta.DeleteReason || !got[0].meta.DeletedAt.Equal(got[1].meta.DeletedAt) {
				t.Fatalf("concurrent tombstones did not converge: %#v / %#v", got[0].meta, got[1].meta)
			}
			matchedCandidate := false
			for _, candidate := range candidates {
				if got[0].meta.DeleteReason == candidate.reason && got[0].meta.DeletedAt.Equal(candidate.at) {
					matchedCandidate = true
				}
			}
			if !matchedCandidate {
				t.Fatalf("concurrent tombstone = reason %q at %v, want one exact candidate", got[0].meta.DeleteReason, got[0].meta.DeletedAt)
			}
		})
	})

	t.Run("DeterministicForUpdateBlockingDeadlineAndRetry", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			lockTestCtx, cancelLockTest := context.WithTimeout(ctx, 8*time.Second)
			secondDB := openMySQLIntegrationDB(t, ctx)
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, sha256.Sum256)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, firstDB, secondDB, firstStore, secondStore)
			base := mysqlListIntegrationMeta("delete-lock")
			mysqlCreateListIntegrationMeta(t, ctx, firstStore, base)

			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
			firstExited := make(chan struct{})
			secondExited := make(chan struct{})
			var firstStarted atomic.Bool
			var secondStarted atomic.Bool
			var cleanupOnce sync.Once
			cleanupLockWorkers := func() {
				cleanupOnce.Do(func() {
					releaseFirst()
					cancelLockTest()
					mysqlReapDeleteIntegrationGoroutine(t, firstExited, &firstStarted, "first lock goroutine")
					mysqlReapDeleteIntegrationGoroutine(t, secondExited, &secondStarted, "second lock goroutine")
				})
			}
			defer cleanupLockWorkers()
			t.Cleanup(cleanupLockWorkers)
			firstHeld := make(chan struct{}, 1)
			secondBeforeQuery := make(chan struct{}, 1)
			firstDone := make(chan mysqlDeleteIntegrationResult, 1)
			secondDone := make(chan mysqlDeleteIntegrationResult, 1)
			firstAt := time.Unix(1_720_020_000, 123).UTC()
			firstReason := artifact.DeleteReason("lock-first")
			firstStore.afterMarkDeletedExactRead = func() {
				select {
				case firstHeld <- struct{}{}:
				default:
				}
				<-release
			}
			secondStore.beforeMarkDeletedLockQuery = func() {
				select {
				case secondBeforeQuery <- struct{}{}:
				default:
				}
			}

			firstStarted.Store(true)
			go func() {
				defer close(firstExited)
				meta, err := firstStore.MarkDeleted(lockTestCtx, base.ArtifactRef, firstReason, firstAt)
				firstDone <- mysqlDeleteIntegrationResult{meta: meta, err: err}
			}()
			mysqlAwaitDeleteIntegrationSignal(t, lockTestCtx, firstHeld, "first store holding FOR UPDATE row lock")

			secondCtx, cancelSecond := context.WithTimeout(lockTestCtx, 1500*time.Millisecond)
			defer cancelSecond()
			secondDeadline, ok := secondCtx.Deadline()
			if !ok {
				t.Fatal("second lock waiter has no deadline")
			}
			secondStarted.Store(true)
			go func() {
				defer close(secondExited)
				meta, err := secondStore.MarkDeleted(secondCtx, base.ArtifactRef, artifact.DeleteReason("lock-second"), firstAt.Add(time.Second))
				secondDone <- mysqlDeleteIntegrationResult{meta: meta, err: err}
			}()
			mysqlAwaitDeleteIntegrationSignal(t, lockTestCtx, secondBeforeQuery, "second store immediately before lock query")
			if margin := time.Until(secondDeadline); margin < 750*time.Millisecond {
				t.Fatalf("second lock deadline margin = %v, want at least 750ms", margin)
			}
			blocked := mysqlAwaitDeleteIntegrationResult(t, lockTestCtx, secondDone, "blocked second delete result")
			if blocked.meta != nil || !errors.Is(blocked.err, context.DeadlineExceeded) {
				t.Fatalf("blocked MarkDeleted = (%#v, %v), want context deadline from row lock wait", blocked.meta, blocked.err)
			}

			releaseFirst()
			first := mysqlAwaitDeleteIntegrationResult(t, lockTestCtx, firstDone, "released first delete result")
			if first.err != nil || first.meta == nil || first.meta.DeleteReason != firstReason || !first.meta.DeletedAt.Equal(firstAt) {
				t.Fatalf("first MarkDeleted after release = (%#v, %v), want first tombstone", first.meta, first.err)
			}
			secondStore.beforeMarkDeletedLockQuery = nil
			retry, err := secondStore.MarkDeleted(ctx, base.ArtifactRef, artifact.DeleteReason("retry-must-not-win"), firstAt.Add(2*time.Second))
			if err != nil || retry.DeleteReason != firstReason || !retry.DeletedAt.Equal(firstAt) {
				t.Fatalf("second MarkDeleted retry = (%#v, %v), want first tombstone", retry, err)
			}
		})
	})

	t.Run("ConstantDigestDifferentRawRefTargetsOnlyExactRow", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, firstDB *sql.DB) {
			secondDB := openMySQLIntegrationDB(t, ctx)
			constantDigest := func([]byte) [sha256.Size]byte { return [sha256.Size]byte{0xdd, 0xee} }
			firstStore := mysqlMustNewIntegrationStore(t, firstDB, constantDigest)
			secondStore := mysqlMustNewIntegrationStore(t, secondDB, constantDigest)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, firstDB, secondDB, firstStore, secondStore)
			target := mysqlListIntegrationMeta("delete-collision-target\x00")
			other := mysqlListIntegrationMeta("delete-collision-other\xff ")
			mysqlCreateListIntegrationMeta(t, ctx, firstStore, target)
			mysqlCreateListIntegrationMeta(t, ctx, firstStore, other)
			at := time.Unix(1_720_030_000, 9).UTC()
			deleted, err := secondStore.MarkDeleted(ctx, target.ArtifactRef, artifact.DeleteReasonUser, at)
			if err != nil || deleted.ArtifactRef != target.ArtifactRef || deleted.Status != artifact.ArtifactStatusDeleted {
				t.Fatalf("MarkDeleted(constant digest target) = (%#v, %v)", deleted, err)
			}
			untouched, err := secondStore.GetByRef(ctx, other.ArtifactRef)
			if err != nil || untouched.Status != artifact.ArtifactStatusReady {
				t.Fatalf("constant-digest other row = (%#v, %v), want ready", untouched, err)
			}
		})
	})

	t.Run("ZeroNilEmptyRichContainersRemainIsolated", func(t *testing.T) {
		runMySQLIntegrationTest(t, func(ctx context.Context, writerDB *sql.DB) {
			readerDB := openMySQLIntegrationDB(t, ctx)
			writer := mysqlMustNewIntegrationStore(t, writerDB, sha256.Sum256)
			reader := mysqlMustNewIntegrationStore(t, readerDB, sha256.Sum256)
			mysqlRequireIndependentIntegrationPoolsAndStores(t, writerDB, readerDB, writer, reader)
			nilMeta := mysqlListIntegrationMeta("delete-containers-nil")
			nilMeta.Preview.Fields = nil
			nilMeta.DerivedFrom = nil
			nilMeta.Metadata = nil
			emptyMeta := mysqlListIntegrationMeta("delete-containers-empty")
			emptyMeta.Preview.Fields = map[string]any{}
			emptyMeta.DerivedFrom = []artifact.ArtifactLineage{}
			emptyMeta.Metadata = map[string]string{}
			richFirst := mysqlListIntegrationMeta("delete-containers-rich-first")
			richFirst.Preview.Fields = map[string]any{"nested": []any{[]string{"original"}}}
			richFirst.DerivedFrom = []artifact.ArtifactLineage{{ArtifactRef: "source", Relation: artifact.LineageReferenced}}
			richFirst.Metadata = map[string]string{"key": "original"}
			richSecond := mysqlListIntegrationMeta("delete-containers-rich-second")
			richSecond.Preview = richFirst.Preview
			richSecond.DerivedFrom = richFirst.DerivedFrom
			richSecond.Metadata = richFirst.Metadata
			metas := []artifact.ArtifactMeta{nilMeta, emptyMeta, richFirst, richSecond}
			ats := []time.Time{{}, time.Unix(1_720_040_000, 1).UTC(), time.Unix(1_720_040_000, 2).UTC(), time.Unix(1_720_040_000, 3).UTC()}
			results := make([]*artifact.ArtifactMeta, len(metas))
			for index, meta := range metas {
				mysqlCreateListIntegrationMeta(t, ctx, writer, meta)
				got, err := reader.MarkDeleted(ctx, meta.ArtifactRef, artifact.DeleteReason("container-reason"), ats[index])
				want := meta
				want.Status = artifact.ArtifactStatusDeleted
				want.DeletedAt = ats[index]
				want.DeleteReason = artifact.DeleteReason("container-reason")
				want.PurgeStatus = artifact.PurgeStatusPending
				want.PurgedAt = time.Time{}
				if err != nil || !reflect.DeepEqual(got, &want) {
					t.Fatalf("MarkDeleted(container %d) = (%#v, %v), want %#v", index, got, err, &want)
				}
				results[index] = got
			}
			results[2].Preview.Fields["nested"].([]any)[0].([]string)[0] = "mutated"
			results[2].DerivedFrom[0].ArtifactRef = "mutated"
			results[2].Metadata["key"] = "mutated"
			if results[3].Preview.Fields["nested"].([]any)[0].([]string)[0] != "original" || results[3].DerivedFrom[0].ArtifactRef != "source" || results[3].Metadata["key"] != "original" {
				t.Fatalf("MarkDeleted rich rows share containers: %#v", results)
			}
			repeated, err := writer.MarkDeleted(ctx, richFirst.ArtifactRef, artifact.DeleteReasonCleanup, time.Now())
			if err != nil || repeated.Preview.Fields["nested"].([]any)[0].([]string)[0] != "original" || repeated.DerivedFrom[0].ArtifactRef != "source" || repeated.Metadata["key"] != "original" {
				t.Fatalf("MarkDeleted repeated rich row = (%#v, %v), want isolated", repeated, err)
			}
		})
	})
}

func TestMySQLMetadataStorePurgeIntegration(t *testing.T) {
	runMySQLIntegrationTest(t, func(ctx context.Context, db *sql.DB) {
		secondDB := openMySQLIntegrationDB(t, ctx)
		writer := mysqlMustNewIntegrationStore(t, db, sha256.Sum256)
		reader := mysqlMustNewIntegrationStore(t, secondDB, sha256.Sum256)
		meta := mysqlCreateIntegrationMeta("purge-lifecycle")
		meta.Status = artifact.ArtifactStatusReady
		meta.DeletedAt = time.Time{}
		meta.DeleteReason = ""
		meta.PurgeStatus = ""
		meta.PurgedAt = time.Time{}
		created, err := writer.Create(ctx, meta, "purge-lifecycle-key")
		if err != nil {
			t.Fatalf("Create() purge integration error = %v", err)
		}

		deletedAt := time.Date(2026, 7, 14, 13, 14, 15, 123456789, time.UTC)
		tombstone, pending, err := writer.RequestPurge(ctx, created.ArtifactRef, artifact.DeleteReasonUser, deletedAt)
		if err != nil {
			t.Fatalf("RequestPurge() integration error = %v", err)
		}
		if tombstone.PurgeStatus != artifact.PurgeStatusPending || pending.Status != artifact.PurgeStatusPending || pending.LeaseVersion != 0 {
			t.Fatalf("RequestPurge() integration = (%#v, %#v), want pending tombstone/job", tombstone, pending)
		}
		listed, err := reader.ListPendingPurges(ctx, artifact.PurgeQuery{
			TenantID: meta.TenantID, SessionID: meta.SessionID, RunID: meta.RunID,
			ReadyAt: deletedAt, Limit: 1000,
		})
		if err != nil || len(listed) != 1 || listed[0].ArtifactRef != meta.ArtifactRef {
			t.Fatalf("ListPendingPurges() integration = (%#v, %v), want one exact job", listed, err)
		}

		claimAt := deletedAt.Add(time.Second)
		lease, claimed, err := reader.ClaimPurge(ctx, meta.ArtifactRef, "integration-worker-1", claimAt, claimAt.Add(time.Minute))
		if err != nil || !claimed || lease.LeaseVersion != 1 {
			t.Fatalf("first ClaimPurge() integration = (%#v, %t, %v), want version 1", lease, claimed, err)
		}
		failedAt := claimAt.Add(time.Second)
		retrying, err := writer.MarkPurgeFailed(ctx, meta.ArtifactRef, "integration-worker-1", lease.LeaseVersion, "injected retry", failedAt)
		if err != nil || retrying.Status != artifact.PurgeStatusRetrying || retrying.Attempts != 1 {
			t.Fatalf("MarkPurgeFailed() integration = (%#v, %v), want retrying attempt 1", retrying, err)
		}

		secondClaimAt := failedAt.Add(time.Second)
		lease, claimed, err = reader.ClaimPurge(ctx, meta.ArtifactRef, "integration-worker-2", secondClaimAt, secondClaimAt.Add(time.Minute))
		if err != nil || !claimed || lease.LeaseVersion != 2 {
			t.Fatalf("second ClaimPurge() integration = (%#v, %t, %v), want version 2", lease, claimed, err)
		}
		purgedAt := secondClaimAt.Add(time.Second)
		purged, err := writer.MarkPurgeSucceeded(ctx, meta.ArtifactRef, "integration-worker-2", lease.LeaseVersion, purgedAt)
		if err != nil || purged.PurgeStatus != artifact.PurgeStatusPurged || !purged.PurgedAt.Equal(purgedAt) {
			t.Fatalf("MarkPurgeSucceeded() integration = (%#v, %v), want purged metadata", purged, err)
		}
		terminal, err := reader.GetPurgeJob(ctx, meta.ArtifactRef)
		if err != nil || terminal.Status != artifact.PurgeStatusPurged || terminal.LeaseVersion != 2 || terminal.Attempts != 1 {
			t.Fatalf("GetPurgeJob(terminal) integration = (%#v, %v), want purged version 2 attempt 1", terminal, err)
		}
	})
}

type mysqlDeleteIntegrationResult struct {
	meta *artifact.ArtifactMeta
	err  error
}

func mysqlAwaitDeleteIntegrationResult(
	t *testing.T,
	ctx context.Context,
	results <-chan mysqlDeleteIntegrationResult,
	label string,
) mysqlDeleteIntegrationResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-ctx.Done():
		t.Fatalf("timeout awaiting %s: %v", label, ctx.Err())
		return mysqlDeleteIntegrationResult{}
	}
}

func mysqlAwaitDeleteIntegrationSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("timeout awaiting %s: %v", label, ctx.Err())
	}
}

func mysqlReapDeleteIntegrationGoroutine(t *testing.T, done <-chan struct{}, started *atomic.Bool, label string) {
	t.Helper()
	if !started.Load() {
		return
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Errorf("timeout reaping %s", label)
	}
}

func mysqlListIntegrationMeta(suffix string) artifact.ArtifactMeta {
	meta := mysqlGetterMetaFixture()
	meta.ArtifactID = "list-integration-id-" + suffix
	meta.ArtifactRef = "list-integration-ref-" + suffix
	meta.StorageKey = "list-integration-key-" + suffix
	meta.CreatedAt = time.Unix(1_720_000_000, int64(len(suffix))).UTC()
	meta.ExpiresAt = time.Time{}
	meta.DeletedAt = time.Time{}
	meta.DeleteReason = ""
	meta.Status = artifact.ArtifactStatusReady
	return meta
}

func mysqlCreateListIntegrationMeta(
	t *testing.T,
	ctx context.Context,
	store *MySQLMetadataStore,
	meta artifact.ArtifactMeta,
) {
	t.Helper()
	got, err := store.Create(ctx, meta, "")
	if err != nil || got == nil || got.ArtifactID != meta.ArtifactID {
		t.Fatalf("Create(List seed %q) = (%#v, %v), want success", meta.ArtifactID, got, err)
	}
}

func mysqlRequireListIntegrationIDs(t *testing.T, got []artifact.ArtifactMeta, want ...string) {
	t.Helper()
	ids := make([]string, len(got))
	for index := range got {
		ids[index] = got[index].ArtifactID
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("List ArtifactIDs = %#v, want %#v", ids, want)
	}
}

func mysqlRequireIndependentIntegrationPoolsAndStores(
	t *testing.T,
	writerDB *sql.DB,
	readerDB *sql.DB,
	writer *MySQLMetadataStore,
	reader *MySQLMetadataStore,
) {
	t.Helper()
	if writerDB == readerDB || writer == reader {
		t.Fatal("List integration fixture must use independent pools and stores")
	}
}

func mysqlNormalizeListIntegrationTimes(metas []artifact.ArtifactMeta) []artifact.ArtifactMeta {
	normalized := make([]artifact.ArtifactMeta, len(metas))
	for index := range metas {
		normalized[index] = metas[index]
		for source, destination := range map[*time.Time]*time.Time{
			&metas[index].CreatedAt: &normalized[index].CreatedAt,
			&metas[index].ExpiresAt: &normalized[index].ExpiresAt,
			&metas[index].DeletedAt: &normalized[index].DeletedAt,
		} {
			if source.IsZero() {
				*destination = time.Time{}
			} else {
				*destination = time.Unix(source.Unix(), int64(source.Nanosecond())).UTC()
			}
		}
	}
	return normalized
}

func mysqlIntegrationSaltedDigest(salt byte) func([]byte) []byte {
	return func(raw []byte) []byte {
		digest := sha256.Sum256(raw)
		digest[0] ^= salt
		return append([]byte(nil), digest[:]...)
	}
}

type mysqlCreateCall struct {
	store *MySQLMetadataStore
	meta  artifact.ArtifactMeta
	key   string
}

type mysqlCreateResult struct {
	meta *artifact.ArtifactMeta
	err  error
}

func mysqlRunConcurrentCreates(ctx context.Context, calls ...mysqlCreateCall) []mysqlCreateResult {
	start := make(chan struct{})
	results := make([]mysqlCreateResult, len(calls))
	var wait sync.WaitGroup
	wait.Add(len(calls))
	for index := range calls {
		index := index
		go func() {
			defer wait.Done()
			<-start
			results[index].meta, results[index].err = calls[index].store.Create(ctx, calls[index].meta, calls[index].key)
		}()
	}
	close(start)
	wait.Wait()
	return results
}

func mysqlRequireOneCreateWinnerAndConflict(t *testing.T, results []mysqlCreateResult) (winnerIndex int, loserIndex int) {
	t.Helper()
	var successes int
	var conflicts int
	for index, result := range results {
		if result.err == nil {
			if result.meta == nil {
				t.Fatalf("Create result %d succeeded with nil metadata", index)
			}
			successes++
			winnerIndex = index
			continue
		}
		if result.meta != nil || !artifact.IsErrorCode(result.err, artifact.ErrConflict) {
			t.Fatalf("Create result %d = (%#v, %v), want only conflict for loser", index, result.meta, result.err)
		}
		conflicts++
		loserIndex = index
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("Create results = %#v, want one success and one conflict", results)
	}
	return winnerIndex, loserIndex
}

func mysqlRequireIntegrationNotFound(
	t *testing.T,
	get func() (*artifact.ArtifactMeta, error),
	label string,
) {
	t.Helper()
	got, err := get()
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("%s lookup = (%#v, %v), want not found", label, got, err)
	}
}

func mysqlMustNewIntegrationStore(t *testing.T, db *sql.DB, digest mysqlDigestFunc) *MySQLMetadataStore {
	t.Helper()
	store, err := newMySQLMetadataStore(db, digest)
	if err != nil {
		t.Fatalf("newMySQLMetadataStore() error = %v", err)
	}
	return store
}

func mysqlCreateIntegrationMeta(suffix string) artifact.ArtifactMeta {
	meta := mysqlGetterMetaFixture()
	meta.ArtifactID = "artifact-id-" + suffix
	meta.ArtifactRef = "artifact-ref-" + suffix
	meta.StorageKey = "storage-key-" + suffix
	meta.Name = "name-" + suffix
	return meta
}

type mysqlCreateBarrier struct {
	mu      sync.Mutex
	want    int
	arrived int
	release chan struct{}
}

func newMySQLCreateBarrier(want int) *mysqlCreateBarrier {
	return &mysqlCreateBarrier{want: want, release: make(chan struct{})}
}

func (b *mysqlCreateBarrier) Wait() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.want {
		close(b.release)
	}
	release := b.release
	b.mu.Unlock()
	<-release
}

func TestClearMySQLIntegrationTablesAfterTestUsesIndependentContext(t *testing.T) {
	testCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if testCtx.Err() != context.Canceled {
		t.Fatalf("test context error = %v, want context.Canceled fixture", testCtx.Err())
	}

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(true)
	mock.ExpectExec("DELETE FROM artifact_purge_job").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM artifact_metadata").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM artifact_metadata_key_lock").WillReturnResult(sqlmock.NewResult(0, 0))

	if err := clearMySQLIntegrationTablesAfterTest(testCtx, db); err != nil {
		t.Fatalf("clearMySQLIntegrationTablesAfterTest() error = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("clearMySQLIntegrationTablesAfterTest() DELETE order/content: %v", err)
	}
}

func runMySQLIntegrationTest(t *testing.T, test func(context.Context, *sql.DB)) {
	t.Helper()

	mysqlIntegrationFixedTablesMu.Lock()
	defer mysqlIntegrationFixedTablesMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := openMySQLIntegrationDB(t, ctx)

	if err := InitializeMySQLSchema(ctx, db); err != nil {
		t.Fatalf("InitializeMySQLSchema() setup error = %v", err)
	}
	if err := clearMySQLIntegrationTables(ctx, db); err != nil {
		t.Fatalf("clear MySQL integration tables before test: %v", err)
	}
	defer func() {
		if err := clearMySQLIntegrationTablesAfterTest(ctx, db); err != nil {
			t.Errorf("clear MySQL integration tables after test: %v", err)
		}
	}()

	test(ctx, db)
}

func openMySQLIntegrationDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()

	config, configured, err := mysqlIntegrationConfig(os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !configured {
		t.Skip("structured MySQL integration fixture environment is not set")
	}
	if err := frameworkmysql.DBInit(config); err != nil {
		t.Fatalf("framework/mysql.DBInit() error = %v", err)
	}
	db := frameworkmysql.GetDB(config.Name)
	if db == nil {
		t.Fatalf("framework/mysql.GetDB(%q) returned nil", config.Name)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("MySQL integration DB Close() error = %v", err)
		}
	})

	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatalf("SELECT VERSION() error = %v", err)
	}
	trimmedVersion := strings.TrimSpace(version)
	if strings.Contains(strings.ToLower(trimmedVersion), "mariadb") || !strings.HasPrefix(trimmedVersion, "8.") {
		t.Fatalf("SELECT VERSION() = %q, want MySQL 8 and not MariaDB", version)
	}
	return db
}

func mysqlIntegrationConfig(lookup func(string) (string, bool)) (frameworkmysql.Config, bool, error) {
	environmentNames := [...]string{
		mysqlIntegrationHostEnvironment,
		mysqlIntegrationPortEnvironment,
		mysqlIntegrationUserEnvironment,
		mysqlIntegrationPasswordEnvironment,
		mysqlIntegrationDatabaseEnvironment,
	}
	values := make(map[string]string, len(environmentNames))
	for _, name := range environmentNames {
		if value, ok := lookup(name); ok {
			values[name] = value
		}
	}
	if len(values) == 0 {
		return frameworkmysql.Config{}, false, nil
	}
	if len(values) != len(environmentNames) {
		return frameworkmysql.Config{}, false, errInvalidMySQLIntegrationFixture
	}

	host := strings.TrimSpace(values[mysqlIntegrationHostEnvironment])
	user := strings.TrimSpace(values[mysqlIntegrationUserEnvironment])
	database := strings.TrimSpace(values[mysqlIntegrationDatabaseEnvironment])
	port, err := strconv.Atoi(strings.TrimSpace(values[mysqlIntegrationPortEnvironment]))
	if host == "" || user == "" || database == "" || err != nil || port < 1 || port > 65535 {
		return frameworkmysql.Config{}, false, errInvalidMySQLIntegrationFixture
	}
	return frameworkmysql.Config{
		Name:         "artifact-test",
		DBName:       database,
		User:         user,
		Password:     values[mysqlIntegrationPasswordEnvironment],
		Host:         host,
		Port:         port,
		Charset:      "utf8mb4",
		MaxOpenConns: 4,
		MaxIdleConns: 4,
	}, true, nil
}

func mysqlIntegrationLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func clearMySQLIntegrationTables(ctx context.Context, db *sql.DB) error {
	for _, table := range []string{"artifact_purge_job", "artifact_metadata", "artifact_metadata_key_lock"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return nil
}

// The test context is deliberately ignored because teardown must outlive its cancellation.
func clearMySQLIntegrationTablesAfterTest(_ context.Context, db *sql.DB) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), mysqlIntegrationCleanupTimeout)
	defer cancel()
	return clearMySQLIntegrationTables(cleanupCtx, db)
}

func insertMySQLIntegrationMetadata(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	meta artifact.ArtifactMeta,
	idempotencyKey string,
) {
	t.Helper()
	insertMySQLIntegrationMetadataWithDigest(t, ctx, db, meta, idempotencyKey, mysqlIntegrationDigest)
}

func insertMySQLIntegrationMetadataWithDigest(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	meta artifact.ArtifactMeta,
	idempotencyKey string,
	digest func([]byte) []byte,
) {
	t.Helper()
	row, _, err := prepareMySQLArtifactRow(meta)
	if err != nil {
		t.Fatalf("prepareMySQLArtifactRow() integration seed error = %v", err)
	}
	purgedAt := encodeNullableMySQLTime(meta.PurgedAt)
	args := []any{
		row.artifactID,
		digest(row.artifactID),
		row.artifactRef,
		digest(row.artifactRef),
		[]byte(idempotencyKey),
		digest([]byte(idempotencyKey)),
		row.tenantID,
		digest(row.tenantID),
		row.userID,
		row.sessionID,
		digest(row.sessionID),
		row.runID,
		digest(row.runID),
		row.stepID,
		row.ownerModule,
		digest(row.ownerModule),
		row.ownerID,
		digest(row.ownerID),
		row.artifactType,
		digest(row.artifactType),
		row.mimeType,
		row.name,
		row.sizeBytes,
		row.artifactHash,
		row.visibility,
		digest(row.visibility),
		row.storageBackend,
		row.storageKey,
		row.previewPayload,
		row.retentionPolicy,
		mysqlTestNullableInt64(row.expiresAt.Seconds),
		mysqlTestNullableInt64(row.expiresAt.Nanoseconds),
		row.createdBy,
		mysqlTestNullableInt64(row.createdAt.Seconds),
		mysqlTestNullableInt64(row.createdAt.Nanoseconds),
		row.status,
		row.derivedFromPayload,
		row.schemaVersion,
		mysqlTestNullableInt64(row.deletedAt.Seconds),
		mysqlTestNullableInt64(row.deletedAt.Nanoseconds),
		row.deleteReason,
		[]byte(meta.PurgeStatus),
		mysqlTestNullableInt64(purgedAt.Seconds),
		mysqlTestNullableInt64(purgedAt.Nanoseconds),
		row.metadataPayload,
	}
	const columns = "artifact_id, artifact_id_hash, artifact_ref, artifact_ref_hash, idempotency_key, idempotency_hash, tenant_id, tenant_id_hash, user_id, session_id, session_id_hash, run_id, run_id_hash, step_id, owner_module, owner_module_hash, owner_id, owner_id_hash, artifact_type, artifact_type_hash, mime_type, name, size_bytes, artifact_hash, visibility, visibility_hash, storage_backend, storage_key, preview_payload, retention_policy, expires_at_sec, expires_at_nano, created_by, created_at_sec, created_at_nano, status, derived_from_payload, schema_version, deleted_at_sec, deleted_at_nano, delete_reason, purge_status, purged_at_sec, purged_at_nano, metadata_payload"
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
	query := "INSERT INTO artifact_metadata (" + columns + ") VALUES (" + placeholders + ")"
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("insert integration metadata: %v", err)
	}
}

func mysqlIntegrationDigest(raw []byte) []byte {
	digest := sha256.Sum256(raw)
	return append([]byte(nil), digest[:]...)
}
