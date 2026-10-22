package service

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func TestAdmissionAcceptsActualDBusStructArrayEncoding(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	values := launchProperties(p, key)
	props := make(map[string]dbus.Variant, len(values))
	for name, value := range values {
		props[name] = dbus.MakeVariant(value)
	}
	type command struct {
		Path                                     string
		Args                                     []string
		Ignore                                   bool
		Started, StartedMono, Exited, ExitedMono uint64
		PID                                      uint32
		Code, Status                             int32
	}
	props["ExecStart"] = dbus.MakeVariant([]command{{Path: p.Worker.Path, Args: []string{p.Worker.Path, "--job", key.String(), "--config", p.Config.Path}}})
	props["LoadCredential"] = dbus.MakeVariant([]struct{ Name, Source string }{{"cortex-claim", p.StateDirectory + "/launch/" + key.String() + "/claim.json"}})
	props["Job"] = dbus.MakeVariant(struct {
		ID   uint32
		Path dbus.ObjectPath
	}{0, "/"})
	message := dbus.Message{Type: dbus.TypeMethodReply, Headers: map[dbus.HeaderField]dbus.Variant{dbus.FieldReplySerial: dbus.MakeVariant(uint32(1)), dbus.FieldSignature: dbus.MakeVariant(dbus.SignatureOf(props))}, Body: []any{props}}
	var encoded bytes.Buffer
	if err := message.EncodeTo(&encoded, binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	decoded, err := dbus.DecodeMessage(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	var received map[string]dbus.Variant
	if err = dbus.Store(decoded.Body, &received); err != nil {
		t.Fatal(err)
	}
	actual := make(map[string]any, len(received))
	for name, value := range received {
		actual[name] = value.Value()
	}
	if err = checkProperties(p, key, actual); err != nil {
		t.Fatalf("valid wire properties refused: %v", err)
	}
}
