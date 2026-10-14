// Package protocol implements AccumuloProtocol — a TCompactProtocol wrapper
// that handles Accumulo's per-message header.
//
// Header layout (encoded through TCompactProtocol's own writers — these are
// varint-encoded ints + length-prefixed strings, NOT raw bytes):
//
//	i32    magic number (0x41434355 == "ACCU")
//	byte   protocol version (currently 1)
//	string Accumulo version (e.g. "4.0.0-SNAPSHOT")
//	string instance ID (UUID canonical)
//
// Reference: core/.../rpc/AccumuloProtocolFactory.java
package protocol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/apache/thrift/lib/go/thrift"
)

const (
	// MagicNumber is "ACCU" in ASCII (A=0x41 C=0x43 C=0x43 U=0x55).
	MagicNumber int32 = 0x41434355

	// ProtocolVersion bumps only when the header layout changes.
	ProtocolVersion int8 = 1
)

// AccumuloProtocol wraps TCompactProtocol with Accumulo's per-message
// header. Client instances prepend the header on every WriteMessageBegin;
// server instances read and validate it on every ReadMessageBegin.
//
// Use Factory.GetProtocol to construct one — direct instantiation skips
// the configuration the factory supplies.
type AccumuloProtocol struct {
	*thrift.TCompactProtocol

	instanceID      string
	accumuloVersion string
	isClient        bool
}

// WriteMessageBegin prepends the Accumulo header on client instances and
// then delegates to TCompactProtocol.
func (p *AccumuloProtocol) WriteMessageBegin(ctx context.Context, name string, typeID thrift.TMessageType, seqID int32) error {
	if p.isClient {
		if err := p.writeClientHeader(ctx); err != nil {
			return err
		}
	}
	return p.TCompactProtocol.WriteMessageBegin(ctx, name, typeID, seqID)
}

// ReadMessageBegin reads and validates the Accumulo header on server
// instances and then delegates to TCompactProtocol.
func (p *AccumuloProtocol) ReadMessageBegin(ctx context.Context) (string, thrift.TMessageType, int32, error) {
	if !p.isClient {
		if err := p.readAndValidateHeader(ctx); err != nil {
			// TSimpleServer swallows protocol errors silently; log
			// real failures (mismatched magic / version / instance
			// id) but suppress the EOF-on-first-byte case — that's
			// just the K8s readiness probe TCP-checking the port,
			// and it'd flood the log every probeInterval.
			if !isClosedConn(err) {
				fmt.Fprintf(os.Stderr, "shoal-protocol: header validation failed: %v\n", err)
			}
			return "", 0, 0, err
		}
	}
	return p.TCompactProtocol.ReadMessageBegin(ctx)
}

// isClosedConn discriminates the "client opened+closed without sending
// data" pattern (typical of K8s readiness TCP probes) from real protocol
// errors. The shape we want to suppress: ReadI32 hits EOF immediately
// because zero bytes were sent. Anything else (bad magic, partial reads,
// version mismatch) we want surfaced.
func isClosedConn(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "read magic: EOF") ||
		strings.Contains(msg, "Incorrect frame size (0)")
}

func (p *AccumuloProtocol) writeClientHeader(ctx context.Context) error {
	if err := p.TCompactProtocol.WriteI32(ctx, MagicNumber); err != nil {
		return fmt.Errorf("write magic: %w", err)
	}
	if err := p.TCompactProtocol.WriteByte(ctx, ProtocolVersion); err != nil {
		return fmt.Errorf("write protocol version: %w", err)
	}
	if err := p.TCompactProtocol.WriteString(ctx, p.accumuloVersion); err != nil {
		return fmt.Errorf("write accumulo version: %w", err)
	}
	if err := p.TCompactProtocol.WriteString(ctx, p.instanceID); err != nil {
		return fmt.Errorf("write instance id: %w", err)
	}
	return nil
}

func (p *AccumuloProtocol) readAndValidateHeader(ctx context.Context) error {
	magic, err := p.TCompactProtocol.ReadI32(ctx)
	if err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if magic != MagicNumber {
		return fmt.Errorf("invalid Accumulo protocol: magic mismatch, expected 0x%x, got 0x%x", MagicNumber, magic)
	}

	clientProtocolVersion, err := p.TCompactProtocol.ReadByte(ctx)
	if err != nil {
		return fmt.Errorf("read protocol version: %w", err)
	}
	if clientProtocolVersion != ProtocolVersion {
		return fmt.Errorf("incompatible protocol version: got %d, expected %d", clientProtocolVersion, ProtocolVersion)
	}

	clientAccumuloVersion, err := p.TCompactProtocol.ReadString(ctx)
	if err != nil {
		return fmt.Errorf("read accumulo version: %w", err)
	}
	if err := validateAccumuloVersion(clientAccumuloVersion, p.accumuloVersion); err != nil {
		return err
	}

	clientInstanceID, err := p.TCompactProtocol.ReadString(ctx)
	if err != nil {
		return fmt.Errorf("read instance id: %w", err)
	}
	if clientInstanceID != p.instanceID {
		return fmt.Errorf("instance id mismatch: server is %q, client sent %q", p.instanceID, clientInstanceID)
	}

	return nil
}

// validateAccumuloVersion enforces major.minor equality. Reference:
// AccumuloProtocolFactory.java:167-179, extractMajorMinorVersion at :184.
func validateAccumuloVersion(client, server string) error {
	cmm, err := majorMinor(client)
	if err != nil {
		return fmt.Errorf("client version: %w", err)
	}
	smm, err := majorMinor(server)
	if err != nil {
		return fmt.Errorf("server version: %w", err)
	}
	if cmm != smm {
		return fmt.Errorf("incompatible Accumulo versions: client %q, server %q (major.minor must match)", client, server)
	}
	return nil
}

// majorMinor returns the substring before the last dot. "4.0.0-SNAPSHOT" -> "4.0".
func majorMinor(version string) (string, error) {
	i := strings.LastIndex(version, ".")
	if i < 0 {
		return "", errors.New("invalid version format: " + version)
	}
	return version[:i], nil
}

// Factory produces AccumuloProtocol instances. It implements
// thrift.TProtocolFactory.
type Factory struct {
	instanceID      string
	accumuloVersion string
	isClient        bool
}

// NewClientFactory returns a Factory whose protocols write the Accumulo
// header before every outbound message.
func NewClientFactory(instanceID, accumuloVersion string) *Factory {
	return &Factory{instanceID: instanceID, accumuloVersion: accumuloVersion, isClient: true}
}

// NewServerFactory returns a Factory whose protocols read and validate the
// Accumulo header before every inbound message.
func NewServerFactory(instanceID, accumuloVersion string) *Factory {
	return &Factory{instanceID: instanceID, accumuloVersion: accumuloVersion, isClient: false}
}

// GetProtocol implements thrift.TProtocolFactory.
func (f *Factory) GetProtocol(t thrift.TTransport) thrift.TProtocol {
	return &AccumuloProtocol{
		TCompactProtocol: thrift.NewTCompactProtocol(t),
		instanceID:       f.instanceID,
		accumuloVersion:  f.accumuloVersion,
		isClient:         f.isClient,
	}
}
