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

// Package itercfg resolves the iterator stack that Java applied at a
// given compaction scope, by reading the table's configuration out of
// ZooKeeper. Layout (Accumulo 4.0):
//
//	/accumulo/<instance-id>/namespaces                          -> JSON {namespace-id: namespace-name}
//	/accumulo/<instance-id>/namespaces/<ns-id>/tables           -> JSON {table-id: table-name}
//	/accumulo/<instance-id>/tables/<id>/config                  -> versioned-props blob (binary)
//	/accumulo/<instance-id>/namespaces/<ns-id>/config           -> versioned-props blob (binary)
//	/accumulo/<instance-id>/config                              -> versioned-props blob (binary)  [site]
//
// Name resolution requires (1) parsing `name` into namespace + table
// halves (TableNameUtil.qualify: "ns.table" or just "table" for default
// namespace), (2) looking up the namespace id from /namespaces, then
// (3) looking up the table id from /namespaces/<ns-id>/tables. The
// JSON-map shape is the same as Java's NamespaceMapping.serializeMap.
//
// VERSIONED-PROPS BLOB FORMAT (Java VersionedPropGzipCodec):
//
//	int32  encoding version (currently 1)
//	bool   compressed flag (1 byte: 0x00 or 0x01)
//	UTF    timestamp string (DataOutputStream.writeUTF: int16 length || UTF-8 bytes)
//	[ gzip(  // payload, optionally gzip-compressed when the bool is true
//	    int32  number of (key, value) pairs
//	    repeated:
//	      UTF  key
//	      UTF  value
//	  ) ]
//
// Properties are MERGED across system → namespace → table levels, with
// table overriding namespace overriding system. Accumulo's actual
// compactor reads all three; an iterator can be defined at any level.
// Most installs configure table.iterator.* at the SYSTEM level so it
// applies to every table by default — the table-level znode is empty
// even when `accumulo shell config -t graph_vidx` shows the properties
// (the shell reports the merged effective view).
//
// We care about properties of the form:
//
//	table.iterator.<scope>.<name>            = "<priority>,<class>"
//	table.iterator.<scope>.<name>.opt.<k>    = "<v>"
//
// where <scope> is scan|minc|majc and <name> is the operator's nickname
// for the stack entry. The resolver groups options under their owning
// iterator, sorts by priority ascending (matching Accumulo's
// IteratorEnvironment build order), maps Java class names to shoal's
// iterrt registry, and returns the resulting []iterrt.IterSpec.
//
// Every configured iterator is checked against the shared, versioned
// capability registry. Unsupported classes, contexts, options, and malformed
// headers fail closed before the stack can execute.
package itercfg

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	gozk "github.com/go-zookeeper/zk"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	nslookup "github.com/phrocker/shoal-oss/internal/namespaces"
	"github.com/phrocker/shoal-oss/internal/tablenames"
)

// ClassAllowlist is retained for API compatibility. It is generated from the
// shared registry so compactor admission and table configuration cannot drift.
var ClassAllowlist = classAllowlist()

// accumulo40TableDefaults pins the table properties consumed by Shoal to the
// defaults in Apache Accumulo revision 1a716b2c. ZooKeeper stores overrides,
// not a complete effective configuration.
var accumulo40TableDefaults = map[string]string{
	"table.file.type":               "rf",
	"table.file.compress.type":      "gz",
	"table.file.compress.blocksize": "100k",
	"table.bloom.enabled":           "false",
	"table.groups.enabled":          "",
	"table.sampler":                 "",
}

func classAllowlist() map[string]string {
	out := map[string]string{}
	for _, capability := range iterrt.RegistrySnapshot().Iterators {
		for _, class := range capability.JavaClasses {
			out[class] = capability.Name
		}
	}
	return out
}

