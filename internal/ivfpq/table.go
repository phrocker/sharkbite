// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package ivfpq provides the IVF-PQ training, encoding, and query-time
// scoring machinery.  The wire formats are byte-compatible with veculo's
// IvfCentroids.java and VectorPQ.java (big-endian / Java DataOutput).
package ivfpq

import "fmt"

// Storage-layout constants mirroring veculo's IvfPqTable.java so that
// written indexes can be imported by the veculo platform without re-ingest.
const (
	IvfSuffix    = "_ivf"
	ConfigSuffix = "_ann_config"

	// ColFam is the column family used for IVF-PQ coded cells.
	ColFam = "V"
	// QualPQCode is the column qualifier for the PQ-encoded byte vector.
	QualPQCode = "_pq"
	// QualCodebookVersion is the column qualifier carrying the codebook version tag.
	QualCodebookVersion = "_centroidVersion"

	// ConfigColFam is the column family for codebook blobs in the config table.
	ConfigColFam = "blob"
	// ConfigQual is the column qualifier for codebook blobs.
	ConfigQual = "data"

	// ConfigRowActiveVersion is the config-table row key for the active codebook version.
	ConfigRowActiveVersion = "active_version"
	// ConfigRowLastTrainedRows is the config-table row key for the row count at last training.
	ConfigRowLastTrainedRows = "last_trained_rows"
)

// CentroidsRow returns the config-table row key for a Centroids blob at
// version v, e.g. "centroids_v1".
func CentroidsRow(v int32) string { return fmt.Sprintf("centroids_v%d", v) }

// PQRow returns the config-table row key for a VectorPQ blob at version v,
// e.g. "pq_v1".
func PQRow(v int32) string { return fmt.Sprintf("pq_v%d", v) }

// EmbeddingSpaceRow returns the config-table row carrying the stable identity
// of the vector space used to train codebook version v.
func EmbeddingSpaceRow(v int32) string {
	return fmt.Sprintf("embedding_space_v%d", v)
}

// VersionReservationRow is an immutable claim that codebook version v has
// been allocated. It is separate from codebook data so reservation and
// publication never write equal keys at equal timestamps.
func VersionReservationRow(v int32) string {
	return fmt.Sprintf("version_reserved_v%d", v)
}

// FormatClusterID formats a cluster identifier as a zero-padded 8-digit
// lowercase hex string, matching veculo's fixed-width cluster key convention.
func FormatClusterID(id int) string { return fmt.Sprintf("%08x", id) }

// RowKey builds an IVF table row key from a cluster id and a vertex id
// string, e.g. "00000003:evt:01H...".
func RowKey(clusterID int, vertexID string) string {
	return FormatClusterID(clusterID) + ":" + vertexID
}

// IvfTableName returns the name of the IVF coded-vector table for a graph.
func IvfTableName(graph string) string { return graph + IvfSuffix }

// ConfigTableName returns the name of the ANN configuration/codebook table for a graph.
func ConfigTableName(graph string) string { return graph + ConfigSuffix }

// ClusterPrefix returns the row-key prefix covering every vertex in one
// cluster, e.g. "00000003:". Scanning this prefix on the IVF table yields all
// coded vectors assigned to the cluster.
func ClusterPrefix(clusterID int) string { return FormatClusterID(clusterID) + ":" }

// ExtractVertexID recovers the original vertex id from a flat IVF row key
// produced by RowKey: everything after the first ':' separator. Returns the
// input unchanged when no separator is present.
func ExtractVertexID(rowKey string) string {
	for i := 0; i < len(rowKey); i++ {
		if rowKey[i] == ':' {
			return rowKey[i+1:]
		}
	}
	return rowKey
}
