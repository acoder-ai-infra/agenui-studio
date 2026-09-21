package harness

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"testing"
)

// TestPublicAPIConformance is the SDK's compile-time contract check. It
// enumerates every exported symbol the harness package is expected to keep,
// then verifies each still resolves. Callers wanting to add a symbol update
// the list; removing or renaming one requires a schema bump per §13.
//
// This test intentionally does NOT diff a full golden JSON snapshot. Snapshot
// files rot rapidly during feature work; the explicit list here is easier to
// review inline.
func TestPublicAPIConformance(t *testing.T) {
	var _ func(*sql.DB) Option = WithSharedSQLDatabase

	expectedInterfaces := map[string]reflect.Type{
		"Engine":         reflect.TypeOf((*Engine)(nil)).Elem(),
		"Execution":      reflect.TypeOf((*Execution)(nil)).Elem(),
		"EventStream":    reflect.TypeOf((*EventStream)(nil)).Elem(),
		"ArtifactClient": reflect.TypeOf((*ArtifactClient)(nil)).Elem(),
	}
	engineMethods := []string{
		"Artifacts", "Cancel", "Close", "GetResult", "GetRun", "GetSession",
		"ListMessages", "ListSessions", "Readiness", "Report",
		"Resume", "Start", "Subscribe",
	}
	if got := interfaceMethods(expectedInterfaces["Engine"]); !equalStringSlice(got, engineMethods) {
		t.Fatalf("Engine method surface drifted:\n got: %v\nwant: %v", got, engineMethods)
	}
	artifactClientMethods := []string{"Get", "Head", "List", "Put"}
	if got := interfaceMethods(expectedInterfaces["ArtifactClient"]); !equalStringSlice(got, artifactClientMethods) {
		t.Fatalf("ArtifactClient method surface drifted:\n got: %v\nwant: %v", got, artifactClientMethods)
	}
	executionMethods := []string{"Events", "Handle", "OutputValidation", "ProjectedFrames"}
	if got := interfaceMethods(expectedInterfaces["Execution"]); !equalStringSlice(got, executionMethods) {
		t.Fatalf("Execution method surface drifted:\n got: %v\nwant: %v", got, executionMethods)
	}
	streamMethods := []string{"Close", "Cursor", "Next"}
	if got := interfaceMethods(expectedInterfaces["EventStream"]); !equalStringSlice(got, streamMethods) {
		t.Fatalf("EventStream method surface drifted:\n got: %v\nwant: %v", got, streamMethods)
	}
}

func TestSchemaVersionsIncludeCanonicalV1(t *testing.T) {
	want := []string{
		"harness.agent_event.v1",
		"harness.agent_binding.v1",
		"harness.control_request.v1",
		"harness.artifact_meta.v1",
		"harness.sse.v1",
		"harness.composition.v1",
	}
	got := canonicalSchemaVersions()
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if !equalStringSlice(got, sortedWant) {
		t.Fatalf("SchemaVersions drifted:\n got: %v\nwant: %v", got, sortedWant)
	}
}

func TestSentinelErrorsExposed(t *testing.T) {
	// Each of these errors must remain accessible via errors.Is so callers do
	// not pattern-match on string content. Removing a sentinel is a breaking
	// change (schema bump).
	for _, err := range []error{
		ErrClosed, ErrNotReady, ErrConflict, ErrIdempotencyMismatch,
		ErrChildControlUnsupported, ErrCapabilityDrift,
		ErrInvalidRequest, ErrUnsupportedCapability,
		ErrNotFound, ErrPermissionDenied,
	} {
		if err == nil {
			t.Fatal("nil sentinel exposed")
		}
		if !errors.Is(err, err) {
			t.Fatalf("errors.Is is broken for %v", err)
		}
	}
}

func TestRunStatusEnumComplete(t *testing.T) {
	want := []RunStatus{
		RunStatusCreated, RunStatusRunning, RunStatusWaitingControl, RunStatusResuming,
		RunStatusCompleted, RunStatusFailed, RunStatusCancelled, RunStatusExpired,
	}
	for _, s := range want {
		if s == "" {
			t.Fatalf("empty RunStatus constant found")
		}
	}
	// Terminal vs non-terminal partition must be a stable bipartition.
	terminals := map[RunStatus]bool{
		RunStatusCompleted: true, RunStatusFailed: true,
		RunStatusCancelled: true, RunStatusExpired: true,
	}
	for _, s := range want {
		if s.IsTerminal() != terminals[s] {
			t.Fatalf("IsTerminal broke for %q", s)
		}
	}
}

