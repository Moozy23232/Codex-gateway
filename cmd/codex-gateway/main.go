package main

import (
	"os"
	"path/filepath"

	"github.com/Moozy23232/Codex-gateway/internal/gateway"
)

func main() {
	if filepath.Base(os.Args[0]) == "codex" {
		os.Exit(gateway.ExecuteWrapped(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(gateway.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
