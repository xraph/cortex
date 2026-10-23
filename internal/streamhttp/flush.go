// Package streamhttp shares HTTP streaming behavior across transport adapters.
package streamhttp

import "net/http"

// Flush preserves wrapper flush behavior, then checks an error-aware inner writer.
// Forge's http.Flusher wrappers cannot return socket errors to ResponseController.
// A second flush of the inner writer is empty on success and observes sticky I/O
// failures without bypassing compression or before-write callbacks.
func Flush(writer http.ResponseWriter) error {
	if err := http.NewResponseController(writer).Flush(); err != nil {
		return err
	}
	type errorFlusher interface{ FlushError() error }
	if _, ok := writer.(errorFlusher); ok {
		return nil
	}
	current := writer
	for range 32 {
		wrapper, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		current = wrapper.Unwrap()
		if flusher, ok := current.(errorFlusher); ok {
			return flusher.FlushError()
		}
	}
	return http.ErrNotSupported
}

// CanFlush checks the response-controller flush capability without writing a
// status line. It follows wrappers with the same bounded traversal as Flush.
func CanFlush(writer http.ResponseWriter) bool {
	for range 32 {
		switch current := writer.(type) {
		case interface{ FlushError() error }:
			return true
		case http.Flusher:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			writer = current.Unwrap()
		default:
			return false
		}
	}
	return false
}
