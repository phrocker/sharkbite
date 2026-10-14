// Package agentmem implements a local agentic-memory policy layer over
// shoal's schema-agnostic ShoalEmbed gRPC contract.
//
// The package deliberately depends on the generated embedpb client and
// internal/graphschema conventions, not on shoal's embedded engine, iterator, or
// tablet internals. Production callers provide a GRPCStore connected to
// `shoal-embed serve`; tests and platform services can provide any EmbedStore.
//
// Event ids are UTC-millisecond sortable ids plus a monotonic counter, so
// evt:<id> rows preserve temporal order for row-range scans. The offline e2e
// tests use FakeStore rather than an in-process engine because importing the
// server would cross the policy/engine boundary this package is meant to enforce.
package agentmem
