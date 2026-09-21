package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestOpenTurnIdempotencyLookupDoesNotLockMissingKeyRange(t *testing.T) {
	if strings.Contains(strings.ToUpper(selectOpenTurnIdempotency), "FOR UPDATE") {
		t.Fatal("open-turn idempotency lookup must not gap-lock a missing key; the unique insert is the CAS")
	}
}

func TestMySQLOpenTurnRejectsOversizedUserIDBeforeStartingTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = New(db).OpenTurnStore().Commit(context.Background(), storage.OpenTurnCommand{
		Session: storage.Session{ID: "session", TenantID: "tenant", UserID: strings.Repeat("u", 65)},
	})
	if !storage.IsErrorCode(err, storage.ErrInvalidArgument) {
		t.Fatalf("Commit() error = %v, want ErrInvalidArgument", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("oversized UserID reached SQL: %v", err)
	}
}
