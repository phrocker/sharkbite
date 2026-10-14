//go:build shoal_capi_test

package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include -I${SRCDIR}/../../capi/tests
#include "bridge.h"
#include "test_seam.h"
*/
import "C"

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

//export shoal_test_sleep_ms
func shoal_test_sleep_ms(milliseconds C.int64_t) {
	if milliseconds > 0 {
		time.Sleep(time.Duration(milliseconds) * time.Millisecond)
	}
}

type testSeamScanCursor struct {
	ctx     context.Context
	entries []accumulo.KeyValue
	index   int
	current accumulo.KeyValue
	err     error
	block   bool
}

func (c *testSeamScanCursor) Next() bool {
	if c.block {
		<-c.ctx.Done()
		c.err = c.ctx.Err()
		return false
	}
	if err := c.ctx.Err(); err != nil {
		c.err = err
		return false
	}
	if c.index >= len(c.entries) {
		return false
	}
	c.current = c.entries[c.index]
	c.index++
	return true
}

func (c *testSeamScanCursor) Entry() accumulo.KeyValue { return c.current }
func (c *testSeamScanCursor) Err() error               { return c.err }
func (c *testSeamScanCursor) Close() error             { return nil }

type testAdminConnector struct {
	mu                     sync.Mutex
	nextID                 int
	tables                 map[string]string
	properties             map[string]map[string]string
	namespaces             map[string]string
	nsProps                map[string]map[string]string
	users                  map[string][]byte
	auths                  map[string][][]byte
	permissions            map[string]bool
	splits                 map[string][][]byte
	constraints            map[string]map[int32]string
	flushFalse             int
	flushTrue              int
	flushStart             []byte
	flushEnd               []byte
	flushStartSet          bool
	flushEndSet            bool
	flushRangeWait         bool
	invalidatedTable       string
	discoveryInvalidations int
	identityBlock          atomic.Bool
	maintenanceBlock       atomic.Bool
}

func newTestAdminConnector() *testAdminConnector {
	return &testAdminConnector{
		nextID: 3,
		tables: map[string]string{
			"analytics.orders": "2",
			"events":           "1",
		},
		properties: map[string]map[string]string{
			"events": {
				"table.custom.mode":  "stream",
				"table.custom.empty": "",
			},
		},
		namespaces: map[string]string{"": "+default", "analytics": "a"},
		nsProps: map[string]map[string]string{
			"analytics": {"table.custom.namespace": "enabled"},
		},
		users:       map[string][]byte{"root": []byte("secret")},
		auths:       map[string][][]byte{"root": {[]byte("A")}},
		permissions: make(map[string]bool),
		splits:      map[string][][]byte{"events": {[]byte("m")}},
		constraints: map[string]map[int32]string{
			"events": {
				1: "org.example.First",
				3: "org.example.Third",
			},
		},
	}
}

func (c *testAdminConnector) Close() error {
	return nil
}

func (c *testAdminConnector) Principal() string {
	return "root"
}

func (c *testAdminConnector) capiConnectorIdentity(ctx context.Context) (accumulo.InstanceInfo, string, error) {
	if c.identityBlock.Load() {
		<-ctx.Done()
		return accumulo.InstanceInfo{}, "", ctx.Err()
	}
	return accumulo.InstanceInfo{Name: "test", ID: "test-id"}, c.Principal(), nil
}

func (c *testAdminConnector) NewScanner(
	accumulo.Table,
	accumulo.ScannerOptions,
) (*accumulo.Scanner, error) {
	return &accumulo.Scanner{}, nil
}

func (c *testAdminConnector) NewBatchScanner(
	accumulo.Table,
	accumulo.ScannerOptions,
) (*accumulo.BatchScanner, error) {
	return nil, accumulo.ErrDiscoveryUnavailable
}

func (c *testAdminConnector) NewBatchWriter(
	accumulo.Table,
	accumulo.BatchWriterOptions,
) (*accumulo.BatchWriter, error) {
	return nil, accumulo.ErrDiscoveryUnavailable
}

