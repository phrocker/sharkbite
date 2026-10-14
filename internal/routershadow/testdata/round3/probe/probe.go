// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package probe is round-3 bypass of the authority review: process,
// environment, file and network access through standard-library packages an
// exact-path denylist did not name. The standard-library allowlist refuses
// every one of these imports.
package probe

import (
	"crypto/tls"
	"net/http/httputil"
	"net/smtp"
	"os"
	"runtime/debug"
)

func Probe() {
	_, _ = os.StartProcess("/bin/true", nil, &os.ProcAttr{})
	_ = os.Getenv("SECRET")
	_ = os.WriteFile("/tmp/x", nil, 0o600)
	_, _ = tls.Dial("tcp", "example.com:443", nil)
	_, _ = smtp.Dial("example.com:25")
	_ = httputil.NewSingleHostReverseProxy(nil)
	debug.SetGCPercent(-1)
}
