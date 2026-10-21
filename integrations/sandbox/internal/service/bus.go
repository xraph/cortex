//go:build linux

package service

import (
	"context"

	"github.com/godbus/dbus/v5"
)

type directBus struct{ connection *dbus.Conn }

func (b *directBus) Call(ctx context.Context, object dbus.ObjectPath, method string, args ...any) ([]any, error) {
	call := b.connection.Object("org.freedesktop.systemd1", object).CallWithContext(ctx, method, 0, args...)
	return call.Body, call.Err
}
func (b *directBus) Close() error { return b.connection.Close() }
