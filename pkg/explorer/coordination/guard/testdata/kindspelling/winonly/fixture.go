//go:build kindspelling_never

// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package winonly is excluded from every build, so go list does not list
// it, and writes a kind. Found in review of #671.
package winonly

import "github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"

func E() guard.Entity { return guard.Entity{Kind: 'W'} }
