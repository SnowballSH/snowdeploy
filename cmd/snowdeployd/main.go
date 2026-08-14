// Command snowdeployd is the host deploy daemon.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if _, err := fmt.Fprintf(os.Stdout, "snowdeployd %s\n", version); err != nil {
		os.Exit(1)
	}
}
