// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package api is the positive control for the API walker: an imported
// package whose exported API would hand the router a file, a raw pointer and
// a service. Each must be flagged.
package api

import (
	"os"
	"unsafe"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

type Holder struct {
	File *os.File
}

func Open() Holder { return Holder{} }

var Pointer unsafe.Pointer

func Registry() *fleet.Service { return nil }
