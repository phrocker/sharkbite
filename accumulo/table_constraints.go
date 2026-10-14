package accumulo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// ConstraintPropertyPrefix is the table-property prefix Accumulo stores
// constraints under: table.constraint.<number> holds the constraint class name.
const ConstraintPropertyPrefix = "table.constraint."

// Constraint is one installed constraint: the class a tablet server loads and
// the number that identifies it within the table.
type Constraint struct {
	// ClassName is the fully qualified constraint class.
	ClassName string

	// Number identifies the constraint within its table. Accumulo numbers
	// constraints from 1 and never reuses a live number.
	Number int32
}

// constraintAllocationAttempts bounds how many times AddConstraint re-reads and
// retries when another writer takes the number it picked.
const constraintAllocationAttempts = 5

// AddConstraint installs a constraint on a table and returns the number
// Accumulo assigned it, mirroring Sharkbite's
// tableOperations.addConstraint(className).
//
// A constraint is a table property: the class is stored under
// table.constraint.<number>, and the number is the lowest positive integer the
// table is not already using. Installing a class the table already carries is
// idempotent — the existing number is returned and no property is written —
// because a duplicate class under a second number would run the same check
// twice on every mutation.
//
// Allocation is check-then-write, which two writers could otherwise resolve to
// the same number. Calls through one connector are serialized, and every call
// reads the property back after writing: if another client took the number
// first, the write is retried against a fresh view rather than reporting a
// number that holds someone else's class.
//
// **Safe shim for unsafe C++ behavior:** the pinned Sharkbite implementation is
// a stub. AccumuloTableOperations::addConstraint has an empty body that returns
// 0, so a Python caller is told a constraint was installed with number 0, no
// property is written, and mutations that the constraint would have rejected are
// accepted forever. Shoal installs the constraint and returns its real number.
// See the compatibility matrix, SB-UNSAFE-036.
func (c *Connector) AddConstraint(
	ctx context.Context,
	tableName, className string,
) (int32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if tableName == "" {
		return 0, fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	if err := validateConstraintClassName(className); err != nil {
		return 0, err
	}

	c.constraintMu.Lock()
	defer c.constraintMu.Unlock()

	for attempt := 0; attempt < constraintAllocationAttempts; attempt++ {
		owned, err := c.versionedTableProperties(ctx, tableName)
		if err != nil {
			return 0, err
		}
		effective, err := c.EffectiveTableProperties(ctx, tableName)
		if err != nil {
			return 0, err
		}
		// Allocate against everything the table sees, so a table-local number
		// cannot shadow one a namespace already installed, and write against
		// the table's own set, which is what the versioned write governs.
		visible := parseConstraints(mergeProperties(effective, owned.Properties))
		if number, found := constraintNumberOf(visible, className); found {
			return number, nil
		}

		number := nextConstraintNumber(visible)
		next := mergeProperties(owned.Properties, nil)
		next[ConstraintPropertyPrefix+strconv.FormatInt(int64(number), 10)] = className
		err = c.modifyTableProperties(ctx, tableName, managerclient.VersionedProperties{
			Version:    owned.Version,
			Properties: next,
		})
		if err == nil {
			return number, nil
		}
		if !isConcurrentModification(err) {
			return 0, err
		}
	}
	return 0, fmt.Errorf(
		"%w: table %q after %d attempts",
		ErrConstraintNumberUnavailable, tableName, constraintAllocationAttempts,
	)
}

// versionedTableProperties reads the table's own properties and the version a
// compare-and-set write must present.
//
// getVersionedTableProperties is a ClientService RPC, not a manager one, so it
// resolves client-service endpoints and retries them the way every other
// property read does; only the write goes to the manager.
func (c *Connector) versionedTableProperties(
	ctx context.Context,
	tableName string,
) (managerclient.VersionedProperties, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return managerclient.VersionedProperties{}, ErrConnectorClosed
	}
	resolver := c.clientAddr
	manager := c.manager
	c.mu.RUnlock()
	if resolver == nil || manager == nil {
		return managerclient.VersionedProperties{}, ErrDiscoveryUnavailable
	}
	addresses, err := resolver.Addresses(ctx)
	if errors.Is(err, zk.ErrClientServiceUnavailable) {
		return managerclient.VersionedProperties{}, ErrClientServiceUnavailable
	}
	if err != nil {
		return managerclient.VersionedProperties{}, fmt.Errorf(
			"accumulo: discover client service: %w", err,
		)
	}

	var endpointErr error
	for _, address := range addresses {
		properties, err := manager.GetVersionedTableProperties(ctx, address, tableName)
		if err == nil {
			return properties, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return managerclient.VersionedProperties{}, ctxErr
		}
		if !managerclient.IsRetryableEndpointError(err) {
			return managerclient.VersionedProperties{}, mapTablePropertyReadError(tableName, err)
		}
		endpointErr = errors.Join(endpointErr, fmt.Errorf("%s: %w", address, err))
	}
	if endpointErr == nil {
		return managerclient.VersionedProperties{}, ErrClientServiceUnavailable
	}
	return managerclient.VersionedProperties{}, fmt.Errorf(
		"%w: %w", ErrClientServiceUnavailable, endpointErr,
	)
}