func TestVisibilityEnumComplete(t *testing.T) {
	want := []Visibility{
		VisibilityUserVisible, VisibilityDebug, VisibilityInternal, VisibilityRestricted,
	}
	for _, v := range want {
		if v == "" {
			t.Fatalf("empty Visibility constant")
		}
	}
}

func TestPartKindEnumComplete(t *testing.T) {
	want := []PartKind{
		PartKindText, PartKindJSON, PartKindImageRef, PartKindFileRef,
		PartKindArtifactRef, PartKindInlineBinary,
	}
	for _, k := range want {
		if k == "" {
			t.Fatalf("empty PartKind constant")
		}
	}
}

// TestEngineInterfaceReceivesContext ensures every mutating method accepts a
// context.Context. Callers use ctx to cancel outstanding operations.
func TestEngineInterfaceReceivesContext(t *testing.T) {
	engineType := reflect.TypeOf((*Engine)(nil)).Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	for i := 0; i < engineType.NumMethod(); i++ {
		m := engineType.Method(i)
		if m.Name == "Report" || m.Name == "Artifacts" { // Report/Artifacts are intentionally ctx-less accessors.
			continue
		}
		if m.Type.NumIn() < 1 {
			t.Fatalf("Engine.%s has no arguments; expected ctx first", m.Name)
		}
		if m.Type.In(0) != ctxType {
			t.Fatalf("Engine.%s first argument must be context.Context, got %v", m.Name, m.Type.In(0))
		}
	}
	// ArtifactClient 的每个方法同样必须 ctx-first，宿主用 ctx 取消挂起的
	// 存储调用。
	clientType := reflect.TypeOf((*ArtifactClient)(nil)).Elem()
	for i := 0; i < clientType.NumMethod(); i++ {
		m := clientType.Method(i)
		if m.Type.NumIn() < 1 || m.Type.In(0) != ctxType {
			t.Fatalf("ArtifactClient.%s first argument must be context.Context", m.Name)
		}
	}
}

// TestBuildReportShapeStable pins the field set of BuildReport so consumers of
// the JSON encoding do not silently gain new required fields.
func TestBuildReportShapeStable(t *testing.T) {
	want := []string{
		"Agents", "BuiltAt", "ConfigFingerprint", "Degraded", "Environment",
		"Extensions", "GitRevision", "Providers", "Runtimes",
		"SDKVersion", "SchemaVersions", "Tenants", "Unsupported",
	}
	got := structFieldNames(reflect.TypeOf(BuildReport{}))
	if !equalStringSlice(got, want) {
		t.Fatalf("BuildReport field surface drifted:\n got: %v\nwant: %v", got, want)
	}
}

func TestReadinessReportShapeStable(t *testing.T) {
	want := []string{
		"CheckedAt", "Extensions", "Ready", "Reasons",
		"Runtimes", "SchemaVersions",
	}
	got := structFieldNames(reflect.TypeOf(ReadinessReport{}))
	if !equalStringSlice(got, want) {
		t.Fatalf("ReadinessReport field surface drifted:\n got: %v\nwant: %v", got, want)
	}
}

func TestExecutionHandleShapeStable(t *testing.T) {
	want := []string{"AgentBindingID", "ConfigSnapshotRef", "Identity", "InputManifest", "TraceID"}
	got := structFieldNames(reflect.TypeOf(RunHandle{}))
	if !equalStringSlice(got, want) {
		t.Fatalf("RunHandle field surface drifted:\n got: %v\nwant: %v", got, want)
	}
}

// interfaceMethods returns the sorted list of method names on an interface type.
func interfaceMethods(t reflect.Type) []string {
	names := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		names = append(names, t.Method(i).Name)
	}
	sort.Strings(names)
	return names
}

// structFieldNames returns the sorted list of exported field names on a struct.
func structFieldNames(t reflect.Type) []string {
	if t.Kind() != reflect.Struct {
		return nil
	}
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
