// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package executorref is the single definition of a valid executor
// reference: the name that ties a fleet descriptor to the executor that
// performs its actions, that an ATPL policy declares, that an attestation
// presentation names, that host configuration binds and trusts, and that an
// action-execution authorization decision is bound to (#391).
//
// Every one of those places calls ValidExecutorRef, so a reference one
// accepts is accepted by all of them, and a worker can always be bound to the
// reference its descriptor registered.
//
// A reference is drawn from a fixed ASCII charset,
// ^[A-Za-z0-9][A-Za-z0-9._:/@-]*$, and nothing else. References are compared
// byte for byte, and Unicode offers too many invisible and look-alike
// characters (variation selectors, fillers, overlay marks, other scripts'
// homoglyphs) for any filter over it to guarantee that two references which
// render alike are equal. Within the charset they are.
//
// The package imports only the standard library, so the fleet, the policy
// compiler, the public attestation contract and the authorization package
// may all depend on it.
package executorref

import "errors"

// MaxExecutorRefBytes bounds an executor reference in bytes.
const MaxExecutorRefBytes = 1024

// ValidExecutorRef reports why ref is not a valid executor reference, or nil.
// A valid reference is non-empty, at most MaxExecutorRefBytes, starts with an
// ASCII letter or digit, and continues with ASCII letters, digits and
// . _ : / @ - only. The error never echoes the reference.
func ValidExecutorRef(ref string) error {
	if ref == "" {
		return errors.New("executor reference is required")
	}
	if len(ref) > MaxExecutorRefBytes {
		return errors.New("executor reference is outside its bound")
	}
	if !alphanumeric(ref[0]) {
		return errors.New("executor reference must start with an ASCII letter or digit")
	}
	for index := 1; index < len(ref); index++ {
		if !allowed(ref[index]) {
			return errors.New(
				"executor reference may contain only ASCII letters, digits and . _ : / @ -")
		}
	}
	return nil
}

func alphanumeric(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func allowed(c byte) bool {
	switch c {
	case '.', '_', ':', '/', '@', '-':
		return true
	}
	return alphanumeric(c)
}
