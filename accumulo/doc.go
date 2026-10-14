// Package accumulo provides the public client bootstrap API for connecting
// Shoal-backed applications to Apache Accumulo.
//
// The current implementation targets the Accumulo 4 protocol and discovery
// layout. The connector supports namespace and table discovery, split
// listing, namespace/table listing and existence checks, scanners, mutation
// construction, and bounded batch writing; mutating administration APIs
// build on these stable boundaries.
package accumulo
