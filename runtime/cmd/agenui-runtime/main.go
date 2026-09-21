// Command agenui-runtime validates and executes a downloaded Runtime Package.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/AGenUI/agenui-studio/runtime"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: agenui-runtime <package.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err)
	}
	pkg, err := runtime.Load(raw)
	if err != nil {
		fail(err)
	}
	result, err := runtime.NewEngine(nil, nil).Execute(context.Background(), pkg, nil)
	if err != nil {
		fail(err)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(encoded))
}

func fail(err error) {
	var execution *runtime.ExecutionError
	if errors.As(err, &execution) {
		_ = json.NewEncoder(os.Stderr).Encode(execution)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
