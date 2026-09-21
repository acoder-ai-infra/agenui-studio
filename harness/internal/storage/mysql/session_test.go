package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestMySQLSessionCreateRejectsOversizedIdentifiersBeforeInsert(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session storage.Session
	}{
		{name: "session_id", session: storage.Session{ID: strings.Repeat("s", 65), TenantID: "tenant"}},
		{name: "tenant_id", session: storage.Session{ID: "session", TenantID: strings.Repeat("t", 65)}},
		{name: "user_id", session: storage.Session{ID: "session", TenantID: "tenant", UserID: strings.Repeat("u", 65)}},
		{name: "agent_id", session: storage.Session{ID: "session", TenantID: "tenant", AgentID: strings.Repeat("a", 129)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			err = New(db).Stores().Sessions.Create(context.Background(), &tc.session)
			if !storage.IsErrorCode(err, storage.ErrInvalidArgument) {
				t.Fatalf("Create() error = %v, want ErrInvalidArgument", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("oversized identifier reached SQL: %v", err)
			}
		})
	}
}