// ResolvedStack is the parsed table.iterator.<scope>.* config for one
// table. Stack is the in-order list (lowest priority first) of iterators
// that ARE supported; Skipped lists the entries that were dropped
// because their class isn't allowlisted.
type ResolvedStack struct {
	// TableID is the table the stack belongs to.
	TableID string `json:"tableId"`
	// Scope is the compaction scope these specs apply to.
	Scope iterrt.IteratorScope `json:"scope"`
	// RegistryVersion is the capability registry used for admission.
	RegistryVersion int `json:"registryVersion"`
	// Report is the machine-readable compatibility inventory for this stack.
	Report iterrt.CompatibilityReport `json:"report"`
	// Stack is the resolved iterator chain in priority order (low first).
	// Empty stack is valid (no iterators configured at this scope).
	Stack []iterrt.IterSpec `json:"stack"`
	// Skipped names each iterator that was dropped because its class
	// isn't in ClassAllowlist. Operators read this list to know which
	// tables need a new iterator port before shadow comparison is
	// meaningful.
	Skipped []SkippedIter `json:"rejected,omitempty"`
	// LoadedAt records when the config snapshot was taken from ZK.
	// Callers use this to gate cache freshness.
	LoadedAt time.Time `json:"loadedAt"`
}

// SkippedIter describes a table.iterator.<scope>.<name> entry that was
// dropped from the resolved stack because its Java class isn't in
// ClassAllowlist.
type SkippedIter struct {
	// Name is the entry's nickname (the third dotted component of the
	// table.iterator property).
	Name string `json:"name"`
	// Class is the fully-qualified Java class name from the property
	// value.
	Class string `json:"javaClass,omitempty"`
	// Priority is the integer priority parsed from the property value
	// (or -1 if the value was malformed).
	Priority int `json:"priority"`
	// Reason explains why admission rejected the iterator.
	Reason string `json:"reason"`
}

// MalformedIteratorConfigError reports an invalid iterator header property.
type MalformedIteratorConfigError struct {
	Prop  string
	Value string
	Err   error
}

func (e *MalformedIteratorConfigError) Error() string {
	return fmt.Sprintf("itercfg: malformed iterator header %q=%q: %v", e.Prop, e.Value, e.Err)
}

func (e *MalformedIteratorConfigError) Unwrap() error { return e.Err }

// IncompleteIteratorConfigError reports options without a valid header.
type IncompleteIteratorConfigError struct {
	Name string
}

func (e *IncompleteIteratorConfigError) Error() string {
	return fmt.Sprintf("itercfg: iterator %q is missing a valid header property", e.Name)
}

// StackConfigError aggregates all blockers in one table/scope stack.
type StackConfigError struct {
	TableID string
	Scope   iterrt.IteratorScope
	Issues  []error
}

func (e *StackConfigError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.Error()
	}
	return fmt.Sprintf("itercfg: table %s %s stack rejected: %s",
		e.TableID, scopeString(e.Scope), strings.Join(parts, "; "))
}

func (e *StackConfigError) Unwrap() []error { return e.Issues }

// HasShoalCoverage is true when at least one iterator was successfully
// resolved AND no iterators were skipped. Operators use this to decide
// whether a table is shadow-eligible end-to-end.
func (r *ResolvedStack) HasShoalCoverage() bool {
	return len(r.Skipped) == 0
}

// Resolver loads + caches per-table iterator stacks from ZK. Safe for
// concurrent use; refreshes are coalesced per table-id.
//
// The cache TTL controls how quickly an operator-side table.iterator.*
// config change is picked up by the poller. Set 0 to disable caching
// (every Resolve hits ZK).
type Resolver struct {
	locator tablenames.Locator
	ttl     time.Duration
	logger  *slog.Logger

	namespaceNames *nslookup.Resolver
	names          *tablenames.Resolver

	cacheMu sync.Mutex
	cache   map[stackKey]*ResolvedStack
}

type stackKey struct {
	tableID string
	scope   iterrt.IteratorScope
}

