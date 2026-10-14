// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package tserver

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// canonicalTestUUID is a UUID in the only form Java's UUID.fromString accepts.
const canonicalTestUUID = "6f1b2c8e-6a4d-4c2e-9f3b-1d5e7a9c0b21"

// nonCanonicalUUIDs are the forms Go's uuid.Parse accepts and Java's
// UUID.fromString does not, plus the near-misses either would reject. Every
// one of them is a UUID this process could otherwise publish and the manager
// could not read.
func nonCanonicalUUIDs() map[string]string {
	return map[string]string{
		"bare hex":    strings.ReplaceAll(canonicalTestUUID, "-", ""),
		"urn":         "urn:uuid:" + canonicalTestUUID,
		"braced":      "{" + canonicalTestUUID + "}",
		"truncated":   canonicalTestUUID[:len(canonicalTestUUID)-1],
		"padded":      canonicalTestUUID + "0",
		"empty":       "",
		"not a uuid":  "shoal-tablet-server-one-two-three-four",
		"right shape": "6f1b2c8e-6a4d-4c2e-9f3b-1d5e7a9c0bZZ",
	}
}

// invalidResourceGroups are names Accumulo's ResourceGroupId.of throws on.
// Publishing one is not a cosmetic problem: ServiceLockData.parse builds a
// ResourceGroupId from every descriptor's group, so the throw takes the whole
// znode with it.
func invalidResourceGroups() []string {
	return []string{
		"",            // no name at all
		"../managers", // a traversal that would clean out of the tservers tree
		"1ingest",     // must open with a letter
		"_ingest",     // must open with a letter
		"ingest_",     // a separator must be followed by something
		"ing__est",    // separators do not double
		"ing-est",     // only underscore separates
		"ing est",     // no spaces
		"ing.est",     // no dots
		"ing/est",     // no path separators
		"ingest\n",    // no trailing newline, which Go's $ would otherwise let by
		"INGEST-EAST", // still only underscore separates
	}
}

// validResourceGroups are names Accumulo accepts, kept alongside the refusals
// so the grammar is not tightened past what a real deployment configures.
func validResourceGroups() []string {
	return []string{
		DefaultResourceGroup,
		"ingest",
		"i",
		"Ingest",
		"ingest_east",
		"ingest2",
		"i_1",
		"INGEST",
	}
}

// TestResourceGroupGrammarMatchesAccumulo pins the grammar itself against
// ResourceGroupId.GROUP_NAME_PATTERN, so a later relaxation has to be
// deliberate.
func TestResourceGroupGrammarMatchesAccumulo(t *testing.T) {
	for _, group := range validResourceGroups() {
		if !validResourceGroup(group) {
			t.Errorf("validResourceGroup(%q) = false, want true", group)
		}
	}
	for _, group := range invalidResourceGroups() {
		if validResourceGroup(group) {
			t.Errorf("validResourceGroup(%q) = true, want false", group)
		}
	}
}

func TestCompactorLockData(t *testing.T) {
	data, err := CompactorLockData(canonicalTestUUID, testAddress, "shoal_default")
	if err != nil {
		t.Fatalf("CompactorLockData: %v", err)
	}
	if len(data.Descriptors) != 1 {
		t.Fatalf("descriptors = %d, want 1", len(data.Descriptors))
	}
	for i, service := range []ThriftService{ServiceCompactor} {
		got := data.Descriptors[i]
		if got.UUID != canonicalTestUUID || got.Service != service ||
			got.Address != testAddress || got.Group != "shoal_default" {
			t.Fatalf("descriptor[%d] = %+v", i, got)
		}
	}
}

