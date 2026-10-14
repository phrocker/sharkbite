// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds runtime configuration sourced from environment variables
// injected by the Kubernetes downward API and Helm values.
type Config struct {
	// PodName is this pod's hostname (e.g. "tserver-0"). Set via POD_NAME.
	PodName string
	// PodNamespace is the Kubernetes namespace. Set via POD_NAMESPACE.
	PodNamespace string
	// TotalReplicas is the StatefulSet replica count. Set via TOTAL_REPLICAS.
	TotalReplicas int
	// GCSBucket is the destination bucket for sealed WAL uploads. Set via GCS_BUCKET.
	GCSBucket string
	// PeerPort is the TCP port used for sidecar-to-sidecar gRPC. Default 9710.
	PeerPort int
	// ServiceName is the headless service governing the StatefulSet.
	// Defaults to "tserver" which gives DNS names like tserver-0.tserver.<ns>.svc.cluster.local.
	ServiceName string
}

// Load reads configuration from environment variables.
// Required: POD_NAME, TOTAL_REPLICAS.
// Optional: POD_NAMESPACE (default "default"), GCS_BUCKET, PEER_PORT (default 9710),
// SERVICE_NAME (default "tserver").
func Load() (*Config, error) {
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		return nil, fmt.Errorf("POD_NAME environment variable is required")
	}

	replicasStr := os.Getenv("TOTAL_REPLICAS")
	if replicasStr == "" {
		return nil, fmt.Errorf("TOTAL_REPLICAS environment variable is required")
	}
	replicas, err := strconv.Atoi(replicasStr)
	if err != nil {
		return nil, fmt.Errorf("TOTAL_REPLICAS must be an integer: %w", err)
	}
	if replicas < 1 {
		return nil, fmt.Errorf("TOTAL_REPLICAS must be >= 1, got %d", replicas)
	}

	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "default"
	}

	peerPort := 9710
	if pp := os.Getenv("PEER_PORT"); pp != "" {
		peerPort, err = strconv.Atoi(pp)
		if err != nil {
			return nil, fmt.Errorf("PEER_PORT must be an integer: %w", err)
		}
	}

	svcName := os.Getenv("PEER_SERVICE_NAME")
	if svcName == "" {
		svcName = os.Getenv("SERVICE_NAME")
	}
	if svcName == "" {
		svcName = "tserver"
	}

	return &Config{
		PodName:       podName,
		PodNamespace:  ns,
		TotalReplicas: replicas,
		GCSBucket:     os.Getenv("GCS_BUCKET"),
		PeerPort:      peerPort,
		ServiceName:   svcName,
	}, nil
}

// OrdinalIndex extracts the ordinal number from the pod name.
// For "tserver-2" it returns 2. Returns -1 if the name has no trailing number.
func (c *Config) OrdinalIndex() int {
	idx := strings.LastIndex(c.PodName, "-")
	if idx < 0 || idx == len(c.PodName)-1 {
		return -1
	}
	n, err := strconv.Atoi(c.PodName[idx+1:])
	if err != nil {
		return -1
	}
	return n
}

// MaxPeers is the number of peers each sidecar replicates to.
// Quorum is 2-of-3 (local + 1 peer), so 2 replicas provides the needed redundancy.
const MaxPeers = 2

// Peers returns the list of peer DNS addresses for this sidecar.
// Each sidecar replicates to exactly 2 peers, spread evenly around the
// StatefulSet ring to minimize correlated failure risk. For 10 replicas
// where this pod is tserver-3, the peers are tserver-4 (offset +1) and
// tserver-8 (offset +5, halfway around the ring).
//
// Each peer address is a fully-qualified headless service DNS name:
//
//	tserver-4.tserver.<namespace>.svc.cluster.local:9710
func (c *Config) Peers() []string {
	ordinal := c.OrdinalIndex()
	if ordinal < 0 {
		return nil
	}

	baseName := c.PodName[:strings.LastIndex(c.PodName, "-")]
	n := c.TotalReplicas
	peerCount := MaxPeers
	if n-1 < peerCount {
		peerCount = n - 1
	}

	// Spread peers evenly: for 2 peers, offsets are 1 and N/2.
	// This ensures a single-zone failure can't take out both peers.
	offsets := make([]int, 0, peerCount)
	if peerCount >= 1 {
		offsets = append(offsets, 1)
	}
	if peerCount >= 2 {
		half := n / 2
		if half <= 1 {
			half = 2 // avoid colliding with offset 1
		}
		offsets = append(offsets, half)
	}

	var peers []string
	for _, off := range offsets {
		peerOrdinal := (ordinal + off) % n
		peerDNS := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local:%d",
			baseName, peerOrdinal, c.ServiceName, c.PodNamespace, c.PeerPort)
		peers = append(peers, peerDNS)
	}
	return peers
}

// PeerAddress returns the FQDN address for the peer with the given ordinal.
func (c *Config) PeerAddress(ordinal int) string {
	baseName := c.PodName[:strings.LastIndex(c.PodName, "-")]
	return fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local:%d",
		baseName, ordinal, c.ServiceName, c.PodNamespace, c.PeerPort)
}
