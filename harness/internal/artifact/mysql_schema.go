package artifact

import _ "embed"

//go:embed artifact_mysql.sql
var mysqlSchemaSQL string

// MySQLSchemaSQL returns the canonical final Artifact MySQL table definitions.
func MySQLSchemaSQL() string {
	return mysqlSchemaSQL
}