// TestServiceDescriptorRefusesAGroupAccumuloWouldReject covers a resource
// group arriving from configuration.
//
// The read side runs every group through ResourceGroupId.of, which throws on
// anything outside its grammar, so a group this process accepts and Accumulo
// does not is another way to publish a znode the manager cannot parse — the
// same failure a non-canonical UUID causes, reached through a different field.
func TestServiceDescriptorRefusesAGroupAccumuloWouldReject(t *testing.T) {
	for _, group := range invalidResourceGroups() {
		t.Run(group, func(t *testing.T) {
			data := ServiceLockData{Descriptors: []ServiceDescriptor{{
				UUID:    serverUUID,
				Service: ServiceClient,
				Address: testAddress,
				Group:   group,
			}}}
			if err := data.Validate(); !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("Validate: want ErrInvalidLockData, got %v", err)
			}
			encoded, err := data.Encode()
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("Encode: want ErrInvalidLockData, got %v", err)
			}
			if encoded != nil {
				t.Fatalf("Encode returned %q alongside its refusal", encoded)
			}
		})
	}
}

// TestTabletServerLockDataRefusesAGroupAccumuloWouldReject covers the same
// group arriving through the constructor a tablet server actually calls.
func TestTabletServerLockDataRefusesAGroupAccumuloWouldReject(t *testing.T) {
	for _, group := range invalidResourceGroups() {
		if group == "" {
			// An empty group means DefaultResourceGroup here, which is valid.
			continue
		}
		t.Run(group, func(t *testing.T) {
			if _, err := TabletServerLockData(serverUUID, testAddress, group,
				ServiceTabletServer); !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("want ErrInvalidLockData, got %v", err)
			}
		})
	}
	for _, group := range validResourceGroups() {
		t.Run("accepts "+group, func(t *testing.T) {
			if _, err := TabletServerLockData(serverUUID, testAddress, group,
				ServiceTabletServer); err != nil {
				t.Fatalf("TabletServerLockData(%q): %v", group, err)
			}
		})
	}
}

// TestTabletServerLockDataRefusesAnotherRolesService keeps this constructor
// honest about what it is for.
//
// The enum covers the manager, the coordinator, the garbage collector and the
// compactors as well, and every one of them is a service Known() accepts. A
// tablet-server lock advertising MANAGER would tell a client that the manager
// endpoint lives on this process, which does not implement it — a lie the
// manager itself has no reason to look for, since a tablet server publishing
// a manager service is not a case Accumulo produces.
func TestTabletServerLockDataRefusesAnotherRolesService(t *testing.T) {
	foreign := []ThriftService{"MANAGER", "COORDINATOR", "COMPACTOR", "GC",
		"FATE_CLIENT", "FATE_WORKER", "NONE"}
	for _, service := range foreign {
		t.Run(string(service), func(t *testing.T) {
			if !service.Known() {
				t.Fatalf("%s is meant to be a service Accumulo defines", service)
			}
			_, err := TabletServerLockData(serverUUID, testAddress, testGroup, service)
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("want ErrInvalidLockData, got %v", err)
			}
			if got := err.Error(); !strings.Contains(got, string(service)) {
				t.Fatalf("error %q does not name the refused service", got)
			}
		})
	}
	t.Run("alongside real ones", func(t *testing.T) {
		services := append(TabletServerServices(), "MANAGER")
		if _, err := TabletServerLockData(serverUUID, testAddress, testGroup,
			services...); !errors.Is(err, ErrInvalidLockData) {
			t.Fatalf("want ErrInvalidLockData, got %v", err)
		}
	})
	t.Run("unknown service", func(t *testing.T) {
		if _, err := TabletServerLockData(serverUUID, testAddress, testGroup,
			"SHOAL_SCAN"); !errors.Is(err, ErrInvalidLockData) {
			t.Fatalf("want ErrInvalidLockData, got %v", err)
		}
	})
}

// TestServiceDescriptorRefusesNonCanonicalUUID pins the UUID shape at the
// write side.
//
// Go's uuid.Parse is not a shape check: it also accepts a bare 32-character
// hex form, a "urn:uuid:" form, and a braced form. Java's UUID.fromString
// accepts none of them, and ServiceLockData is deserialized with it, so
// publishing one would not make the descriptor merely odd — it would make the
// whole lock znode unparseable and the server it names invisible to the
// manager. Refusing to encode is the only outcome that keeps the promise that
// validated payloads are manager-readable.
func TestServiceDescriptorRefusesNonCanonicalUUID(t *testing.T) {
	for name, value := range nonCanonicalUUIDs() {
		t.Run(name, func(t *testing.T) {
			descriptor := ServiceDescriptor{
				UUID:    value,
				Service: ServiceClient,
				Address: testAddress,
				Group:   testGroup,
			}
			err := descriptor.Validate()
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("Validate(%q) = %v, want ErrInvalidLockData", value, err)
			}
			if !strings.Contains(err.Error(), "36-character dashed form") {
				t.Fatalf("Validate(%q) = %q, want it to name the shape it wants", value, err)
			}
		})
	}
	descriptor := ServiceDescriptor{
		UUID:    canonicalTestUUID,
		Service: ServiceClient,
		Address: testAddress,
		Group:   testGroup,
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("the canonical form must still validate: %v", err)
	}
}

