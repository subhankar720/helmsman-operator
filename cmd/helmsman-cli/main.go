package main

import (
	"fmt"
	"os"

	"github.com/subhankar720/helmsman-operator/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