// modifyTableProperties replaces the table's own properties at a version.
func (c *Connector) modifyTableProperties(
	ctx context.Context,
	tableName string,
	properties managerclient.VersionedProperties,
) error {
	manager, address, err := c.managerEndpoint(ctx)
	if err != nil {
		return err
	}
	if err := manager.ModifyTableProperties(ctx, address, tableName, properties); err != nil {
		return mapManagerPropertyError(tableName, ConstraintPropertyPrefix, err)
	}
	return nil
}

// managerEndpoint resolves the manager adapter and address, applying the same
// lifecycle checks every table operation performs.
func (c *Connector) managerEndpoint(ctx context.Context) (managerclient.Adapter, string, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, "", ErrConnectorClosed
	}
	manager := c.manager
	resolver := c.managerAddr
	c.mu.RUnlock()
	if manager == nil || resolver == nil {
		return nil, "", ErrDiscoveryUnavailable
	}
	address, err := resolver.Address(ctx)
	if errors.Is(err, zk.ErrManagerUnavailable) {
		return nil, "", ErrManagerUnavailable
	}
	if err != nil {
		return nil, "", fmt.Errorf("accumulo: discover manager: %w", err)
	}
	return manager, address, nil
}

// isConcurrentModification reports whether a versioned write lost its
// compare-and-set and should be retried against a fresh read.
func isConcurrentModification(err error) bool {
	var managerErr *managerclient.Error
	return errors.As(err, &managerErr) && managerErr.Kind == managerclient.ErrorConcurrentModification
}

// mergeProperties returns a new map holding base overlaid with overlay.
func mergeProperties(base, overlay map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(overlay))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range overlay {
		merged[key] = value
	}
	return merged
}

// ListConstraints returns the constraints installed on a table, ordered by
// number. Sharkbite has no listing operation, but a caller that cannot see the
// installed set cannot decide what to remove, and the numbers AddConstraint
// returns are only meaningful against it.
func (c *Connector) ListConstraints(ctx context.Context, tableName string) ([]Constraint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tableName == "" {
		return nil, fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	properties, err := c.EffectiveTableProperties(ctx, tableName)
	if err != nil {
		return nil, err
	}
	return parseConstraints(properties), nil
}

// RemoveConstraint removes the constraint a table installed under number,
// which is the operation AddConstraint's return value exists for. Removing a
// number the table does not carry is not an error: the property is already
// absent, which is the state the caller asked for.
func (c *Connector) RemoveConstraint(ctx context.Context, tableName string, number int32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tableName == "" {
		return fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	if number < 1 {
		return fmt.Errorf(
			"%w: constraint number %d is not positive", ErrInvalidProperty, number,
		)
	}
	property := ConstraintPropertyPrefix + strconv.FormatInt(int64(number), 10)
	return c.RemoveTableProperty(ctx, tableName, property)
}

// parseConstraints extracts the constraint entries from a table's effective
// properties. A property whose suffix is not a positive integer is not a
// constraint Accumulo would load, so it is ignored rather than reported.
func parseConstraints(properties map[string]string) []Constraint {
	var constraints []Constraint
	for property, className := range properties {
		if !strings.HasPrefix(property, ConstraintPropertyPrefix) {
			continue
		}
		suffix := property[len(ConstraintPropertyPrefix):]
		number, err := strconv.ParseInt(suffix, 10, 32)
		if err != nil || number < 1 {
			continue
		}
		if className == "" {
			continue
		}
		constraints = append(constraints, Constraint{
			ClassName: className,
			Number:    int32(number),
		})
	}
	sort.Slice(constraints, func(i, j int) bool {
		return constraints[i].Number < constraints[j].Number
	})
	return constraints
}

// nextConstraintNumber returns the lowest positive number the table is not
// using, which is how Accumulo assigns one.
func nextConstraintNumber(installed []Constraint) int32 {
	used := make(map[int32]struct{}, len(installed))
	for _, constraint := range installed {
		used[constraint.Number] = struct{}{}
	}
	for number := int32(1); ; number++ {
		if _, taken := used[number]; !taken {
			return number
		}
	}
}

// constraintNumberOf reports the number a class is installed under.
func constraintNumberOf(installed []Constraint, className string) (int32, bool) {
	for _, constraint := range installed {
		if constraint.ClassName == className {
			return constraint.Number, true
		}
	}
	return 0, false
}

// constraintClassAt reports the class installed under a number.
func constraintClassAt(installed []Constraint, number int32) (string, bool) {
	for _, constraint := range installed {
		if constraint.Number == number {
			return constraint.ClassName, true
		}
	}
	return "", false
}

func validateConstraintClassName(className string) error {
	if className == "" {
		return fmt.Errorf("%w: empty constraint class name", ErrInvalidProperty)
	}
	if strings.ContainsAny(className, " \t\r\n") {
		return fmt.Errorf(
			"%w: constraint class name %q holds whitespace", ErrInvalidProperty, className,
		)
	}
	return nil
}