// TestTabletServerLockDataRefusesNonCanonicalUUID checks the constructor, not
// just the descriptor it builds: a caller that never touches ServiceDescriptor
// still must not be able to publish a UUID the manager cannot read.
func TestTabletServerLockDataRefusesNonCanonicalUUID(t *testing.T) {
	for name, value := range nonCanonicalUUIDs() {
		t.Run(name, func(t *testing.T) {
			_, err := TabletServerLockData(value, testAddress, testGroup, ServiceTabletServer)
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("TabletServerLockData(%q) = %v, want ErrInvalidLockData", value, err)
			}
		})
	}
}

// TestLockIDValidRequiresTheCanonicalUUID pins the same shape on the fencing
// identity. LockID.Valid is what decides whether an identity could name a real
// lock node; a UUID Accumulo cannot parse could never appear in one, so
// treating it as fencing authority would be fencing against nothing.
func TestLockIDValidRequiresTheCanonicalUUID(t *testing.T) {
	for name, value := range nonCanonicalUUIDs() {
		t.Run(name, func(t *testing.T) {
			if (LockID{UUID: value, Sequence: 1}).Valid() {
				t.Fatalf("LockID{UUID: %q} must not be usable for fencing", value)
			}
		})
	}
	if !(LockID{UUID: canonicalTestUUID, Sequence: 1}).Valid() {
		t.Fatal("the canonical form must still be usable for fencing")
	}
}

// TestTabletServerLockDataMatchesTheAccumuloWireForm pins the exact bytes a
// Shoal tablet server writes to its lock znode.
//
// This is the compatibility contract with an unmodified manager: it reads the
// znode with Gson into ServiceLockData.ServiceDescriptorsGson, whose fields
// are uuid, service, address and group in that order, and keys the result by
// service. Field names, the descriptors wrapper, the enum spellings, and the
// group being a bare string are all load-bearing — get any of them wrong and
// the manager sees a server it cannot parse.
//
// The order of the array is not part of that contract in either direction:
// Accumulo serializes through Collectors.toSet() and deserializes into a set
// keyed by service, so a Java-written node is in hash order and no reader
// depends on position. Pinning an order here is what makes the same
// advertisement the same bytes every time.
func TestTabletServerLockDataMatchesTheAccumuloWireForm(t *testing.T) {
	data, err := TabletServerLockData(serverUUID, testAddress, testGroup, TabletServerServices()...)
	if err != nil {
		t.Fatalf("TabletServerLockData: %v", err)
	}
	encoded, err := data.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := `{"descriptors":[` +
		`{"uuid":"` + serverUUID + `","service":"CLIENT","address":"` + testAddress + `","group":"default"},` +
		`{"uuid":"` + serverUUID + `","service":"TABLET_INGEST","address":"` + testAddress + `","group":"default"},` +
		`{"uuid":"` + serverUUID + `","service":"TABLET_MANAGEMENT","address":"` + testAddress + `","group":"default"},` +
		`{"uuid":"` + serverUUID + `","service":"TABLET_SCAN","address":"` + testAddress + `","group":"default"},` +
		`{"uuid":"` + serverUUID + `","service":"TSERV","address":"` + testAddress + `","group":"default"}` +
		`]}`
	if string(encoded) != want {
		t.Fatalf("lock data mismatch\n got: %s\nwant: %s", encoded, want)
	}
}

