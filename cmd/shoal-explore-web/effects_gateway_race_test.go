// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

//go:build race

package main

// raceBuild: this test binary runs under the race detector, so the gateway
// binary the end-to-end suite builds is built with it too.
const raceBuild = true
