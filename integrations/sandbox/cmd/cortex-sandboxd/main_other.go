//go:build !linux

package main

import (
	"log"
	"os"
)

func main() { log.Print("cortex: sandbox broker requires Linux"); os.Exit(1) }
