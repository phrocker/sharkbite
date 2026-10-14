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
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ErrInvalidLockData means the ServiceLockData could not describe a server the
// manager can talk to, so it is refused rather than published. A lock znode
// carrying an address the manager cannot dial is worse than no lock at all:
// the manager would see a live tablet server and route work into a black hole.
var ErrInvalidLockData = errors.New("tserver: invalid service lock data")

// DefaultResourceGroup is the resource group a server belongs to when none is
// configured, matching Accumulo's Constants.DEFAULT_RESOURCE_GROUP_NAME.
const DefaultResourceGroup = "default"

// placeholderAddress is the address Accumulo publishes before a server has
// bound its port. It names no endpoint, so it can never be advertised as one.
const placeholderAddress = "0.0.0.0:0"

// canonicalUUIDLength is the length of the 36-character dashed UUID form.
const canonicalUUIDLength = 36

// validAccumuloUUID reports whether value is a UUID Accumulo can read back.
//
// Go's uuid.Parse is not a shape check: it also accepts a 32-character bare
// hex form, a "urn:uuid:" form, and a braced form. Java's UUID.fromString
// accepts none of those, so they are exactly the forms a Go writer could
// publish and a Java reader could not parse — in a lock node name that makes
// this process invisible to Accumulo's ServiceLock.validateAndSort, and in a
// lock payload it makes the whole znode unreadable to the manager. Only the
// length has to be tested, because 36 is the one length at which uuid.Parse
// requires the dashed form.
func validAccumuloUUID(value string) bool {
	if len(value) != canonicalUUIDLength {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

// resourceGroupPattern is Accumulo's ResourceGroupId.GROUP_NAME_PATTERN.
//
// ResourceGroupId.of applies it to every name it is handed and throws when it
// does not match, and ServiceLockData.parse runs every descriptor's group
// through ResourceGroupId.of. A group outside this grammar therefore does not
// merely read back oddly: it throws on the manager's side and makes the whole
// lock znode unreadable, which is the same failure a non-canonical UUID
// causes and is refused here for the same reason.
//
// It also keeps a group out of a lock path it has no business in. Path
// segments are cleaned when the lock directory is built, so a name like
// "../managers" would not land under the tablet-server subtree at all.
//
// Java writes it as "^[a-zA-Z]+(_?[a-zA-Z0-9])*$" and applies it with
// Pattern.matches, which anchors to the whole input. It is spelled with \A and
// \z here so the Go form is anchored the same way beyond argument: ^ and $ are
// line anchors under the m flag, and spelling out the text anchors keeps the
// grammar from depending on which flags a later edit adds.
var resourceGroupPattern = regexp.MustCompile(`\A[a-zA-Z]+(_?[a-zA-Z0-9])*\z`)

// validResourceGroup reports whether name is a resource group Accumulo reads
// back without throwing.
func validResourceGroup(name string) bool {
	return resourceGroupPattern.MatchString(name)
}

// ThriftService names one service advertised on a ServiceLock znode. The
// values mirror ServiceLockData.ThriftService, which is what the manager and
// Accumulo clients match against when they read the lock.
type ThriftService string

// The subset of ThriftService a tablet server can publish. The full enum
// covers manager-side and compaction services too; those are advertised on
// other locks and are not a tablet server's to claim.
const (
	// ServiceClient is the ClientService endpoint scan clients dial. It is
	// the descriptor internal/zk reads when it enumerates live servers.
	ServiceClient ThriftService = "CLIENT"
	// ServiceTabletIngest is the write path: mutations, conditional writes,
	// and the WAL behind them.
	ServiceTabletIngest ThriftService = "TABLET_INGEST"
	// ServiceTabletManagement is the manager-facing lifecycle surface —
	// assignment, unassignment, split, and status.
	ServiceTabletManagement ThriftService = "TABLET_MANAGEMENT"
	// ServiceTabletScan is the scan path against hosted tablets.
	ServiceTabletScan ThriftService = "TABLET_SCAN"
	// ServiceTabletServer is the legacy combined tablet-server endpoint.
	ServiceTabletServer ThriftService = "TSERV"
	// ServiceCompactor is the coordinator-facing external compactor endpoint.
	ServiceCompactor ThriftService = "COMPACTOR"
)

// thriftServiceOrder is the declaration order of ServiceLockData.ThriftService.
// Encoding in it makes a Shoal-written znode the same bytes every time, which
// is what lets the wire form be pinned by a test and diffed by an operator.
//
// It is not what Java emits. ServiceLockData serializes through
// Collectors.toSet(), so the array a Java tablet server writes is in hash
// order. Nothing reads the array positionally — Accumulo deserializes it into
// a set and keys it by service — so the order is this process's to choose, and
// a stable order is worth more than an imitation of an unstable one.
var thriftServiceOrder = map[ThriftService]int{
	"CLIENT":            0,
	"COORDINATOR":       1,
	"COMPACTOR":         2,
	"FATE_CLIENT":       3,
	"FATE_WORKER":       4,
	"GC":                5,
	"MANAGER":           6,
	"NONE":              7,
	"TABLET_INGEST":     8,
	"TABLET_MANAGEMENT": 9,
	"TABLET_SCAN":       10,
	"TSERV":             11,
}

// tabletServerServices is the descriptor set an Accumulo tablet server
// publishes (TabletServer.announceExistence). It is the set a Shoal process
// must reach before it can stand in for a Java tablet server.
var tabletServerServices = []ThriftService{
	ServiceClient,
	ServiceTabletIngest,
	ServiceTabletManagement,
	ServiceTabletScan,
	ServiceTabletServer,
}

// tabletServerServiceSet is tabletServerServices as a set, for refusing a
// service that belongs to another role.
var tabletServerServiceSet = func() map[ThriftService]struct{} {
	set := make(map[ThriftService]struct{}, len(tabletServerServices))
	for _, service := range tabletServerServices {
		set[service] = struct{}{}
	}
	return set
}()

// Known reports whether the service is one Accumulo defines. An unknown name
// is refused rather than published: Accumulo's reader parses the field as an
// enum, so a name it does not know makes the whole lock znode unreadable and
// the server invisible.
func (s ThriftService) Known() bool {
	_, ok := thriftServiceOrder[s]
	return ok
}

// TabletServerServices returns the descriptor set a Java tablet server
// publishes, in Accumulo's enum order.
//
// A Shoal process should advertise only the services it actually implements.
// Publishing the whole set before the matching endpoints exist does not make
// the process a tablet server; it makes the manager route assignments, scans,
// and writes to a server that cannot answer them.
func TabletServerServices() []ThriftService {
	return append([]ThriftService(nil), tabletServerServices...)
}

// ServiceDescriptor is one advertised endpoint on a ServiceLock znode: which
// process it belongs to, which service it speaks, where to reach it, and which
// resource group it serves.
//
// The JSON field names are the wire contract with Accumulo's Gson
// serialization of ServiceLockData.ServiceDescriptorGson.
type ServiceDescriptor struct {
	UUID    string        `json:"uuid"`
	Service ThriftService `json:"service"`
	Address string        `json:"address"`
	Group   string        `json:"group"`
}

// Validate reports whether the descriptor names a reachable service.
func (d ServiceDescriptor) Validate() error {
	if !validAccumuloUUID(d.UUID) {
		return fmt.Errorf("%w: server uuid %q is not the 36-character dashed form Accumulo reads",
			ErrInvalidLockData, d.UUID)
	}
	if !d.Service.Known() {
		return fmt.Errorf("%w: unknown service %q", ErrInvalidLockData, d.Service)
	}
	if d.Group == "" {
		return fmt.Errorf("%w: %s descriptor has no resource group", ErrInvalidLockData, d.Service)
	}
	if !validResourceGroup(d.Group) {
		return fmt.Errorf("%w: %s descriptor resource group %q is not a name Accumulo reads (must match %s)",
			ErrInvalidLockData, d.Service, d.Group, resourceGroupPattern)
	}
	if err := validateAdvertiseAddress(d.Address); err != nil {
		return fmt.Errorf("%w: %s descriptor: %w", ErrInvalidLockData, d.Service, err)
	}
	return nil
}

// validateAdvertiseAddress rejects anything that is not a host:port another
// process could dial, and anything that could not also name the directory the
// lock is registered in. The advertised address is both: the manager dials it,
// and it is the last segment of <instance>/tservers/<group>/<address>.
func validateAdvertiseAddress(address string) error {
	if address == "" {
		return errors.New("empty address")
	}
	if address == placeholderAddress {
		return fmt.Errorf("address %q is the unbound placeholder", address)
	}
	if strings.ContainsRune(address, '+') {
		// Every Accumulo reader of this address goes through
		// AddressUtil.parseAddress, which replaces each '+' with ':' before
		// parsing — the encoding older Accumulo used to spell host+port in a
		// path segment. A '+' anywhere therefore names one endpoint here and
		// a different string there, and usually not a host:port at all:
		// "shoal-1.example:+9997" is read as "shoal-1.example::9997", which
		// throws out of the manager's scan over every live server rather than
		// out of the reading of this one. Nothing that belongs in a hostname,
		// an IP literal or a port contains '+', so refusing it costs nothing.
		return fmt.Errorf("address %q contains '+', which Accumulo rewrites to ':' before parsing", address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("address %q is not host:port: %w", address, err)
	}
	if host == "" {
		return fmt.Errorf("address %q has no host", address)
	}
	if strings.ContainsRune(host, '/') || host == "." || host == ".." {
		// Segments are cleaned when they are joined, so a host carrying a
		// separator or a dot segment would move the lock directory out of the
		// tablet-server subtree entirely — the same escape the resource group
		// is checked for. ZooKeeper refuses "." and ".." as path components in
		// any case, and neither is a host anything could dial.
		return fmt.Errorf("address %q host %q is not a single path segment", address, host)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		// A wildcard is what a server binds, not what it is reachable at.
		// Published to the manager it identifies no host: every reader would
		// have to substitute one, and work routed from that guess lands on
		// whichever server the reader guessed rather than this one.
		return fmt.Errorf("address %q is a wildcard listen address, not a reachable one", address)
	}
	// A port is a bare decimal, and that is what reads it on the other side:
	// Guava's HostAndPort refuses a port string that is not all digits.
	// strconv.Atoi is more permissive than the grammar — it accepts a sign —
	// so the digits are checked before the value is.
	for _, r := range port {
		if r < '0' || r > '9' {
			return fmt.Errorf("address %q port %q is not a decimal number", address, port)
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("address %q has an unreadable port: %w", address, err)
	}
	if number < 1 || number > 65535 {
		return fmt.Errorf("address %q port %d is out of range", address, number)
	}
	return nil
}

// ServiceLockData is the payload written to a ServiceLock znode: the set of
// services the lock holder advertises.
type ServiceLockData struct {
	Descriptors []ServiceDescriptor
}

// serviceLockDataJSON is the on-the-wire shape, matching Accumulo's
// ServiceLockData.ServiceDescriptorsGson.
type serviceLockDataJSON struct {
	Descriptors []ServiceDescriptor `json:"descriptors"`
}

// TabletServerLockData builds the payload a Shoal tablet server publishes for
// the services it implements, all pointing at one advertise address in one
// resource group under one server UUID — the shape
// TabletServer.announceExistence writes.
//
// An empty group means DefaultResourceGroup. Passing no services is refused:
// a lock that advertises nothing tells the manager a server is alive without
// telling it how to reach anything.
//
// Only the five tablet-server services are accepted. The rest of the enum
// belongs to the manager, the coordinator, the garbage collector and the
// compactors, which advertise on their own locks; a tablet server publishing
// one of them would be claiming an endpoint another role owns and pointing it
// at a process that does not implement it.
//
// TSERV has to be one of them. The manager reads that descriptor off every
// lock in the tablet-server tree and dereferences the address it finds, so a
// payload without it cannot be published on the lock this constructor names —
// ServiceLock.Acquire refuses it, and refusing it here means the caller learns
// so where the mistake is rather than at acquisition time.
func TabletServerLockData(serverUUID, address, group string, services ...ThriftService) (ServiceLockData, error) {
	if group == "" {
		group = DefaultResourceGroup
	}
	if len(services) == 0 {
		return ServiceLockData{}, fmt.Errorf("%w: no services to advertise", ErrInvalidLockData)
	}
	advertisesTabletServer := false
	data := ServiceLockData{Descriptors: make([]ServiceDescriptor, 0, len(services))}
	for _, service := range services {
		if _, ours := tabletServerServiceSet[service]; !ours {
			return ServiceLockData{}, fmt.Errorf("%w: %q is not a tablet-server service",
				ErrInvalidLockData, service)
		}
		if service == ServiceTabletServer {
			advertisesTabletServer = true
		}
		data.Descriptors = append(data.Descriptors, ServiceDescriptor{
			UUID:    serverUUID,
			Service: service,
			Address: address,
			Group:   group,
		})
	}
	if !advertisesTabletServer {
		return ServiceLockData{}, fmt.Errorf("%w: a tablet server must advertise %s, which the manager reads off every lock in the tree",
			ErrInvalidLockData, ServiceTabletServer)
	}
	if err := data.Validate(); err != nil {
		return ServiceLockData{}, err
	}
	return data, nil
}

// CompactorLockData builds the payload an external compactor publishes under
// <instance>/compactors/<group>/<address>.
func CompactorLockData(serverUUID, address, group string) (ServiceLockData, error) {
	if group == "" {
		group = DefaultResourceGroup
	}
	data := ServiceLockData{Descriptors: []ServiceDescriptor{
		{
			UUID:    serverUUID,
			Service: ServiceCompactor,
			Address: address,
			Group:   group,
		},
	}}
	if err := data.Validate(); err != nil {
		return ServiceLockData{}, err
	}
	return data, nil
}

// Validate reports whether the payload is one Accumulo can read and act on.
//
// A service may appear only once. Accumulo keys descriptors by service in an
// EnumMap, so a duplicate would be silently dropped and the surviving one
// chosen by iteration order — publishing two addresses for a service means
// publishing an arbitrary one of them.
func (d ServiceLockData) Validate() error {
	if len(d.Descriptors) == 0 {
		return fmt.Errorf("%w: no descriptors", ErrInvalidLockData)
	}
	seen := make(map[ThriftService]struct{}, len(d.Descriptors))
	for _, descriptor := range d.Descriptors {
		if err := descriptor.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[descriptor.Service]; duplicate {
			return fmt.Errorf("%w: %s advertised more than once",
				ErrInvalidLockData, descriptor.Service)
		}
		seen[descriptor.Service] = struct{}{}
	}
	return nil
}

// Address returns the address advertised for a service, and whether it is
// advertised at all. It mirrors ServiceLockData.getAddressString.
func (d ServiceLockData) Address(service ThriftService) (string, bool) {
	for _, descriptor := range d.Descriptors {
		if descriptor.Service == service {
			return descriptor.Address, true
		}
	}
	return "", false
}

// Encode serializes the payload for a lock znode. It validates first, so an
// unusable advertisement is never written to ZooKeeper.
func (d ServiceLockData) Encode() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	descriptors := append([]ServiceDescriptor(nil), d.Descriptors...)
	sort.Slice(descriptors, func(i, j int) bool {
		return thriftServiceOrder[descriptors[i].Service] < thriftServiceOrder[descriptors[j].Service]
	})
	encoded, err := json.Marshal(serviceLockDataJSON{Descriptors: descriptors})
	if err != nil {
		return nil, fmt.Errorf("encode service lock data: %w", err)
	}
	return encoded, nil
}

// DecodeServiceLockData parses a lock znode payload.
//
// It does not validate: reading is how this process observes servers it does
// not control, including Java ones and future Accumulo versions, and a
// descriptor it cannot use is not a reason to discard the ones it can. Callers
// that intend to act on the result check the fields they need.
func DecodeServiceLockData(raw []byte) (ServiceLockData, error) {
	if len(raw) == 0 {
		return ServiceLockData{}, fmt.Errorf("%w: empty payload", ErrInvalidLockData)
	}
	var decoded serviceLockDataJSON
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ServiceLockData{}, fmt.Errorf("decode service lock data: %w", err)
	}
	return ServiceLockData{Descriptors: decoded.Descriptors}, nil
}
