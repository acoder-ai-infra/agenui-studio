// agenui-local-demo serves public local knowledge and Operator Detail fixtures
// on the existing source integration contracts. It is not an Agent runtime.
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

	"github.com/AGenUI/agenui-studio/agenui-agent/examples"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/knowragmcp"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/localadmin"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/operatorlocal"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18082", "local demo listen address")
	dsn := flag.String("sqlite", "var/local-demo.sqlite", "SQLite knowledge database path")
	initOnly := flag.Bool("init-only", false, "initialize local demo data and exit")
	flag.Parse()
	if err := ensureParent(*dsn); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite3", *dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	knowledge, err := knowragmcp.New(db)
	if err != nil {
		log.Fatal(err)
	}
	if err := knowledge.SeedDemo(context.Background()); err != nil {
		log.Fatal(err)
	}
	admin, err := localadmin.New(db)
	if err != nil {
		log.Fatal(err)
	}
	if err := admin.SeedDemo(context.Background()); err != nil {
		log.Fatal(err)
	}
	if *initOnly {
		log.Printf("AGenUI local demo initialized: %s", *dsn)
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.StripPrefix("/mcp", knowledge.Handler()))
	mux.HandleFunc("GET /demo/products", writeExampleJSON(examples.ProductsJSON))
	mux.HandleFunc("GET /demo/offers", writeExampleJSON(examples.OffersJSON))
	mux.HandleFunc("GET /demo/travel-status", writeExampleJSON(examples.TravelStatusJSON))
	if _, err := localadmin.Register(mux, db); err != nil {
		log.Fatal(err)
	}
	operatorlocal.Register(mux, db)
	log.Printf("AGenUI local demo: MCP http://%s/mcp; Operator Detail http://%s%s", *listen, *listen, operatorlocal.DetailPath)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func writeExampleJSON(value []byte) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(value)
	}
}

func ensureParent(dsn string) error {
	path := strings.TrimSpace(dsn)
	if path == "" || path == ":memory:" || strings.Contains(path, ":") {
		return nil
	}
	directory := filepath.Dir(path)
	if directory == "." {
		return nil
	}
	return os.MkdirAll(directory, 0o755)
}