// NewResolver builds a Resolver bound to locator. ttl=0 disables the
// per-stack cache. logger=nil uses slog.Default().
func NewResolver(locator tablenames.Locator, ttl time.Duration, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	namespaceNames := nslookup.NewResolver(locator)
	return &Resolver{
		locator:        locator,
		ttl:            ttl,
		logger:         logger,
		namespaceNames: namespaceNames,
		names:          tablenames.NewResolver(locator, namespaceNames),
		cache:          map[stackKey]*ResolvedStack{},
	}
}

// ResolveTableID returns the table-id whose entry in the appropriate
// namespace JSON map equals tableName. Accepts both qualified
// ("ns.table") and unqualified ("table") forms; unqualified resolves
// against the default namespace.
//
// Caches the per-namespace JSON map for the resolver's lifetime — new
// tables added after construction won't be visible until
// InvalidateNames clears the cache.
func (r *Resolver) ResolveTableID(ctx context.Context, tableName string) (string, error) {
	return r.names.ResolveID(ctx, tableName)
}

// InvalidateNames clears the name→id cache so the next ResolveTableID
// re-scans ZK. Used by the poller after detecting a CreateTable / rename.
func (r *Resolver) InvalidateNames() {
	r.namespaceNames.Invalidate()
	r.names.Invalidate()
}

// EffectiveProperties returns the merged system, namespace, and table
// configuration for tableID. Missing namespace- or table-level configuration
// nodes are valid, while transport and decoding failures fail the read.
func (r *Resolver) EffectiveProperties(ctx context.Context, tableID string) (map[string]string, error) {
	if r == nil || r.locator == nil || tableID == "" {
		return nil, errors.New("itercfg: invalid effective-configuration dependency")
	}
	merged := make(map[string]string, len(accumulo40TableDefaults))
	for key, value := range accumulo40TableDefaults {
		merged[key] = value
	}
	systemPath := path.Join(r.locator.InstancePath(), "config")
	if err := r.mergePropsFrom(ctx, systemPath, "", merged); err != nil {
		return nil, fmt.Errorf("itercfg: read system configuration: %w", err)
	}
	tableNS, err := r.tableNamespaceID(ctx, tableID)
	if err != nil {
		return nil, fmt.Errorf("itercfg: read namespace for table %s: %w", tableID, err)
	}
	if tableNS != "" {
		nsPath := path.Join(r.locator.InstancePath(), "namespaces", tableNS, "config")
		if err := r.mergeOptionalPropsFrom(ctx, nsPath, "", merged); err != nil {
			return nil, fmt.Errorf("itercfg: read namespace %s configuration: %w", tableNS, err)
		}
	}
	tablePath := path.Join(r.locator.InstancePath(), "tables", tableID, "config")
	if err := r.mergeOptionalPropsFrom(ctx, tablePath, "", merged); err != nil {
		return nil, fmt.Errorf("itercfg: read table %s configuration: %w", tableID, err)
	}
	return merged, nil
}

// Resolve loads the iterator stack for tableID at scope, parses
// table.iterator.<scope>.* properties, and returns the resolved chain.
// Cache hits return the cached result iff age < ttl.
func (r *Resolver) Resolve(ctx context.Context, tableID string, scope iterrt.IteratorScope) (*ResolvedStack, error) {
	key := stackKey{tableID: tableID, scope: scope}

	r.cacheMu.Lock()
	if cached, ok := r.cache[key]; ok && r.ttl > 0 && time.Since(cached.LoadedAt) < r.ttl {
		r.cacheMu.Unlock()
		return cached, nil
	}
	r.cacheMu.Unlock()

	stack, err := r.loadStack(ctx, tableID, scope)
	if err != nil {
		return stack, err
	}

	r.cacheMu.Lock()
	r.cache[key] = stack
	r.cacheMu.Unlock()
	return stack, nil
}

