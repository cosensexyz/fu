package main

import (
	"os"

	"fu/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