func (c *testAdminConnector) InvalidateTable(table accumulo.Table) error {
	if table.ID == "" {
		return accumulo.ErrTableNotFound
	}
	c.mu.Lock()
	c.invalidatedTable = table.ID
	c.mu.Unlock()
	return nil
}

func (c *testAdminConnector) InvalidateDiscovery() error {
	c.mu.Lock()
	c.discoveryInvalidations++
	c.mu.Unlock()
	return nil
}

func (c *testAdminConnector) Tables(ctx context.Context) ([]accumulo.Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.tables))
	for name := range c.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	tables := make([]accumulo.Table, 0, len(names))
	for _, name := range names {
		tables = append(tables, accumulo.Table{Name: name, ID: c.tables[name]})
	}
	return tables, nil
}

func (c *testAdminConnector) TableExists(ctx context.Context, name string) (bool, error) {
	if err := c.maybeTableError(ctx, name, nil); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.tables[name]
	return ok, nil
}

func (c *testAdminConnector) CreateTable(ctx context.Context, name string) error {
	if err := c.maybeTableError(ctx, name, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[name]; exists {
		return accumulo.ErrTableExists
	}
	c.tables[name] = strconv.Itoa(c.nextID)
	c.nextID++
	return nil
}

func (c *testAdminConnector) DeleteTable(ctx context.Context, name string) error {
	if err := c.maybeTableError(ctx, name, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[name]; !exists {
		return accumulo.ErrTableNotFound
	}
	delete(c.tables, name)
	delete(c.properties, name)
	delete(c.splits, name)
	return nil
}

func (c *testAdminConnector) RenameTable(ctx context.Context, oldName, newName string) error {
	if err := c.maybeTableError(ctx, oldName, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	if err := c.maybeTableError(ctx, newName, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id, exists := c.tables[oldName]
	if !exists {
		return accumulo.ErrTableNotFound
	}
	if _, exists := c.tables[newName]; exists {
		return accumulo.ErrTableExists
	}
	c.tables[newName] = id
	delete(c.tables, oldName)
	if props, ok := c.properties[oldName]; ok {
		c.properties[newName] = cloneProperties(props)
		delete(c.properties, oldName)
	}
	if splits, ok := c.splits[oldName]; ok {
		c.splits[newName] = cloneRows(splits)
		delete(c.splits, oldName)
	}
	return nil
}

func (c *testAdminConnector) FlushTable(
	ctx context.Context,
	name string,
	wait bool,
) error {
	if err := c.maybeTableError(ctx, name, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[name]; !exists {
		return accumulo.ErrTableNotFound
	}
	if wait {
		c.flushTrue++
	} else {
		c.flushFalse++
	}
	return nil
}

func (c *testAdminConnector) FlushTableRange(
	ctx context.Context,
	name string,
	startRow, endRow []byte,
	wait bool,
) error {
	if c.maintenanceBlock.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := c.maybeTableError(ctx, name, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	if startRow != nil && endRow != nil && bytes.Compare(startRow, endRow) > 0 {
		return accumulo.ErrInvalidTableRange
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[name]; !exists {
		return accumulo.ErrTableNotFound
	}
	c.flushStartSet = startRow != nil
	c.flushEndSet = endRow != nil
	c.flushStart = append([]byte(nil), startRow...)
	c.flushEnd = append([]byte(nil), endRow...)
	c.flushRangeWait = wait
	return nil
}

func (c *testAdminConnector) AddConstraint(
	ctx context.Context,
	tableName, className string,
) (int32, error) {
	if c.maintenanceBlock.Load() {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if className == "" {
		return 0, accumulo.ErrInvalidProperty
	}
	if err := c.maybeTableError(ctx, tableName, accumulo.ErrClientServiceUnavailable); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	installed := c.constraints[tableName]
	if installed == nil {
		installed = make(map[int32]string)
		c.constraints[tableName] = installed
	}
	for number, class := range installed {
		if class == className {
			return number, nil
		}
	}
	for number := int32(1); number > 0; number++ {
		if _, exists := installed[number]; !exists {
			installed[number] = className
			return number, nil
		}
	}
	return 0, accumulo.ErrConstraintNumberUnavailable
}

func (c *testAdminConnector) ListConstraints(
	ctx context.Context,
	tableName string,
) ([]accumulo.Constraint, error) {
	if c.maintenanceBlock.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := c.maybeTableError(ctx, tableName, accumulo.ErrClientServiceUnavailable); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]accumulo.Constraint, 0, len(c.constraints[tableName]))
	for number, className := range c.constraints[tableName] {
		result = append(result, accumulo.Constraint{Number: number, ClassName: className})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func (c *testAdminConnector) RemoveConstraint(
	ctx context.Context,
	tableName string,
	number int32,
) error {
	if c.maintenanceBlock.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if number <= 0 {
		return accumulo.ErrInvalidProperty
	}
	if err := c.maybeTableError(ctx, tableName, accumulo.ErrClientServiceUnavailable); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.constraints[tableName], number)
	return nil
}

func (c *testAdminConnector) flushWaitCount(wait bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait {
		return c.flushTrue
	}
	return c.flushFalse
}

func (c *testAdminConnector) SetTableProperty(
	ctx context.Context,
	tableName, property, value string,
) error {
	if property == "invalid" {
		return accumulo.ErrInvalidProperty
	}
	if err := c.maybeTableError(ctx, tableName, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[tableName]; !exists {
		return accumulo.ErrTableNotFound
	}
	if c.properties[tableName] == nil {
		c.properties[tableName] = make(map[string]string)
	}
	c.properties[tableName][property] = value
	return nil
}

func (c *testAdminConnector) RemoveTableProperty(
	ctx context.Context,
	tableName, property string,
) error {
	if property == "invalid" {
		return accumulo.ErrInvalidProperty
	}
	if err := c.maybeTableError(ctx, tableName, accumulo.ErrManagerUnavailable); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[tableName]; !exists {
		return accumulo.ErrTableNotFound
	}
	if c.properties[tableName] != nil {
		delete(c.properties[tableName], property)
	}
	return nil
}

func (c *testAdminConnector) EffectiveTableProperties(
	ctx context.Context,
	tableName string,
) (map[string]string, error) {
	if err := c.maybeTableError(ctx, tableName, accumulo.ErrClientServiceUnavailable); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tables[tableName]; !exists {
		return nil, accumulo.ErrTableNotFound
	}
	return cloneProperties(c.properties[tableName]), nil
}

func (c *testAdminConnector) maybeTableError(
	ctx context.Context,
	tableName string,
	unavailable error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch tableName {
	case "block":
		<-ctx.Done()
		return ctx.Err()
	case "denied":
		return accumulo.ErrPermissionDenied
	case "down":
		if unavailable != nil {
			return unavailable
		}
	}
	return ctx.Err()
}

func (c *testAdminConnector) Namespaces(ctx context.Context) ([]accumulo.Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.namespaces))
	for name := range c.namespaces {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]accumulo.Namespace, 0, len(names))
	for _, name := range names {
		result = append(result, accumulo.Namespace{Name: name, ID: c.namespaces[name]})
	}
	return result, nil
}

func (c *testAdminConnector) NamespaceExists(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.namespaces[name]
	return ok, nil
}

func (c *testAdminConnector) CreateNamespace(ctx context.Context, name string) error {
	if name == "block" {
		<-ctx.Done()
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.namespaces[name]; ok {
		return accumulo.ErrNamespaceExists
	}
	c.namespaces[name] = "n" + strconv.Itoa(len(c.namespaces))
	return nil
}

func (c *testAdminConnector) DeleteNamespace(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.namespaces[name]; !ok {
		return accumulo.ErrNamespaceNotFound
	}
	for table := range c.tables {
		namespace := ""
		if separator := strings.IndexByte(table, '.'); separator >= 0 {
			namespace = table[:separator]
		}
		if namespace == name {
			return accumulo.ErrNamespaceNotEmpty
		}
	}
	delete(c.namespaces, name)
	delete(c.nsProps, name)
	return nil
}

func (c *testAdminConnector) RenameNamespace(ctx context.Context, oldName, newName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.namespaces[oldName]
	if !ok {
		return accumulo.ErrNamespaceNotFound
	}
	if _, ok := c.namespaces[newName]; ok {
		return accumulo.ErrNamespaceExists
	}
	delete(c.namespaces, oldName)
	c.namespaces[newName] = id
	c.nsProps[newName] = cloneProperties(c.nsProps[oldName])
	delete(c.nsProps, oldName)
	return nil
}

func (c *testAdminConnector) SetNamespaceProperty(ctx context.Context, namespace, property, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.namespaces[namespace]; !ok {
		return accumulo.ErrNamespaceNotFound
	}
	if c.nsProps[namespace] == nil {
		c.nsProps[namespace] = make(map[string]string)
	}
	c.nsProps[namespace][property] = value
	return nil
}

func (c *testAdminConnector) RemoveNamespaceProperty(ctx context.Context, namespace, property string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.namespaces[namespace]; !ok {
		return accumulo.ErrNamespaceNotFound
	}
	delete(c.nsProps[namespace], property)
	return nil
}

func (c *testAdminConnector) EffectiveNamespaceProperties(ctx context.Context, namespace string) (map[string]string, error) {
	return c.NamespaceProperties(ctx, namespace)
}

func (c *testAdminConnector) NamespaceProperties(ctx context.Context, namespace string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.namespaces[namespace]; !ok {
		return nil, accumulo.ErrNamespaceNotFound
	}
	return cloneProperties(c.nsProps[namespace]), nil
}

func (c *testAdminConnector) VersionedNamespaceProperties(ctx context.Context, namespace string) (accumulo.VersionedProperties, error) {
	properties, err := c.NamespaceProperties(ctx, namespace)
	return accumulo.VersionedProperties{Version: 7, Properties: properties}, err
}

func (c *testAdminConnector) CreateUser(ctx context.Context, user string, password []byte) error {
	if user == "block" {
		<-ctx.Done()
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.users[user]; ok {
		return accumulo.ErrUserExists
	}
	c.users[user] = append([]byte(nil), password...)
	return nil
}

func (c *testAdminConnector) DropUser(ctx context.Context, user string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.users[user]; !ok {
		return accumulo.ErrUserNotFound
	}
	delete(c.users, user)
	delete(c.auths, user)
	for key := range c.permissions {
		parts := strings.Split(key, "\x00")
		if len(parts) == 4 && parts[1] == user {
			delete(c.permissions, key)
		}
	}
	return nil
}

func (c *testAdminConnector) ChangePassword(ctx context.Context, user string, password []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.users[user]; !ok {
		return &accumulo.SecurityError{User: user, Code: "USER_DOESNT_EXIST", Err: accumulo.ErrUserNotFound}
	}
	c.users[user] = append([]byte(nil), password...)
	return nil
}

func (c *testAdminConnector) ChangeUserAuthorizations(ctx context.Context, user string, auths [][]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.users[user]; !ok {
		return accumulo.ErrUserNotFound
	}
	c.auths[user] = cloneRows(auths)
	return nil
}

func (c *testAdminConnector) GetUserAuthorizations(ctx context.Context, user string) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.users[user]; !ok {
		return nil, accumulo.ErrUserNotFound
	}
	return cloneRows(c.auths[user]), nil
}

func permissionKey(kind, user, target string, permission int8) string {
	return kind + "\x00" + user + "\x00" + target + "\x00" + strconv.Itoa(int(permission))
}

func (c *testAdminConnector) permission(ctx context.Context, key string, set *bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if set != nil {
		c.permissions[key] = *set
	}
	return c.permissions[key], nil
}

func (c *testAdminConnector) HasSystemPermission(ctx context.Context, user string, permission accumulo.SystemPermission) (bool, error) {
	return c.permission(ctx, permissionKey("system", user, "", int8(permission)), nil)
}
func (c *testAdminConnector) HasTablePermission(ctx context.Context, user, table string, permission accumulo.TablePermission) (bool, error) {
	return c.permission(ctx, permissionKey("table", user, table, int8(permission)), nil)
}
func (c *testAdminConnector) HasNamespacePermission(ctx context.Context, user, namespace string, permission accumulo.NamespacePermission) (bool, error) {
	return c.permission(ctx, permissionKey("namespace", user, namespace, int8(permission)), nil)
}
func (c *testAdminConnector) GrantSystemPermission(ctx context.Context, user string, permission accumulo.SystemPermission) error {
	value := true
	_, err := c.permission(ctx, permissionKey("system", user, "", int8(permission)), &value)
	return err
}
func (c *testAdminConnector) RevokeSystemPermission(ctx context.Context, user string, permission accumulo.SystemPermission) error {
	value := false
	_, err := c.permission(ctx, permissionKey("system", user, "", int8(permission)), &value)
	return err
}
func (c *testAdminConnector) GrantTablePermission(ctx context.Context, user, table string, permission accumulo.TablePermission) error {
	value := true
	_, err := c.permission(ctx, permissionKey("table", user, table, int8(permission)), &value)
	return err
}
func (c *testAdminConnector) RevokeTablePermission(ctx context.Context, user, table string, permission accumulo.TablePermission) error {
	value := false
	_, err := c.permission(ctx, permissionKey("table", user, table, int8(permission)), &value)
	return err
}
func (c *testAdminConnector) GrantNamespacePermission(ctx context.Context, user, namespace string, permission accumulo.NamespacePermission) error {
	value := true
	_, err := c.permission(ctx, permissionKey("namespace", user, namespace, int8(permission)), &value)
	return err
}
func (c *testAdminConnector) RevokeNamespacePermission(ctx context.Context, user, namespace string, permission accumulo.NamespacePermission) error {
	value := false
	_, err := c.permission(ctx, permissionKey("namespace", user, namespace, int8(permission)), &value)
	return err
}

func (c *testAdminConnector) ListTableSplits(ctx context.Context, table string) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[table]; !ok {
		return nil, accumulo.ErrTableNotFound
	}
	return cloneRows(c.splits[table]), nil
}

func (c *testAdminConnector) AddTableSplits(ctx context.Context, table string, splits [][]byte) error {
	if table == "block" {
		<-ctx.Done()
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[table]; !ok {
		return accumulo.ErrTableNotFound
	}
	merged := append(cloneRows(c.splits[table]), cloneRows(splits)...)
	sort.Slice(merged, func(i, j int) bool { return bytes.Compare(merged[i], merged[j]) < 0 })
	deduped := merged[:0]
	for _, split := range merged {
		if len(deduped) == 0 || !bytes.Equal(deduped[len(deduped)-1], split) {
			deduped = append(deduped, split)
		}
	}
	c.splits[table] = deduped
	return nil
}

func cloneRows(rows [][]byte) [][]byte {
	result := make([][]byte, len(rows))
	for i := range rows {
		result[i] = append([]byte(nil), rows[i]...)
	}
	return result
}

func cloneProperties(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

type testAdminInstance struct {
	block         atomic.Bool
	configuration *accumulo.Configuration
}

func (*testAdminInstance) Info() accumulo.InstanceInfo {
	return accumulo.InstanceInfo{Name: "test", ID: "test-id"}
}

func (*testAdminInstance) Close() error {
	return nil
}

func (i *testAdminInstance) RootTabletLocation(ctx context.Context) (accumulo.TabletLocation, error) {
	if i.block.Load() {
		<-ctx.Done()
		return accumulo.TabletLocation{}, ctx.Err()
	}
	return accumulo.TabletLocation{HostPort: "tablet.example:9997", Session: "lock-1"}, nil
}

func (i *testAdminInstance) ManagerLocations(ctx context.Context) ([]string, error) {
	if i.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []string{"manager.example:9999"}, nil
}

func (i *testAdminInstance) Servers(ctx context.Context) ([]accumulo.ServerConnection, error) {
	if i.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []accumulo.ServerConnection{{
		Kind:  accumulo.TabletServerKind,
		Group: "default",
		Host:  "server.example",
		Port:  9997,
	}}, nil
}

func (*testAdminInstance) ZooKeepers() []string { return []string{"zk-a:2181", "zk-b:2181"} }
func (*testAdminInstance) Root() string         { return "/accumulo/test-id" }

func (i *testAdminInstance) Configuration() *accumulo.Configuration {
	if i.configuration == nil {
		i.configuration = accumulo.NewConfiguration()
	}
	return i.configuration
}

//export shoal_test_connector_create
func shoal_test_connector_create(outConnector **C.shoal_connector) C.int {
	if outConnector == nil {
		return 0
	}
	*outConnector = nil
	owned := newOwnedConnector(newTestAdminConnector(), &testAdminInstance{
		configuration: accumulo.NewConfiguration(),
	})
	id, ok := connectors.add(owned)
	if !ok {
		return 0
	}

	handle := C.shoal_bridge_connector_alloc(C.uint64_t(id))
	if handle == nil {
		connectors.remove(id)
		return 0
	}
	*outConnector = handle
	return 1
}

//export shoal_test_scanners_create
func shoal_test_scanners_create(
	outScanner **C.shoal_scanner,
	outBatchScanner **C.shoal_batch_scanner,
) C.int {
	if outScanner == nil || outBatchScanner == nil {
		return 0
	}
	*outScanner = nil
	*outBatchScanner = nil

	single := newOwnedScanner(nil, nil, nil)
	single.streamOne = func(ctx context.Context, _ *accumulo.Range) (scanCursorSource, error) {
		return &testSeamScanCursor{
			ctx: ctx,
			entries: []accumulo.KeyValue{
				{Key: accumulo.Key{Row: []byte("single")}, Value: []byte("1")},
			},
		}, nil
	}
	singleID, ok := scanners.add(single)
	if !ok {
		return 0
	}
	singleHandle := C.shoal_bridge_scanner_alloc(C.uint64_t(singleID))
	if singleHandle == nil {
		scanners.remove(singleID)
		return 0
	}

	batch := newOwnedScanner(nil, nil, nil)
	batch.streamMany = func(ctx context.Context, ranges []*accumulo.Range) (scanCursorSource, error) {
		entries := make([]accumulo.KeyValue, len(ranges))
		for index := range ranges {
			entries[index] = accumulo.KeyValue{
				Key:   accumulo.Key{Row: []byte("batch"), Timestamp: int64(index)},
				Value: []byte{byte(index)},
			}
		}
		return &testSeamScanCursor{ctx: ctx, entries: entries}, nil
	}
	batchID, ok := batchScanners.add(batch)
	if !ok {
		scanners.remove(singleID)
		C.shoal_bridge_scanner_free(singleHandle)
		return 0
	}
	batchHandle := C.shoal_bridge_batch_scanner_alloc(C.uint64_t(batchID))
	if batchHandle == nil {
		batchScanners.remove(batchID)
		scanners.remove(singleID)
		C.shoal_bridge_scanner_free(singleHandle)
		return 0
	}

	*outScanner = singleHandle
	*outBatchScanner = batchHandle
	return 1
}

//export shoal_test_client_create
func shoal_test_client_create(outClient **C.shoal_client) C.int {
	if outClient == nil {
		return 0
	}
	*outClient = nil
	owner := newOwnedConnector(newTestAdminConnector(), &testAdminInstance{
		configuration: accumulo.NewConfiguration(),
	})
	client := newOwnedClient(owner, "events", [][]byte{[]byte("A")}, 10)
	client.scanOne = func(
		ctx context.Context,
		_ clientSnapshot,
		_ *accumulo.Range,
	) ([]accumulo.KeyValue, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []accumulo.KeyValue{{
			Key: accumulo.Key{
				Row:             []byte("single"),
				ColumnFamily:    []byte("cf"),
				ColumnQualifier: []byte("cq"),
				Timestamp:       7,
			},
			Value: []byte("value"),
		}}, nil
	}
	client.scanMany = func(
		ctx context.Context,
		_ clientSnapshot,
		ranges []*accumulo.Range,
	) ([]accumulo.KeyValue, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values := make([]accumulo.KeyValue, len(ranges))
		for index := range ranges {
			values[index] = accumulo.KeyValue{
				Key:   accumulo.Key{Row: []byte("many"), Timestamp: int64(index)},
				Value: []byte{byte(index)},
			}
		}
		return values, nil
	}
	client.streamOne = func(
		ctx context.Context,
		_ clientSnapshot,
		scanRange *accumulo.Range,
	) (scanCursorSource, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		block := string(scanRange.StartRow()) == "block"
		return &testSeamScanCursor{
			ctx:   ctx,
			block: block,
			entries: []accumulo.KeyValue{
				{Key: accumulo.Key{Row: []byte("one"), Timestamp: 1}, Value: []byte("1")},
				{Key: accumulo.Key{Row: []byte("two"), Timestamp: 2}, Value: []byte("2")},
				{Key: accumulo.Key{Row: []byte("three"), Timestamp: 3}, Value: []byte("3")},
			},
		}, nil
	}
	client.streamMany = func(
		ctx context.Context,
		_ clientSnapshot,
		ranges []*accumulo.Range,
	) (scanCursorSource, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values := make([]accumulo.KeyValue, len(ranges))
		for index := range ranges {
			values[index] = accumulo.KeyValue{
				Key:   accumulo.Key{Row: []byte("many"), Timestamp: int64(index)},
				Value: []byte{byte(index)},
			}
		}
		if len(values) > 1 {
			values[1].Key.Timestamp = -7
			values[1].Value = []byte{0, 1, 0}
		}
		return &testSeamScanCursor{ctx: ctx, entries: values}, nil
	}
	id, ok := clients.add(client)
	if !ok {
		_ = client.close()
		return 0
	}
	handle := C.shoal_bridge_client_alloc(C.uint64_t(id))
	if handle == nil {
		clients.remove(id)
		_ = client.close()
		return 0
	}
	*outClient = handle
	return 1
}

//export shoal_test_client_columns_match
func shoal_test_client_columns_match(
	handle *C.shoal_client,
	familyValue C.shoal_bytes,
	qualifierValue C.shoal_bytes,
	hasQualifier C.uint8_t,
	columnCount C.size_t,
) C.int {
	client, err := lookupClient(handle)
	if err != nil {
		return 0
	}
	family, err := copyByteValue(familyValue, "column family")
	if err != nil {
		return 0
	}
	qualifier, err := copyByteValue(qualifierValue, "column qualifier")
	if err != nil {
		return 0
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.columns) != int(columnCount) || len(client.columns) == 0 {
		return 0
	}
	column := client.columns[len(client.columns)-1]
	if !bytes.Equal(column.Family(), family) {
		return 0
	}
	actualQualifier := column.Qualifier()
	if hasQualifier == 0 {
		if actualQualifier != nil {
			return 0
		}
	} else if !bytes.Equal(actualQualifier, qualifier) {
		return 0
	}
	return 1
}

//export shoal_test_client_settings_match
func shoal_test_client_settings_match(
	handle *C.shoal_client,
	tableName *C.char,
	authorization C.shoal_bytes,
	threadCount C.int32_t,
) C.int {
	client, err := lookupClient(handle)
	if err != nil {
		return 0
	}
	table, err := requiredString(tableName, "table_name")
	if err != nil {
		return 0
	}
	expectedAuthorization, err := copyByteValue(authorization, "authorization")
	if err != nil {
		return 0
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.table != table ||
		client.threadCount != int32(threadCount) ||
		len(client.authorizations) != 1 ||
		!bytes.Equal(client.authorizations[0], expectedAuthorization) {
		return 0
	}
	return 1
}

//export shoal_test_connector_topology_block
func shoal_test_connector_topology_block(
	handle *C.shoal_connector,
	block C.uint8_t,
) C.int {
	connector, err := lookupConnector(handle)
	if err != nil {
		return 0
	}
	instance, ok := connector.instance.(*testAdminInstance)
	if !ok {
		return 0
	}
	value, err := boolFlag(block, "block")
	if err != nil {
		return 0
	}
	instance.block.Store(value)
	return 1
}

//export shoal_test_connector_flush_wait_count
func shoal_test_connector_flush_wait_count(
	handle *C.shoal_connector,
	wait C.uint8_t,
) C.size_t {
	connector, err := lookupConnector(handle)
	if err != nil {
		return 0
	}
	admin, ok := connector.connector.(*testAdminConnector)
	if !ok {
		return 0
	}
	waitForCompletion, err := boolFlag(wait, "wait")
	if err != nil {
		return 0
	}
	return C.size_t(admin.flushWaitCount(waitForCompletion))
}

//export shoal_test_connector_identity_block
func shoal_test_connector_identity_block(
	handle *C.shoal_connector,
	block C.uint8_t,
) C.int {
	connector, err := lookupConnector(handle)
	if err != nil {
		return 0
	}
	admin, ok := connector.connector.(*testAdminConnector)
	if !ok {
		return 0
	}
	value, err := boolFlag(block, "block")
	if err != nil {
		return 0
	}
	admin.identityBlock.Store(value)
	return 1
}

//export shoal_test_connector_table_maintenance_block
func shoal_test_connector_table_maintenance_block(
	handle *C.shoal_connector,
	block C.uint8_t,
) C.int {
	connector, err := lookupConnector(handle)
	if err != nil {
		return 0
	}
	admin, ok := connector.connector.(*testAdminConnector)
	if !ok {
		return 0
	}
	value, err := boolFlag(block, "block")
	if err != nil {
		return 0
	}
	admin.maintenanceBlock.Store(value)
	return 1
}

//export shoal_test_connector_last_flush_range_matches
func shoal_test_connector_last_flush_range_matches(
	handle *C.shoal_connector,
	start *C.shoal_bytes,
	end *C.shoal_bytes,
	wait C.uint8_t,
) C.int {
	connector, err := lookupConnector(handle)
	if err != nil {
		return 0
	}
	admin, ok := connector.connector.(*testAdminConnector)
	if !ok {
		return 0
	}
	expectedStart, err := optionalRowBound(start, "start")
	if err != nil {
		return 0
	}
	expectedEnd, err := optionalRowBound(end, "end")
	if err != nil {
		return 0
	}
	expectedWait, err := boolFlag(wait, "wait")
	if err != nil {
		return 0
	}
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if admin.flushStartSet != (start != nil) ||
		admin.flushEndSet != (end != nil) ||
		admin.flushRangeWait != expectedWait ||
		!bytes.Equal(admin.flushStart, expectedStart) ||
		!bytes.Equal(admin.flushEnd, expectedEnd) {
		return 0
	}
	return 1
}

//export shoal_test_connector_invalidation_matches
func shoal_test_connector_invalidation_matches(
	handle *C.shoal_connector,
	tableID *C.char,
	discoveryCount C.size_t,
) C.int {
	connector, err := lookupConnector(handle)
	if err != nil {
		return 0
	}
	admin, ok := connector.connector.(*testAdminConnector)
	if !ok {
		return 0
	}
	expected, err := requiredString(tableID, "table_id")
	if err != nil {
		return 0
	}
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if admin.invalidatedTable != expected ||
		admin.discoveryInvalidations != int(discoveryCount) {
		return 0
	}
	return 1
}

//export shoal_test_string_alloc_fail_after
func shoal_test_string_alloc_fail_after(successfulAllocations C.size_t) {
	C.shoal_bridge_test_string_alloc_fail_after(successfulAllocations)
}

//export shoal_test_string_alloc_reset
func shoal_test_string_alloc_reset() {
	C.shoal_bridge_test_string_alloc_reset()
}

//export shoal_test_result_alloc_fail_after
func shoal_test_result_alloc_fail_after(successfulAllocations C.size_t) {
	C.shoal_bridge_test_result_alloc_fail_after(successfulAllocations)
}

//export shoal_test_result_alloc_reset
func shoal_test_result_alloc_reset() {
	C.shoal_bridge_test_result_alloc_reset()
}

//export shoal_test_error_alloc_fail_after
func shoal_test_error_alloc_fail_after(successfulAllocations C.size_t) {
	C.shoal_bridge_test_error_alloc_fail_after(successfulAllocations)
}

//export shoal_test_error_alloc_reset
func shoal_test_error_alloc_reset() {
	C.shoal_bridge_test_error_alloc_reset()
}

//export shoal_test_error_message_alloc_fail_after
func shoal_test_error_message_alloc_fail_after(successfulAllocations C.size_t) {
	C.shoal_bridge_test_error_message_alloc_fail_after(successfulAllocations)
}

//export shoal_test_error_message_alloc_reset
func shoal_test_error_message_alloc_reset() {
	C.shoal_bridge_test_error_message_alloc_reset()
}
