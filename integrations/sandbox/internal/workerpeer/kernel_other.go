//go:build !linux

package workerpeer

import (
	"context"
	"fmt"
	"net"
)

func openProcess(context.Context, *net.UnixConn) (peerProcess, error) {
	return nil, fmt.Errorf("cortex: worker peer authentication requires Linux")
}
