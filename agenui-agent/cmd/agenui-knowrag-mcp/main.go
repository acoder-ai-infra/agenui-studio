// agenui-knowrag-mcp exposes the bundled public SQLite knowledge store through
// the Streamable HTTP MCP contract consumed by AGenUI agents.
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/knowragmcp"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	listen := flag.String("listen", ":18082", "MCP listen address")
	dsn := flag.String("sqlite", "var/knowrag.sqlite", "SQLite knowledge database path")
	seedDemo := flag.Bool("seed-demo", false, "insert public local demo knowledge and operator facts without overwriting existing rows")
	flag.Parse()
	if err := ensureSQLiteParent(*dsn); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite3", *dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	server, err := knowragmcp.New(db)
	if err != nil {
		log.Fatal(err)
	}
	if err := server.EnsureSchema(context.Background()); err != nil {
		log.Fatal(err)
	}
	if *seedDemo {
		if err := server.SeedDemo(context.Background()); err != nil {
			log.Fatal(err)
		}
		log.Printf("seeded public local KnowRAG demo facts")
	}
	log.Printf("AGenUI KnowRAG SQLite MCP listening on %s/mcp", *listen)
	if err := http.ListenAndServe(*listen, http.StripPrefix("/mcp", server.Handler())); err != nil {
		log.Fatal(err)
	}
}

func ensureSQLiteParent(dsn string) error {
	path := strings.TrimSpace(dsn)
	// URI and in-memory DSNs are deliberately passed through unchanged.
	if path == "" || path == ":memory:" || strings.Contains(path, ":") {
		return nil
	}
	directory := filepath.Dir(path)
	if directory == "." {
		return nil
	}
	return os.MkdirAll(directory, 0o755)
}
