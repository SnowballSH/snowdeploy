package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	fmt.Fprintf(os.Stdout, "snowdeployd %s\n", version)
	os.Exit(0)
}
