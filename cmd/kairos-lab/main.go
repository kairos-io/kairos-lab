package main

import (
	"fmt"
	"os"

	"github.com/kairos-io/kairos-lab/internal/app"
)

var version = "dev"

func main() {
	if err := app.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
