package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"go.uber.org/zap"
)

func TestLoggerOptionValidation(t *testing.T) {
	zapLogger := zap.NewNop()
	customLogger := &recordingHarnessLogger{}
	var typedNilCustomLogger *recordingHarnessLogger
	tests := []struct {
		name    string
		opts    []Option
		wantNil bool
		wantErr bool
	}{
		{name: "not configured", wantNil: true},
		{name: "single zap logger", opts: []Option{WithLogger(zapLogger)}},
		{name: "single custom logger", opts: []Option{WithLogger(customLogger)}},
		{name: "nil logger", opts: []Option{WithLogger(nil)}, wantErr: true},
		{name: "typed nil zap logger", opts: []Option{WithLogger((*zap.Logger)(nil))}, wantErr: true},
		{name: "typed nil custom logger", opts: []Option{WithLogger(typedNilCustomLogger)}, wantErr: true},
		{name: "unsupported logger", opts: []Option{WithLogger(struct{}{})}, wantErr: true},
		{name: "duplicate logger", opts: []Option{WithLogger(zapLogger), WithLogger(customLogger)}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := &buildSettings{}
			for _, opt := range test.opts {
				opt(settings)
			}
			var kernelOptions kernel.KernelOptions
			err := applyLoggerOption(settings, &kernelOptions)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("applyLoggerOption() error = %v, want ErrInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyLoggerOption() error = %v", err)
			}
			if test.wantNil != (kernelOptions.Logger == nil) {
				t.Fatalf("Logger nil = %v, want %v", kernelOptions.Logger == nil, test.wantNil)
			}
		})
	}
}

func TestCustomLoggerAdapterPreservesContextLevelErrorAndFields(t *testing.T) {
	customLogger := &recordingHarnessLogger{}
	settings := &buildSettings{}
	WithLogger(customLogger)(settings)
	var kernelOptions kernel.KernelOptions
	if err := applyLoggerOption(settings, &kernelOptions); err != nil {
		t.Fatal(err)
	}

	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace-1",
		RunID:   "run-1",
	})
	wantErr := errors.New("boom")
	logger := kernelOptions.Logger.With(
		observability.String("service", "harness"),
		observability.String("env", "local"),
	)
	logger.Debug(ctx, "debug")
	logger.Info(ctx, "info")
	logger.Warn(ctx, "warn")
	logger.Error(ctx, "failed", wantErr, observability.Int("attempt", 2))

	if len(customLogger.records) != 4 {
		t.Fatalf("record count = %d, want 4", len(customLogger.records))
	}
	for i, want := range []LogLevel{LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError} {
		if got := customLogger.records[i].record.Level; got != want {
			t.Fatalf("record[%d].Level = %q, want %q", i, got, want)
		}
	}
	got := customLogger.records[3]
	if got.context != ctx {
		t.Fatal("custom logger did not receive the original context")
	}
	if got.record.Level != LogLevelError || got.record.Message != "failed" || !errors.Is(got.record.Error, wantErr) {
		t.Fatalf("record = %#v", got.record)
	}
	for key, want := range map[string]any{
		"service":  "harness",
		"env":      "local",
		"trace_id": "trace-1",
		"run_id":   "run-1",
		"attempt":  int64(2),
	} {
		if got.record.Fields[key] != want {
			t.Fatalf("Fields[%q] = %#v, want %#v; all fields = %#v", key, got.record.Fields[key], want, got.record.Fields)
		}
	}
}

type recordingHarnessLogger struct {
	records []recordedHarnessLog
}

func (l *recordingHarnessLogger) Log(ctx context.Context, record LogRecord) {
	l.records = append(l.records, recordedHarnessLog{context: ctx, record: record})
}

type recordedHarnessLog struct {
	context context.Context
	record  LogRecord
}
