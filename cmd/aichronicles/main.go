package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "aichronicles: no subcommand provided")
	os.Exit(2)
}
