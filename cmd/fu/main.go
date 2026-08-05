package main

import (
	"os"

	"github.com/cosensexyz/fu/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
