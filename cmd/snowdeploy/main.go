package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	fmt.Fprintf(os.Stdout, "snowdeploy %s\n", version)
	os.Exit(0)
}
