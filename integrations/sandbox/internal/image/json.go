//go:build linux || darwin

package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Go's ordinary decoder keeps the last duplicate member and matches struct
// fields without case sensitivity. Reject both sources of ambiguous metadata
// before decoding into OCI structs. Bounds also cover ignored annotations.
func unambiguousJSON(body []byte) error {
	if !utf8.Valid(body) {
		return fmt.Errorf("cortex: image metadata must be UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	remaining := 100_000
	if err := jsonValue(decoder, 0, &remaining); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("cortex: trailing image metadata")
	}
	return nil
}

func jsonValue(decoder *json.Decoder, depth int, remaining *int) error {
	*remaining--
	if depth > 64 || *remaining < 0 {
		return fmt.Errorf("cortex: image metadata exceeds structural budget")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			member, readErr := decoder.Token()
			if readErr != nil {
				return readErr
			}
			key, ok := member.(string)
			if !ok {
				return fmt.Errorf("cortex: invalid image metadata member")
			}
			folded := strings.Map(foldRune, key)
			if _, duplicate := seen[folded]; duplicate {
				return fmt.Errorf("cortex: ambiguous image metadata member %q", key)
			}
			seen[folded] = struct{}{}
			if valueErr := jsonValue(decoder, depth+1, remaining); valueErr != nil {
				return valueErr
			}
		}
	case '[':
		for decoder.More() {
			if valueErr := jsonValue(decoder, depth+1, remaining); valueErr != nil {
				return valueErr
			}
		}
	default:
		return fmt.Errorf("cortex: invalid image metadata structure")
	}
	_, err = decoder.Token()
	return err
}

// Match the Unicode case equivalence used by encoding/json's struct lookup.
func foldRune(r rune) rune {
	least := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		if next < least {
			least = next
		}
	}
	return least
}
