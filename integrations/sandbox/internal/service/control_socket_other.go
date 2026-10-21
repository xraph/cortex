//go:build !linux

package service

import (
	"context"
	"net"
)

func dialWorkerControl(context.Context, string) (*net.UnixConn, error) { return nil, errUnsupported }
