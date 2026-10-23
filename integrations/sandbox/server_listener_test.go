package sandboxbroker

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type serverListenerFixture struct {
	accept func() (net.Conn, error)
	close  func() error
}

func (l *serverListenerFixture) Accept() (net.Conn, error) { return l.accept() }
func (l *serverListenerFixture) Close() error              { return l.close() }
func (l *serverListenerFixture) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func TestServerListenerClosesRawConnectionsBeforeTLSShutdown(t *testing.T) {
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	listener := &serverListener{Listener: &serverListenerFixture{accept: func() (net.Conn, error) { return raw, nil }, close: func() error { return nil }}, slots: make(chan struct{}, 1), closed: make(chan struct{})}
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err = peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err = peer.Read(data[:]); !errors.Is(err, io.EOF) {
		t.Fatal("raw connection survived listener shutdown", err)
	}
}

func TestServerListenerRejectsAcceptCompletingDuringClose(t *testing.T) {
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	listener := &serverListener{Listener: &serverListenerFixture{accept: func() (net.Conn, error) { close(entered); <-release; return raw, nil }, close: func() error { close(release); return nil }}, slots: make(chan struct{}, 1), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if connection != nil {
			_ = connection.Close()
		}
		done <- err
	}()
	<-entered
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal("accepted connection escaped shutdown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("accept did not join close")
	}
	if len(listener.slots) != 0 {
		t.Fatal("late accept leaked a connection slot")
	}
}
