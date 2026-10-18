// Package store exposes optimization persistence ports for backend implementers.
package store

import "github.com/xraph/cortex/optimization"

// Store aliases the consumer-owned port without introducing an import cycle.
type Store = optimization.Repository

// Tx aliases the synchronous atomic transaction port.
type Tx = optimization.Transaction