// TestEncodedLockDataIsReadableByTheLiveServerReader checks the payload
// against the shape internal/zk decodes when it enumerates live servers: a
// descriptors array whose entries carry a service name and an address. That
// reader is how a Shoal process becomes visible to Shoal's own clients, so it
// has to be able to read what this package writes.
func TestEncodedLockDataIsReadableByTheLiveServerReader(t *testing.T) {
	data, err := TabletServerLockData(serverUUID, testAddress, "ingest",
		ServiceClient, ServiceTabletScan, ServiceTabletServer)
	if err != nil {
		t.Fatalf("TabletServerLockData: %v", err)
	}
	encoded, err := data.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// The exact struct internal/zk decodes into: only service and address.
	var reader struct {
		Descriptors []struct {
			Service string `json:"service"`
			Address string `json:"address"`
		} `json:"descriptors"`
	}
	if err := json.Unmarshal(encoded, &reader); err != nil {
		t.Fatalf("decode as the live-server reader does: %v", err)
	}
	found := ""
	for _, descriptor := range reader.Descriptors {
		if descriptor.Service == "CLIENT" {
			found = descriptor.Address
		}
	}
	if found != testAddress {
		t.Fatalf("CLIENT address = %q, want %q", found, testAddress)
	}
}

func TestServiceLockDataRoundTrips(t *testing.T) {
	data, err := TabletServerLockData(serverUUID, testAddress, "", TabletServerServices()...)
	if err != nil {
		t.Fatalf("TabletServerLockData: %v", err)
	}
	encoded, err := data.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := DecodeServiceLockData(encoded)
	if err != nil {
		t.Fatalf("DecodeServiceLockData: %v", err)
	}
	if !reflect.DeepEqual(decoded.Descriptors, data.Descriptors) {
		t.Fatalf("round trip changed the descriptors:\n got %+v\nwant %+v",
			decoded.Descriptors, data.Descriptors)
	}
	for _, service := range TabletServerServices() {
		address, ok := decoded.Address(service)
		if !ok || address != testAddress {
			t.Fatalf("Address(%s) = %q, %v; want %q, true", service, address, ok, testAddress)
		}
	}
	if _, ok := decoded.Address("MANAGER"); ok {
		t.Fatal("a tablet server must not advertise the manager service")
	}
}

// TestEncodeRefusesUnusableAdvertisements checks that nothing which would
// mislead the manager can be written. A lock znode is a claim to be a live
// server; a claim the manager cannot act on is worse than staying invisible,
// because it draws work to a process that cannot do it.
func TestEncodeRefusesUnusableAdvertisements(t *testing.T) {
	good := ServiceDescriptor{
		UUID:    serverUUID,
		Service: ServiceClient,
		Address: testAddress,
		Group:   testGroup,
	}
	with := func(mutate func(*ServiceDescriptor)) ServiceLockData {
		descriptor := good
		mutate(&descriptor)
		return ServiceLockData{Descriptors: []ServiceDescriptor{descriptor}}
	}
	tests := []struct {
		name string
		data ServiceLockData
	}{
		{"no descriptors", ServiceLockData{}},
		{"not a uuid", with(func(d *ServiceDescriptor) { d.UUID = "shoal-1" })},
		{"no uuid", with(func(d *ServiceDescriptor) { d.UUID = "" })},
		{"unknown service", with(func(d *ServiceDescriptor) { d.Service = "SHOAL_SCAN" })},
		{"no service", with(func(d *ServiceDescriptor) { d.Service = "" })},
		{"no group", with(func(d *ServiceDescriptor) { d.Group = "" })},
		{"no address", with(func(d *ServiceDescriptor) { d.Address = "" })},
		{"unbound placeholder", with(func(d *ServiceDescriptor) { d.Address = placeholderAddress })},
		{"wildcard listen address", with(func(d *ServiceDescriptor) { d.Address = "0.0.0.0:9997" })},
		{"wildcard ipv6 listen address", with(func(d *ServiceDescriptor) { d.Address = "[::]:9997" })},
		{"host with a separator", with(func(d *ServiceDescriptor) { d.Address = "../..:9997" })},
		{"no port", with(func(d *ServiceDescriptor) { d.Address = "shoal-1.example" })},
		{"no host", with(func(d *ServiceDescriptor) { d.Address = ":9997" })},
		{"non-numeric port", with(func(d *ServiceDescriptor) { d.Address = "shoal-1.example:thrift" })},
		{"port zero", with(func(d *ServiceDescriptor) { d.Address = "shoal-1.example:0" })},
		{"port past the range", with(func(d *ServiceDescriptor) { d.Address = "shoal-1.example:70000" })},
		{"signed port", with(func(d *ServiceDescriptor) { d.Address = "shoal-1.example:+9997" })},
		{"host Accumulo would rewrite", with(func(d *ServiceDescriptor) { d.Address = "shoal+1.example:9997" })},
		{"duplicate service", ServiceLockData{Descriptors: []ServiceDescriptor{good, good}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.data.Validate(); !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("Validate: want ErrInvalidLockData, got %v", err)
			}
			encoded, err := tt.data.Encode()
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("Encode: want ErrInvalidLockData, got %v", err)
			}
			if encoded != nil {
				t.Fatalf("Encode returned %q alongside its refusal", encoded)
			}
		})
	}
}