// loadStack performs the ZK fetch + parse. Properties are merged in
// inheritance order: system → namespace → table. Each level's znode is
// a versioned-props blob (see decodePropBlob).
func (r *Resolver) loadStack(ctx context.Context, tableID string, scope iterrt.IteratorScope) (*ResolvedStack, error) {
	prefix := "table.iterator." + scopeString(scope) + "."

	merged := map[string]string{}

	// System level: /accumulo/<id>/config
	if err := r.mergePropsFrom(ctx, path.Join(r.locator.InstancePath(), "config"), prefix, merged); err != nil {
		r.logger.Warn("itercfg: system config read failed (continuing without site overrides)",
			slog.String("err", err.Error()))
	}

	// Namespace level. Look up the table's namespace, then read that
	// namespace's config znode.
	tableNS, err := r.tableNamespaceID(ctx, tableID)
	if err == nil && tableNS != "" {
		nsPath := path.Join(r.locator.InstancePath(), "namespaces", tableNS, "config")
		if err := r.mergePropsFrom(ctx, nsPath, prefix, merged); err != nil {
			r.logger.Debug("itercfg: namespace config read failed",
				slog.String("ns", tableNS), slog.String("err", err.Error()))
		}
	}

	// Table level: /accumulo/<id>/tables/<id>/config
	tablePath := path.Join(r.locator.InstancePath(), "tables", tableID, "config")
	if err := r.mergePropsFrom(ctx, tablePath, prefix, merged); err != nil {
		r.logger.Debug("itercfg: table config read failed",
			slog.String("table", tableID), slog.String("err", err.Error()))
	}

	return parseStack(tableID, scope, prefix, merged)
}

