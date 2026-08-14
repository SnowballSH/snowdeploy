// Command snowdeploy is the client for a snowdeployd daemon.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if _, err := fmt.Fprintf(os.Stdout, "snowdeploy %s\n", version); err != nil {
		os.Exit(1)
	}
}
