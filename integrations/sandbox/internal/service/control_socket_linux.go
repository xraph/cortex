package service

import (
	"context"
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"

	broker "github.com/xraph/cortex/integrations/sandbox"
)

func dialWorkerControl(ctx context.Context, path string) (*net.UnixConn, error) {
	if os.Geteuid() != 0 {
		return nil, errUnsupported
	}
	if err := inspectControlSocket(path, 0); err != nil {
		return nil, err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	socket, ok := connection.(*net.UnixConn)
	if !ok {
		return nil, errors.Join(broker.ErrDenied, connection.Close())
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return nil, errors.Join(err, socket.Close())
	}
	var identity *unix.Ucred
	var peerErr error
	err = raw.Control(func(fd uintptr) { identity, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err = errors.Join(err, peerErr, ctx.Err()); err != nil {
		return nil, errors.Join(err, socket.Close())
	}
	if identity == nil || identity.Pid <= 1 || identity.Uid != 0 || identity.Gid != 0 {
		return nil, errors.Join(broker.ErrDenied, socket.Close())
	}
	if err = inspectControlSocket(path, 0); err != nil {
		return nil, errors.Join(err, socket.Close())
	}
	return socket, nil
}