// TestWildcardListenAddressIsRefused separates what a server binds from what
// it advertises. 0.0.0.0 and :: are how a process says "every interface"; they
// are not an identity, and a manager reading one out of this lock would have
// to substitute a host of its own choosing to dial anything. Real endpoints —
// a name, a loopback, an interface address — stay accepted, because refusing
// those would refuse ordinary deployments.
func TestWildcardListenAddressIsRefused(t *testing.T) {
	for _, address := range []string{
		"0.0.0.0:9997",
		"[::]:9997",
		"[0:0:0:0:0:0:0:0]:9997",
	} {
		t.Run(address, func(t *testing.T) {
			_, err := TabletServerLockData(serverUUID, address, testGroup, ServiceTabletServer)
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("TabletServerLockData(%q): want ErrInvalidLockData, got %v", address, err)
			}
		})
	}
	for _, address := range []string{
		testAddress,
		"127.0.0.1:9997",
		"[::1]:9997",
		"10.0.0.7:9997",
	} {
		t.Run(address, func(t *testing.T) {
			if _, err := TabletServerLockData(serverUUID, address, testGroup, ServiceTabletServer); err != nil {
				t.Fatalf("TabletServerLockData(%q): %v", address, err)
			}
		})
	}
}

// TestAddressAccumuloWouldRewriteIsRefused pins the one character that does
// not survive the trip. Every reader of a descriptor goes through
// AddressUtil.parseAddress, which begins by replacing every '+' with ':'
// before handing the result to HostAndPort.fromString. An address that Go
// reads as a perfectly good host:port therefore becomes a different string on
// the other side, and one with two colons in it is not a host:port at all:
// "shoal-1.example:+9997" arrives as "shoal-1.example::9997" and throws.
//
// The throw lands in the manager's live-server scan, where it ends the pass
// over every server rather than the reading of this one — the same blast
// radius as the missing TSERV descriptor. Refusing the character here means a
// server that would be unreadable never publishes.
//
// A '+' cannot appear in a hostname, an IP literal or a port, so nothing that
// belongs in an advertise address is lost by refusing it outright.
func TestAddressAccumuloWouldRewriteIsRefused(t *testing.T) {
	for _, address := range []string{
		"shoal-1.example:+9997", // becomes shoal-1.example::9997
		"shoal+1.example:9997",  // becomes shoal:1.example:9997
		"+shoal-1.example:9997", // becomes :shoal-1.example:9997
		"[::1]:+9997",           // becomes [::1]::9997
		"shoal-1.example+9997",  // the host+port form; not what we advertise
	} {
		t.Run(address, func(t *testing.T) {
			_, err := TabletServerLockData(serverUUID, address, testGroup, ServiceTabletServer)
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("TabletServerLockData(%q): want ErrInvalidLockData, got %v", address, err)
			}
			// The address is the last segment of the lock directory as well
			// as the descriptor's endpoint, and Accumulo builds a server's
			// identity from both. A character that makes one unreadable must
			// not be allowed to name the other.
			if _, err := TabletServerLockPath(testInstancePath, testGroup, address); !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("TabletServerLockPath(%q): want ErrInvalidLockData, got %v", address, err)
			}
		})
	}
}

