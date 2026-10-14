package protocol

import (
	"context"
	"strings"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"
)

const (
	testInstanceID = "11111111-2222-3333-4444-555555555555"
	testVersion    = "4.0.0-SNAPSHOT"
)

// pipeProtocols returns a (client, server) protocol pair that share an
// in-memory transport. Anything the client writes the server reads.
func pipeProtocols(t *testing.T, instanceID, accVersion string) (thrift.TProtocol, thrift.TProtocol) {
	t.Helper()
	buf := thrift.NewTMemoryBuffer()
	client := NewClientFactory(instanceID, accVersion).GetProtocol(buf)
	server := NewServerFactory(instanceID, accVersion).GetProtocol(buf)
	return client, server
}

func TestRoundtrip(t *testing.T) {
	ctx := context.Background()
	client, server := pipeProtocols(t, testInstanceID, testVersion)

	if err := client.WriteMessageBegin(ctx, "ping", thrift.CALL, 7); err != nil {
		t.Fatalf("client WriteMessageBegin: %v", err)
	}
	if err := client.WriteString(ctx, "hello"); err != nil {
		t.Fatalf("client WriteString: %v", err)
	}
	if err := client.WriteMessageEnd(ctx); err != nil {
		t.Fatalf("client WriteMessageEnd: %v", err)
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("client Flush: %v", err)
	}

	name, typeID, seqID, err := server.ReadMessageBegin(ctx)
	if err != nil {
		t.Fatalf("server ReadMessageBegin: %v", err)
	}
	if name != "ping" || typeID != thrift.CALL || seqID != 7 {
		t.Fatalf("ReadMessageBegin = (%q, %v, %d), want (\"ping\", CALL, 7)", name, typeID, seqID)
	}
	got, err := server.ReadString(ctx)
	if err != nil {
		t.Fatalf("server ReadString: %v", err)
	}
	if got != "hello" {
		t.Fatalf("payload = %q, want %q", got, "hello")
	}
	if err := server.ReadMessageEnd(ctx); err != nil {
		t.Fatalf("server ReadMessageEnd: %v", err)
	}
}

// writeBadHeader gives a test fine-grained control over the header bytes
// without going through AccumuloProtocol's writer. Returns the underlying
// TMemoryBuffer so the caller can hand it to a server-side AccumuloProtocol.
func writeBadHeader(t *testing.T, magic int32, protocolVersion int8, accVersion, instanceID string) *thrift.TMemoryBuffer {
	t.Helper()
	ctx := context.Background()
	buf := thrift.NewTMemoryBuffer()
	plain := thrift.NewTCompactProtocol(buf)
	if err := plain.WriteI32(ctx, magic); err != nil {
		t.Fatalf("WriteI32: %v", err)
	}
	if err := plain.WriteByte(ctx, protocolVersion); err != nil {
		t.Fatalf("WriteByte: %v", err)
	}
	if err := plain.WriteString(ctx, accVersion); err != nil {
		t.Fatalf("WriteString accVersion: %v", err)
	}
	if err := plain.WriteString(ctx, instanceID); err != nil {
		t.Fatalf("WriteString instanceID: %v", err)
	}
	// Append a real WriteMessageBegin so any post-header read attempt has
	// well-formed bytes to read — that way validation failures surface in
	// the header check, not in framing weirdness afterwards.
	if err := plain.WriteMessageBegin(ctx, "ping", thrift.CALL, 1); err != nil {
		t.Fatalf("WriteMessageBegin: %v", err)
	}
	if err := plain.WriteMessageEnd(ctx); err != nil {
		t.Fatalf("WriteMessageEnd: %v", err)
	}
	if err := plain.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf
}

func TestRejectsBadMagic(t *testing.T) {
	buf := writeBadHeader(t, int32(0x12345678), ProtocolVersion, testVersion, testInstanceID)
	server := NewServerFactory(testInstanceID, testVersion).GetProtocol(buf)
	_, _, _, err := server.ReadMessageBegin(context.Background())
	if err == nil {
		t.Fatal("expected error on bad magic, got nil")
	}
	if !strings.Contains(err.Error(), "magic mismatch") {
		t.Fatalf("expected magic-mismatch error, got: %v", err)
	}
}

func TestRejectsBadProtocolVersion(t *testing.T) {
	buf := writeBadHeader(t, MagicNumber, 99, testVersion, testInstanceID)
	server := NewServerFactory(testInstanceID, testVersion).GetProtocol(buf)
	_, _, _, err := server.ReadMessageBegin(context.Background())
	if err == nil {
		t.Fatal("expected error on bad protocol version, got nil")
	}
	if !strings.Contains(err.Error(), "incompatible protocol version") {
		t.Fatalf("expected protocol-version error, got: %v", err)
	}
}

func TestRejectsAccumuloMajorMinorMismatch(t *testing.T) {
	// Server expects 4.0; client sends 3.1 (different major.minor).
	buf := writeBadHeader(t, MagicNumber, ProtocolVersion, "3.1.0", testInstanceID)
	server := NewServerFactory(testInstanceID, testVersion).GetProtocol(buf)
	_, _, _, err := server.ReadMessageBegin(context.Background())
	if err == nil {
		t.Fatal("expected error on version mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "incompatible Accumulo versions") {
		t.Fatalf("expected accumulo-version error, got: %v", err)
	}
}

func TestAcceptsAccumuloMajorMinorMatchWithDifferentPatch(t *testing.T) {
	// Server is "4.0.0-SNAPSHOT" (major.minor "4.0"). Client sends "4.0.7"
	// (major.minor "4.0"). Patch differs but should be accepted.
	buf := writeBadHeader(t, MagicNumber, ProtocolVersion, "4.0.7", testInstanceID)
	server := NewServerFactory(testInstanceID, testVersion).GetProtocol(buf)
	if _, _, _, err := server.ReadMessageBegin(context.Background()); err != nil {
		t.Fatalf("expected no error on patch-only diff, got: %v", err)
	}
}

func TestRejectsInstanceIDMismatch(t *testing.T) {
	buf := writeBadHeader(t, MagicNumber, ProtocolVersion, testVersion, "00000000-0000-0000-0000-000000000000")
	server := NewServerFactory(testInstanceID, testVersion).GetProtocol(buf)
	_, _, _, err := server.ReadMessageBegin(context.Background())
	if err == nil {
		t.Fatal("expected error on instance id mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "instance id mismatch") {
		t.Fatalf("expected instance-id-mismatch error, got: %v", err)
	}
}

func TestMajorMinor(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"4.0.0-SNAPSHOT", "4.0", false},
		{"4.0.7", "4.0", false},
		{"3.1.0", "3.1", false},
		{"2.1", "2", false},
		{"nodot", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := majorMinor(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("majorMinor(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if !c.wantErr && got != c.want {
			t.Errorf("majorMinor(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}
