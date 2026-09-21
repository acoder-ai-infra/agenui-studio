package app

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func callTool(t *testing.T, handler toolgateway.FunctionTool, ctx context.Context, id, args string) (*toolgateway.FunctionResult, error) {
	t.Helper()
	return handler(ctx, toolgateway.FunctionCall{ToolCallID: id, Arguments: json.RawMessage(args)})
}

func TestReadFileFallsBackToSessionArtifactByPath(t *testing.T) {
	store := artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	uploadCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", Role: artifact.ActorUser,
	})
	if _, err := store.Put(uploadCtx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1",
		OwnerModule: artifact.OwnerModuleProtocol, OwnerID: "msg-1",
		ArtifactType: artifact.ArtifactTypeFile, MimeType: "text/markdown", Name: "notes.md",
		Visibility: artifact.VisibilityUserVisible, Content: bytes.NewReader([]byte("# Notes")),
		RetentionPolicy: artifact.RetentionSessionTTL, CreatedBy: "user-a",
		Metadata: map[string]string{"source": "agent_chat"},
	}); err != nil {
		t.Fatal(err)
	}

	readCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-2", Role: artifact.ActorRuntime,
	})
	result, err := readFileFunctionHandler(store)(readCtx, toolgateway.FunctionCall{
		ToolCallID: "read-prev",
		Arguments:  json.RawMessage(`{"path":"notes.md"}`),
		Trace:      observability.TraceContext{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Path != "notes.md" || out.Content != "# Notes" {
		t.Fatalf("read_file output=%#v", out)
	}
}

func TestCalculatorFunction(t *testing.T) {
	handler := calculatorFunctionHandler()
	cases := []struct {
		expr string
		want float64
	}{
		{"(3 + 4) * 2", 14},
		{"2 ^ 3 ^ 2", 512}, // right-associative
		{"-5 + 3", -2},
		{"10 % 3", 1},
		{"1.5 * 2", 3},
	}
	for _, tc := range cases {
		result, err := callTool(t, handler, context.Background(), "calc", `{"expression":"`+tc.expr+`"}`)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		var out struct {
			Result float64 `json:"result"`
		}
		if err := json.Unmarshal(result.Data, &out); err != nil {
			t.Fatal(err)
		}
		if out.Result != tc.want {
			t.Fatalf("%s = %v, want %v", tc.expr, out.Result, tc.want)
		}
	}
	for _, bad := range []string{`{"expression":"1/0"}`, `{"expression":"2 +"}`, `{"expression":"foo()"}`, `{"expression":""}`} {
		if _, err := callTool(t, handler, context.Background(), "calc", bad); err == nil {
			t.Fatalf("expected error for %s", bad)
		}
	}
}

func TestGetCurrentTimeFunction(t *testing.T) {
	fixed := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	handler := getCurrentTimeFunctionHandler(func() time.Time { return fixed })

	result, err := callTool(t, handler, context.Background(), "time", `{"timezone":"Asia/Shanghai"}`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ISO8601     string `json:"iso8601"`
		UnixSeconds int64  `json:"unix_seconds"`
		Timezone    string `json:"timezone"`
	}
	if err := json.Unmarshal(result.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.UnixSeconds != fixed.Unix() || out.Timezone != "Asia/Shanghai" {
		t.Fatalf("unexpected time result: %#v", out)
	}
	if out.ISO8601 != "2026-07-21T20:00:00+08:00" {
		t.Fatalf("iso8601 = %q, want Shanghai offset", out.ISO8601)
	}
	if _, err := callTool(t, handler, context.Background(), "time", `{"timezone":"Not/AZone"}`); err == nil {
		t.Fatal("expected error for unknown timezone")
	}
}

func TestJSONExtractFunction(t *testing.T) {
	handler := jsonExtractFunctionHandler()
	doc := `{"json":{"data":{"items":[{"id":"a"},{"id":"b"}]}},"path":"data.items[1].id"}`
	result, err := callTool(t, handler, context.Background(), "jx", doc)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Found bool `json:"found"`
		Value any  `json:"value"`
	}
	if err := json.Unmarshal(result.Data, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Value != "b" {
		t.Fatalf("unexpected extract result: %#v", out)
	}

	// Missing path returns found=false, not an error.
	missing, err := callTool(t, handler, context.Background(), "jx", `{"json":{"a":1},"path":"a.b.c"}`)
	if err != nil {
		t.Fatal(err)
	}
	var missOut struct {
		Found bool `json:"found"`
	}
	_ = json.Unmarshal(missing.Data, &missOut)
	if missOut.Found {
		t.Fatal("expected found=false for missing path")
	}

	// JSON provided as an encoded string is also accepted.
	strDoc := `{"json":"{\"x\":42}","path":"x"}`
	strResult, err := callTool(t, handler, context.Background(), "jx", strDoc)
	if err != nil {
		t.Fatal(err)
	}
	var strOut struct {
		Value float64 `json:"value"`
	}
	_ = json.Unmarshal(strResult.Data, &strOut)
	if strOut.Value != 42 {
		t.Fatalf("string-encoded json extract = %v, want 42", strOut.Value)
	}
}

func TestRequestApprovalInterruptAndResume(t *testing.T) {
	handler := requestApprovalFunctionHandler()
	// First call interrupts for human input.
	_, err := callTool(t, handler, context.Background(), "ap", `{"action":"delete prod table","reason":"cleanup"}`)
	var interrupt *toolgateway.ToolInterruptedError
	if err == nil || !asToolInterrupt(err, &interrupt) {
		t.Fatalf("expected ToolInterruptedError, got %v", err)
	}
	// Resume with an approve decision.
	approved, err := handler(context.Background(), toolgateway.FunctionCall{
		ToolCallID: "ap",
		Resume: &toolgateway.ToolCallResume{
			WasInterrupted: true, IsResumeTarget: true,
			Payload: json.RawMessage(`{"approved":true,"comment":"ok"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Approved bool   `json:"approved"`
		Comment  string `json:"comment"`
	}
	if err := json.Unmarshal(approved.Data, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Approved || out.Comment != "ok" {
		t.Fatalf("unexpected approval result: %#v", out)
	}
	// A resume payload without a boolean decision is rejected.
	if _, err := handler(context.Background(), toolgateway.FunctionCall{
		ToolCallID: "ap",
		Resume:     &toolgateway.ToolCallResume{WasInterrupted: true, IsResumeTarget: true, Payload: json.RawMessage(`{"comment":"x"}`)},
	}); err == nil {
		t.Fatal("expected error for missing approved field")
	}
}

func asToolInterrupt(err error, target **toolgateway.ToolInterruptedError) bool {
	for err != nil {
		if ti, ok := err.(*toolgateway.ToolInterruptedError); ok {
			*target = ti
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestFileToolsReadListEdit(t *testing.T) {
	store := artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	handlers := configuredFunctionHandlers(store)
	ctx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", Role: artifact.ActorRuntime,
	})

	// Seed two files via write_file.
	if _, err := callTool(t, handlers["harness.write_file"], ctx, "w1", `{"path":"notes/a.md","content":"hello world"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, handlers["harness.write_file"], ctx, "w2", `{"path":"b.md","content":"second"}`); err != nil {
		t.Fatal(err)
	}

	// read_file by path.
	read, err := callTool(t, handlers["harness.read_file"], ctx, "r1", `{"path":"notes/a.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	var readOut struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(read.Data, &readOut)
	if readOut.Content != "hello world" {
		t.Fatalf("read_file content = %q", readOut.Content)
	}

	// list_files with prefix filter.
	list, err := callTool(t, handlers["harness.list_files"], ctx, "l1", `{"prefix":"notes/"}`)
	if err != nil {
		t.Fatal(err)
	}
	var listOut struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
		Count int `json:"count"`
	}
	_ = json.Unmarshal(list.Data, &listOut)
	if listOut.Count != 1 || listOut.Files[0].Path != "notes/a.md" {
		t.Fatalf("unexpected list_files result: %#v", listOut)
	}

	// edit_file str-replace, then read back.
	if _, err := callTool(t, handlers["harness.edit_file"], ctx, "e1", `{"path":"b.md","old_string":"second","new_string":"SECOND"}`); err != nil {
		t.Fatal(err)
	}
	edited, err := callTool(t, handlers["harness.read_file"], ctx, "r2", `{"path":"b.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(edited.Data, &readOut)
	if readOut.Content != "SECOND" {
		t.Fatalf("edit_file did not apply: %q", readOut.Content)
	}

	// edit_file with a non-unique old_string is rejected.
	if _, err := callTool(t, handlers["harness.write_file"], ctx, "w3", `{"path":"dup.md","content":"x x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, handlers["harness.edit_file"], ctx, "e2", `{"path":"dup.md","old_string":"x","new_string":"y"}`); err == nil {
		t.Fatal("expected error for non-unique old_string")
	}
	// Reading a missing file errors.
	if _, err := callTool(t, handlers["harness.read_file"], ctx, "r3", `{"path":"nope.md"}`); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestShippedToolCatalogBuilds ensures the public example catalog parses and
// every enabled tool resolves a handler and validates.
func TestShippedToolCatalogBuilds(t *testing.T) {
	var catalog toolCatalogConfig
	if err := decodeStrictYAML("../../testdata/local/catalogs/tools.yaml", &catalog); err != nil {
		t.Fatal(err)
	}
	store := artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	defs, _, err := buildConfiguredTools(catalog, store, nil)
	if err != nil {
		t.Fatalf("buildConfiguredTools: %v", err)
	}
	byName := map[string]bool{}
	for _, d := range defs {
		byName[d.Name] = true
	}
	for _, want := range []string{"ask_user", "request_approval", "read_file"} {
		if !byName[want] {
			t.Fatalf("built-in tool %s missing from built catalog", want)
		}
	}
}
