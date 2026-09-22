package main

import (
	"fmt"
	"os"

	"github.com/awked-com/node-textfile-tools/metrics"
)

func main() {
	if err := metrics.HostMain(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
