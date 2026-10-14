// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// TestNoModelNetworkOrClock pins the package's direct imports: no network,
// no process execution, no model client, no clock. The catalog is embedded,
// and time is only what a record or Options.Now carries.
func TestNoModelNetworkOrClock(t *testing.T) {
	allowed := map[string]bool{
		"bytes": true, "embed": true, "encoding/hex": true, "encoding/json": true,
		"fmt": true, "math": true, "sort": true, "strconv": true, "strings": true,
		"sync": true, "time": true, "unicode": true, "unicode/utf8": true,
		"golang.org/x/text/feature/plural":                 true,
		"golang.org/x/text/language":                       true,
		"github.com/phrocker/shoal-oss/pkg/decision":       true,
		"github.com/phrocker/shoal-oss/pkg/explorer/fleet": true,
		"github.com/phrocker/shoal-oss/pkg/interaction":    true,
		"github.com/phrocker/shoal-oss/pkg/router":         true,
		"github.com/phrocker/shoal-oss/pkg/shoal":          true,
	}
	for _, f := range parseDir(t, ".") {
		for _, spec := range f.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if !allowed[path] {
				t.Errorf("%s imports %s", f.Name.Name, path)
			}
		}
	}
	for _, v := range clockViolations(parseDir(t, ".")) {
		t.Errorf("the renderer reads the clock: %s", v)
	}
}

func TestConcurrentRendering(t *testing.T) {
	r := New(nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			record := action(fleet.DispatchClaimed)
			record.ClaimFence = uint64(i + 1)
			if _, err := r.Action(record, Options{Now: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}
