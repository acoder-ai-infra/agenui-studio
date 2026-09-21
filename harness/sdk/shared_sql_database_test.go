package harness

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

func TestSharedSQLDatabaseOptionValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tests := []struct {
		name    string
		opts    []Option
		want    *sql.DB
		wantErr bool
	}{
		{name: "not configured"},
		{name: "single external pool", opts: []Option{WithSharedSQLDatabase(db)}, want: db},
		{name: "nil external pool", opts: []Option{WithSharedSQLDatabase(nil)}, wantErr: true},
		{name: "duplicate registration", opts: []Option{WithSharedSQLDatabase(db), WithSharedSQLDatabase(db)}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := &buildSettings{}
			for _, opt := range test.opts {
				opt(settings)
			}
			var kernelOptions kernel.KernelOptions
			err := applySharedSQLDatabaseOption(settings, &kernelOptions)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("applySharedSQLDatabaseOption() error = %v, want ErrInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applySharedSQLDatabaseOption() error = %v", err)
			}
			if kernelOptions.SharedSQLDatabase != test.want {
				t.Fatalf("SharedSQLDatabase = %p, want %p", kernelOptions.SharedSQLDatabase, test.want)
			}
		})
	}
}

func TestSharedSQLDatabaseOptionForwardsToKernelOptions(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	settings := &buildSettings{}
	WithSharedSQLDatabase(db)(settings)
	kernelOptions := &kernel.KernelOptions{}
	if err := applySharedSQLDatabaseOption(settings, kernelOptions); err != nil {
		t.Fatalf("applySharedSQLDatabaseOption() error = %v", err)
	}
	if kernelOptions.SharedSQLDatabase != db {
		t.Fatalf("SharedSQLDatabase = %p, want %p", kernelOptions.SharedSQLDatabase, db)
	}
}
