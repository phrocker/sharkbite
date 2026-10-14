// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package eval

import "fmt"

// PreRegisteredTestDigest is the test split's Digest as recorded in
// docs/router-evaluation.md before the test split was first run. Until it is
// set, the test split cannot be run; once set, a test split that differs from
// what was registered cannot be run either.
const PreRegisteredTestDigest = "83b9411380288a69baf5cb5ffd97c4862c760921d96ecabee6a901d2c94ffab6"

// PreRegistered returns a split's cases. It is the only way to the test
// split, which it releases only when its digest equals the pre-registered
// digest.
func (cs *Cases) PreRegistered(split string) ([]Case, error) {
	if split != "test" {
		return cs.Split(split)
	}
	digest := cs.Digest("test")
	if PreRegisteredTestDigest == "" || digest != PreRegisteredTestDigest {
		return nil, fmt.Errorf("test split %s is not the pre-registered %q", digest, PreRegisteredTestDigest)
	}
	return cs.selectSplit("test"), nil
}
