//go:build !linux

package service

import "context"

func openBus(context.Context) (bus, error) { return nil, errUnsupported }