// TestPortIsDigitsRatherThanASignedNumber closes the gap the '+' arrived
// through. strconv.Atoi reads a sign, so "+9997" and "-9997" are numbers to
// Go; a port is a bare decimal to everyone else, and Guava's HostAndPort
// refuses anything that is not all digits. Reading the port the way its
// grammar defines it — rather than the way the nearest conversion function
// happens to — is what keeps the two ends agreeing.
func TestPortIsDigitsRatherThanASignedNumber(t *testing.T) {
	for _, address := range []string{
		"shoal-1.example:+9997",
		"shoal-1.example:-9997",
		"shoal-1.example: 9997",
		"shoal-1.example:9997 ",
	} {
		t.Run(address, func(t *testing.T) {
			_, err := TabletServerLockData(serverUUID, address, testGroup, ServiceTabletServer)
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("TabletServerLockData(%q): want ErrInvalidLockData, got %v", address, err)
			}
		})
	}
	// The ordinary form still passes: this is a narrowing of the port
	// grammar, not of the addresses a deployment may use.
	if _, err := TabletServerLockData(serverUUID, "shoal-1.example:9997", testGroup, ServiceTabletServer); err != nil {
		t.Fatalf("TabletServerLockData: %v", err)
	}
}

// TestDuplicateServiceIsRefusedRatherThanCollapsed covers the one refusal that
// is about Accumulo's reader rather than about reachability: descriptors land
// in an EnumMap keyed by service, so a second address for a service is dropped
// and which one survives depends on iteration order. Advertising an
// arbitrarily chosen address is not something to do quietly.
func TestDuplicateServiceIsRefusedRatherThanCollapsed(t *testing.T) {
	data := ServiceLockData{Descriptors: []ServiceDescriptor{
		{UUID: serverUUID, Service: ServiceClient, Address: testAddress, Group: testGroup},
		{UUID: serverUUID, Service: ServiceClient, Address: "shoal-2.example:9997", Group: testGroup},
	}}
	err := data.Validate()
	if !errors.Is(err, ErrInvalidLockData) {
		t.Fatalf("Validate: want ErrInvalidLockData, got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "CLIENT") {
		t.Fatalf("error %q does not name the duplicated service", got)
	}
}

func TestTabletServerLockDataRefusesAnEmptyServiceSet(t *testing.T) {
	if _, err := TabletServerLockData(serverUUID, testAddress, testGroup); !errors.Is(err, ErrInvalidLockData) {
		t.Fatalf("want ErrInvalidLockData, got %v", err)
	}
}

// TestTabletServerLockDataDefaultsTheResourceGroup mirrors Accumulo, where a
// server with no configured group belongs to "default".
func TestTabletServerLockDataDefaultsTheResourceGroup(t *testing.T) {
	data, err := TabletServerLockData(serverUUID, testAddress, "", ServiceTabletServer)
	if err != nil {
		t.Fatalf("TabletServerLockData: %v", err)
	}
	if got := data.Descriptors[0].Group; got != DefaultResourceGroup {
		t.Fatalf("group = %q, want %q", got, DefaultResourceGroup)
	}
}

// TestDecodeIsLenientAboutServicesItDoesNotKnow keeps reading and writing
// asymmetric on purpose. This process must not publish a service it cannot
// serve, but it reads znodes written by servers it does not control —
// including Java ones and newer Accumulo versions — and a descriptor it does
// not recognize is no reason to discard the ones it does.
func TestDecodeIsLenientAboutServicesItDoesNotKnow(t *testing.T) {
	raw := []byte(`{"descriptors":[` +
		`{"uuid":"` + serverUUID + `","service":"SOME_FUTURE_SERVICE","address":"shoal-9.example:1","group":"default"},` +
		`{"uuid":"` + serverUUID + `","service":"CLIENT","address":"` + testAddress + `","group":"default"}]}`)
	decoded, err := DecodeServiceLockData(raw)
	if err != nil {
		t.Fatalf("DecodeServiceLockData: %v", err)
	}
	if len(decoded.Descriptors) != 2 {
		t.Fatalf("kept %d descriptors, want 2", len(decoded.Descriptors))
	}
	if address, ok := decoded.Address(ServiceClient); !ok || address != testAddress {
		t.Fatalf("Address(CLIENT) = %q, %v; want %q, true", address, ok, testAddress)
	}
	// Leniency stops at re-publishing: what was read cannot be written back.
	if err := decoded.Validate(); !errors.Is(err, ErrInvalidLockData) {
		t.Fatalf("Validate: want ErrInvalidLockData for an unknown service, got %v", err)
	}
}

func TestDecodeRejectsUnusablePayloads(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte("not json"), []byte(`{"descriptors":`)} {
		if _, err := DecodeServiceLockData(raw); err == nil {
			t.Fatalf("DecodeServiceLockData(%q) succeeded", raw)
		}
	}
}

