//go:build !linux

package main

import (
	"log"
	"os"
)

func main() { log.Print("cortex: input provisioning requires Linux"); os.Exit(1) }
