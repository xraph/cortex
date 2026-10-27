//go:build !linux

package main

import (
	"log"
	"os"
)

func main() {
	log.Print("cortex: sandbox evidence authority requires a protected Linux host")
	os.Exit(1)
}
