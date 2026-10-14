// Package protocol implements AccumuloProtocol: a TCompactProtocol wrapper
// that reads/writes Accumulo's 5-byte custom header on every Thrift message
// (4 magic bytes 0x41435355 "ACCU" + 1 version byte 0x01). Reference:
// core/.../rpc/AccumuloProtocolFactory.java.
package protocol
