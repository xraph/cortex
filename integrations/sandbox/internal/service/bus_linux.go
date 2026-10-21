package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func openBus(ctx context.Context) (bus, error) {
	if os.Geteuid() != 0 {
		return nil, errUnsupported
	}
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	socket, err := (&net.Dialer{}).DialContext(startup, "unix", "/run/systemd/private")
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(startup, func() { _ = socket.Close() })
	defer stop()
	unixSocket, ok := socket.(*net.UnixConn)
	if !ok {
		socket.Close()
		return nil, fmt.Errorf("cortex: systemd requires a Unix socket")
	}
	raw, err := unixSocket.SyscallConn()
	if err != nil {
		socket.Close()
		return nil, err
	}
	var credential *unix.Ucred
	var peerErr error
	if err = raw.Control(func(fd uintptr) {
		credential, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || peerErr != nil {
		socket.Close()
		return nil, errors.Join(err, peerErr)
	}
	if credential == nil || credential.Uid != 0 || credential.Pid != 1 {
		socket.Close()
		return nil, fmt.Errorf("cortex: systemd socket is not owned by root PID 1")
	}
	connection, err := dbus.NewConn(socket)
	if err != nil {
		socket.Close()
		return nil, err
	}
	if err = connection.Auth([]dbus.Auth{dbus.AuthExternal("0")}); err != nil {
		return nil, errors.Join(err, connection.Close())
	}
	if !stop() || startup.Err() != nil {
		return nil, errors.Join(startup.Err(), connection.Close(), fmt.Errorf("cortex: systemd connection cancelled"))
	}
	return &directBus{connection: connection}, nil
}
