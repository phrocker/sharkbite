// Package metadata implements the from-scratch Go client that scans the
// accumulo.metadata table to discover tablet→tserver and tablet→RFile
// mappings for user tables. Bootstrap chain: ZK root-tablet location →
// scan root tablet for metadata-tablet locations + files → scan metadata
// tablets for user-table tablet locations + files.
package metadata
