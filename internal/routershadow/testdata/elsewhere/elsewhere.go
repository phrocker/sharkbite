// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package elsewhere stands for a package outside the guarded set that binds a
// controlled operation to a func variable; using it from a guarded package
// must be flagged.
package elsewhere

import "github.com/phrocker/shoal-oss/pkg/explorer/fleet"

// Enqueue is Enqueue, stored.
var Enqueue = (*fleet.DispatchService).Enqueue