// TestTabletServerServicesIsTheJavaSet pins the descriptor set a Java tablet
// server publishes, which is the set a Shoal process has to reach before it
// can stand in for one.
func TestTabletServerServicesIsTheJavaSet(t *testing.T) {
	want := []ThriftService{"CLIENT", "TABLET_INGEST", "TABLET_MANAGEMENT", "TABLET_SCAN", "TSERV"}
	got := TabletServerServices()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TabletServerServices() = %v, want %v", got, want)
	}
	got[0] = "MANAGER"
	if again := TabletServerServices(); !reflect.DeepEqual(again, want) {
		t.Fatalf("a caller mutated the package's service set: %v", again)
	}
}

// TestAdvertisingASubsetIsAllowed is the capability story: a process that has
// scans but not the write path advertises exactly that, so the manager routes
// it what it can serve rather than what it aspires to. TSERV is not part of
// the choice — see below.
func TestAdvertisingASubsetIsAllowed(t *testing.T) {
	data, err := TabletServerLockData(serverUUID, testAddress, testGroup,
		ServiceClient, ServiceTabletScan, ServiceTabletServer)
	if err != nil {
		t.Fatalf("TabletServerLockData: %v", err)
	}
	if _, err := data.Encode(); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, ok := data.Address(ServiceTabletIngest); ok {
		t.Fatal("a service that was not advertised must not resolve")
	}
}

// TestTabletServerLockDataRequiresTSERV is the one service the subset cannot
// leave out. The manager reads TSERV off every lock in the tablet-server tree
// and dereferences the address it finds there, so a payload without it cannot
// be published on the lock this constructor is named for. Acquire refuses it
// too; refusing it here reports the mistake where it was made.
func TestTabletServerLockDataRequiresTSERV(t *testing.T) {
	for _, services := range [][]ThriftService{
		{ServiceClient},
		{ServiceClient, ServiceTabletScan},
		{ServiceClient, ServiceTabletIngest, ServiceTabletManagement, ServiceTabletScan},
	} {
		_, err := TabletServerLockData(serverUUID, testAddress, testGroup, services...)
		if !errors.Is(err, ErrInvalidLockData) {
			t.Fatalf("TabletServerLockData(%v) = %v, want ErrInvalidLockData", services, err)
		}
		if !strings.Contains(err.Error(), string(ServiceTabletServer)) {
			t.Fatalf("TabletServerLockData(%v) = %v, want the missing service named", services, err)
		}
	}
	if _, err := TabletServerLockData(serverUUID, testAddress, testGroup, ServiceTabletServer); err != nil {
		t.Fatalf("TabletServerLockData with only TSERV: %v", err)
	}
}

func TestThriftServiceKnown(t *testing.T) {
	for _, service := range TabletServerServices() {
		if !service.Known() {
			t.Fatalf("%s must be a known service", service)
		}
	}
	for _, service := range []ThriftService{"MANAGER", "COORDINATOR", "COMPACTOR", "GC", "NONE"} {
		if !service.Known() {
			t.Fatalf("%s is part of Accumulo's enum and must parse", service)
		}
	}
	for _, service := range []ThriftService{"", "SHOAL", "tserv", "TSERV "} {
		if service.Known() {
			t.Fatalf("%q must not be treated as an Accumulo service", service)
		}
	}
}
