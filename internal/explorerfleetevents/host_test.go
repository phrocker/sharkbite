/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorerfleetevents

import (
	"bytes"
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

func TestHostedServicesDispatchDependencies(t *testing.T) {
	registry := new(fleet.Service)
	publisher := new(ActionEventPublisher)
	services := &HostedServices{registry: registry, actionEvents: publisher}
	gotRegistry, gotPublisher, err := services.DispatchDependencies()
	if err != nil {
		t.Fatal(err)
	}
	if gotRegistry != registry || gotPublisher != publisher {
		t.Fatalf("dispatch dependencies = %p, %T", gotRegistry, gotPublisher)
	}
	if _, _, err := (*HostedServices)(nil).DispatchDependencies(); err == nil {
		t.Fatal("nil hosted services returned dispatch dependencies")
	}
}

func TestLoadOrCreateCursorKeyIsStableAndDomainSeparated(t *testing.T) {
	store := cursorKeyStore{key: bytes.Repeat([]byte{0x5a}, 32)}
	first, err := LoadOrCreateCursorKey(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCursorKey(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || !bytes.Equal(first, second) {
		t.Fatalf("derived keys are not stable: %x / %x", first, second)
	}
	if bytes.Equal(first, store.key) {
		t.Fatal("fleet cursor key was not domain-separated from the corpus key")
	}
	first[0] ^= 0xff
	again, err := LoadOrCreateCursorKey(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, again) {
		t.Fatal("returned cursor key aliases durable key state")
	}
}

func TestLoadOrCreateCursorKeyRejectsInvalidStores(t *testing.T) {
	for _, size := range []int{0, 31, 33} {
		if _, err := LoadOrCreateCursorKey(
			context.Background(), cursorKeyStore{key: make([]byte, size)},
		); err == nil {
			t.Fatalf("accepted cursor root key size %d", size)
		}
	}
	if _, err := LoadOrCreateCursorKey(context.Background(), nil); err == nil {
		t.Fatal("accepted nil cursor key store")
	}
	if _, err := LoadOrCreateCursorKey(nil, cursorKeyStore{key: make([]byte, 32)}); err == nil {
		t.Fatal("accepted nil context")
	}
}

type cursorKeyStore struct {
	key []byte
	err error
}

func (s cursorKeyStore) ChangeCursorSealKey(context.Context) ([]byte, error) {
	return append([]byte(nil), s.key...), s.err
}
