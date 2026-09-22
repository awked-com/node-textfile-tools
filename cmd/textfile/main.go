package main

import (
	"fmt"
	"os"

	"github.com/awked-com/node-textfile-tools/metrics"
)

func main() {
	if err := metrics.JobMain(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
