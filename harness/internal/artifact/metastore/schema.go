package metastore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const mysqlSchemaStatementDelimiter = "\n-- harness:artifact-mysql-schema-statement --\n"

type mysqlSchemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func InitializeMySQLSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("metastore: initialize MySQL schema: database is required")
	}
	return initializeMySQLSchema(ctx, db)
}

func initializeMySQLSchema(ctx context.Context, execer mysqlSchemaExecer) error {
	if ctx == nil {
		return errors.New("metastore: initialize MySQL schema: context is required")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("metastore: initialize MySQL schema: %w", err)
	}

	statements := strings.Split(artifact.MySQLSchemaSQL(), mysqlSchemaStatementDelimiter)
	if len(statements) != 3 {
		return errors.New("metastore: initialize MySQL schema: embedded schema must contain exactly three statements")
	}
	for index, statement := range statements {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			return fmt.Errorf("metastore: initialize MySQL schema: embedded statement %d is empty", index+1)
		}
		if _, err := execer.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("metastore: initialize MySQL schema statement %d failed: %w", index+1, err)
		}
	}
	return nil
}