// mergePropsFrom reads the versioned-props blob at znodePath, decodes
// it, and merges keys whose name starts with prefix into out (later
// calls override earlier). A non-existent znode is not an error — some
// installs don't have namespace-level config znodes at all.
func (r *Resolver) mergePropsFrom(ctx context.Context, znodePath, prefix string, out map[string]string) error {
	data, err := r.locator.GetRaw(ctx, znodePath)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	props, err := decodePropBlob(data)
	if err != nil {
		return fmt.Errorf("decode %s: %w", znodePath, err)
	}
	for k, v := range props {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return nil
}

func (r *Resolver) mergeOptionalPropsFrom(
	ctx context.Context,
	znodePath, prefix string,
	out map[string]string,
) error {
	err := r.mergePropsFrom(ctx, znodePath, prefix, out)
	if errors.Is(err, gozk.ErrNoNode) {
		return nil
	}
	return err
}

// tableNamespaceID returns the namespace-id znode-value stored at
// /accumulo/<id>/tables/<table-id>/namespace, or "" if not found.
func (r *Resolver) tableNamespaceID(ctx context.Context, tableID string) (string, error) {
	p := path.Join(r.locator.InstancePath(), "tables", tableID, "namespace")
	data, err := r.locator.GetRaw(ctx, p)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// decodePropBlob parses an Accumulo VersionedPropGzipCodec encoded
// property blob and returns the property map. Wire layout:
//
//	int32   encoding version (must be 1)
//	bool    compressed flag (1 byte)
//	UTF     timestamp string (DataOutputStream.writeUTF: int16 length + UTF-8 bytes)
//	payload either gzip-stream or raw:
//	  int32 count
//	  repeated: UTF key, UTF value
//
// Mirrors Java's VersionedPropCodec.fromBytes + VersionedPropGzipCodec.decodePayload.
func decodePropBlob(data []byte) (map[string]string, error) {
	r := bytes.NewReader(data)

	// EncodingOptions.fromDataStream: readInt() then readBoolean().
	var version uint32
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if version != 1 {
		return nil, fmt.Errorf("unsupported props encoding version %d", version)
	}
	var compressed byte
	if err := binary.Read(r, binary.BigEndian, &compressed); err != nil {
		return nil, fmt.Errorf("read compressed flag: %w", err)
	}

	// Java DataOutputStream.writeUTF: int16 (unsigned) length + UTF-8 bytes.
	if _, err := readJavaUTF(r); err != nil {
		return nil, fmt.Errorf("read timestamp: %w", err)
	}

	var payload io.Reader = r
	if compressed == 1 {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		payload = gz
	}

	// Property map: int32 count + count × (UTF key, UTF value).
	var count uint32
	if err := binary.Read(payload, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read prop count: %w", err)
	}
	out := make(map[string]string, count)
	for i := uint32(0); i < count; i++ {
		k, err := readJavaUTF(payload)
		if err != nil {
			return nil, fmt.Errorf("read key %d: %w", i, err)
		}
		v, err := readJavaUTF(payload)
		if err != nil {
			return nil, fmt.Errorf("read value %d (key=%q): %w", i, k, err)
		}
		out[k] = v
	}
	return out, nil
}

// readJavaUTF reads a string in Java's DataOutputStream.writeUTF format:
// uint16-be length followed by that many bytes interpreted as "modified
// UTF-8". For property strings (ASCII / regular UTF-8) the modified
// encoding is identical to plain UTF-8, so we just slice the bytes.
func readJavaUTF(r io.Reader) (string, error) {
	var length uint16
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return "", err
	}
	if length == 0 {
		return "", nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// parseStack converts the prop map into an ordered ResolvedStack. Pure
// over (tableID, scope, prefix, props); covered by unit tests that
// bypass ZK entirely.
func parseStack(tableID string, scope iterrt.IteratorScope, prefix string, props map[string]string) (*ResolvedStack, error) {
	out := &ResolvedStack{
		TableID:         tableID,
		Scope:           scope,
		RegistryVersion: iterrt.CapabilityRegistryVersion,
		LoadedAt:        time.Now(),
	}

	var issues []error

	type rawEntry struct {
		name        string
		priority    int
		class       string
		opts        map[string]string
		prop        string
		headerValue string
		headerOK    bool
		headerErr   error
	}
	entries := map[string]*rawEntry{} // by iterator nickname

	for k, v := range props {
		rest := strings.TrimPrefix(k, prefix)
		if rest == "" {
			continue
		}
		name, tail, hasTail := strings.Cut(rest, ".")
		optKey, isOption := strings.CutPrefix(tail, "opt.")
		if hasTail && !isOption {
			continue
		}
		entry, ok := entries[name]
		if !ok {
			entry = &rawEntry{name: name, priority: -1, opts: map[string]string{}}
			entries[name] = entry
		}
		if !hasTail {
			// Header property: "<priority>,<class>".
			pri, class, perr := splitPriorityClass(v)
			if perr != nil {
				entry.prop = k
				entry.headerValue = v
				entry.headerErr = perr
				continue
			}
			entry.priority = pri
			entry.class = class
			entry.prop = k
			entry.headerValue = v
			entry.headerOK = true
			continue
		}
		// Option property: "opt.<key>".
		if isOption {
			entry.opts[optKey] = v
		}
	}

	// Order by priority ascending (lowest priority runs LOWEST in the
	// stack — Accumulo IteratorUtil.loadIterators convention). Java
	// docs say "lowest priority first" matches "leaf side of the stack."
	keys := make([]string, 0, len(entries))
	for n := range entries {
		keys = append(keys, n)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		left, right := entries[keys[i]], entries[keys[j]]
		if left.priority == right.priority {
			return compareUTF16(left.name, right.name) < 0
		}
		return left.priority < right.priority
	})

	configured := make([]iterrt.ConfiguredIterator, 0, len(keys))
	for _, name := range keys {
		e := entries[name]
		if e.headerErr != nil {
			err := &MalformedIteratorConfigError{Prop: e.prop, Value: e.headerValue, Err: e.headerErr}
			issues = append(issues, err)
			out.Skipped = append(out.Skipped, SkippedIter{
				Name: e.name, Priority: e.priority, Reason: err.Error(),
			})
			continue
		}
		if !e.headerOK {
			err := &IncompleteIteratorConfigError{Name: e.name}
			issues = append(issues, err)
			out.Skipped = append(out.Skipped, SkippedIter{
				Name: e.name, Priority: e.priority, Reason: err.Error(),
			})
			continue
		}
		configured = append(configured, iterrt.ConfiguredIterator{
			Name:      e.name,
			JavaClass: e.class,
			Priority:  e.priority,
			Options:   e.opts,
		})
	}

	report, reportErr := iterrt.CheckCompatibility(iterrt.CompatibilityRequest{
		RegistryVersion: iterrt.CapabilityRegistryVersion,
		AccumuloVersion: iterrt.AccumuloCompatibilityVersion,
		Context:         iterrt.ContextFromScope(scope),
		Iterators:       configured,
	})
	out.Report = report
	for _, item := range report.Iterators {
		if item.Supported {
			out.Stack = append(out.Stack, iterrt.IterSpec{
				Name:    item.NativeName,
				Options: item.Options,
			})
			continue
		}
		reason := "unsupported iterator capability"
		for _, issue := range report.Issues {
			if issue.Name == item.Name && issue.JavaClass == item.JavaClass {
				reason = issue.Message
				break
			}
		}
		out.Skipped = append(out.Skipped, SkippedIter{
			Name: item.Name, Class: item.JavaClass, Priority: item.Priority, Reason: reason,
		})
	}
	if reportErr != nil {
		issues = append(issues, reportErr)
	}
	if len(issues) > 0 {
		out.Report.Supported = false
		for _, issue := range issues {
			if issue == reportErr {
				continue
			}
			out.Report.Issues = append(out.Report.Issues, iterrt.CompatibilityIssue{
				Code:    "malformed_iterator_config",
				Message: issue.Error(),
				Context: iterrt.ContextFromScope(scope),
			})
		}
		return out, &StackConfigError{TableID: tableID, Scope: scope, Issues: issues}
	}
	return out, nil
}

// ResolveProperties validates an already-merged effective table configuration.
// It is the process-wiring counterpart to Resolver.Resolve: callers that read
// configuration through Accumulo's ClientService can apply the same fail-closed
// iterator gate without reading ZooKeeper configuration independently.
func ResolveProperties(tableID string, scope iterrt.IteratorScope, props map[string]string) (*ResolvedStack, error) {
	return parseStack(tableID, scope, "table.iterator."+scopeString(scope)+".", props)
}

func compareUTF16(left, right string) int {
	if left == right {
		return 0
	}
	leftUnits, rightUnits := utf16.Encode([]rune(left)), utf16.Encode([]rune(right))
	for i := 0; i < len(leftUnits) && i < len(rightUnits); i++ {
		if leftUnits[i] != rightUnits[i] {
			return int(leftUnits[i]) - int(rightUnits[i])
		}
	}
	return len(leftUnits) - len(rightUnits)
}

// scopeString renders an iterator scope as the lowercase token used in
// Accumulo property names: scan|minc|majc.
func scopeString(s iterrt.IteratorScope) string {
	switch s {
	case iterrt.ScopeScan:
		return "scan"
	case iterrt.ScopeMinc:
		return "minc"
	case iterrt.ScopeMajc:
		return "majc"
	default:
		return "unknown"
	}
}

// splitPriorityClass parses "<priority>,<class>" from a table.iterator
// header property value.
func splitPriorityClass(v string) (int, string, error) {
	priS, class, ok := strings.Cut(v, ",")
	if !ok {
		return -1, "", errors.New("missing ','")
	}
	pri, err := strconv.Atoi(strings.TrimSpace(priS))
	if err != nil {
		return -1, "", fmt.Errorf("priority %q: %w", priS, err)
	}
	class = strings.TrimSpace(class)
	if class == "" {
		return pri, "", errors.New("empty class name")
	}
	return pri, class, nil
}
