package main

import (
	"os"

	"github.com/Moozy23232/Codex-gateway/internal/gateway"
)

func main() { os.Exit(gateway.Execute(os.Args[1:], os.Stdout, os.Stderr)) }
