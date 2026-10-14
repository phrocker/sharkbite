package main

import (
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestAuthorizationRegistryConcurrentGetters(t *testing.T) {
	value := accumulo.NewAuthorizations([]byte("a"), []byte("b"))
	id, ok := authorizationValues.add(value)
	if !ok {
		t.Fatal("register authorizations")
	}
	defer authorizationValues.remove(id)

	const goroutines = 32
	const iterations = 1000
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for range goroutines {
		go func() {
			defer wait.Done()
			for range iterations {
				got, found := authorizationValues.get(id)
				if !found || !got.Contains([]byte("a")) || got.Len() != 2 {
					t.Errorf("unstable authorization lookup")
					return
				}
			}
		}()
	}
	wait.Wait()
}

func TestAuthorizationRegistryRemoveInvalidatesHandle(t *testing.T) {
	id, ok := authorizationValues.add(accumulo.NewAuthorizations())
	if !ok {
		t.Fatal("register authorizations")
	}
	if _, found := authorizationValues.remove(id); !found {
		t.Fatal("remove authorizations")
	}
	if _, found := authorizationValues.get(id); found {
		t.Fatal("removed authorizations remain reachable")
	}
}
