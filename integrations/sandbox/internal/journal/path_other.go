//go:build !linux && !darwin

package journal

import "fmt"

func preparePath(string) (string, error) {
	return "", fmt.Errorf("cortex: sandbox journals require Linux or macOS")
}
