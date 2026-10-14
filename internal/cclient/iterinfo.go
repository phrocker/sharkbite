//go:build !embed

package cclient

import (
	"errors"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// IterInfo describes a server-side iterator for a scan: its name (the
// instance label), class (the canonical Java class), and priority.
//
// Both Name and ClassName must be non-empty (sharkbite IterInfo.h:118-120
// — the empty() predicate that distinguishes a "real" iterinfo from the
// default-constructed sentinel).
//
// Options are an arbitrary map<string,string> attached to the iterator;
// they ride alongside the IterInfo on the wire as a `ssio` parameter on
// startScan rather than inside the IterInfo struct itself, so they are
// returned separately from ToThrift.
type IterInfo struct {
	name      string
	className string
	priority  int32
	options   map[string]string
}

// NewIterInfo constructs an IterInfo. name and className must be non-empty.
//
// References:
//   - sharkbite: include/data/constructs/IterInfo.h:71-99
func NewIterInfo(name, className string, priority int32, options map[string]string) (*IterInfo, error) {
	if name == "" {
		return nil, errors.New("cclient: IterInfo Name must be non-empty")
	}
	if className == "" {
		return nil, errors.New("cclient: IterInfo ClassName must be non-empty")
	}
	// Defensive copy — caller may mutate their map after handing it to us.
	var optsCopy map[string]string
	if len(options) > 0 {
		optsCopy = make(map[string]string, len(options))
		for k, v := range options {
			optsCopy[k] = v
		}
	}
	return &IterInfo{
		name:      name,
		className: className,
		priority:  priority,
		options:   optsCopy,
	}, nil
}

// Name returns the iterator instance name.
func (i *IterInfo) Name() string { return i.name }

// ClassName returns the iterator's canonical Java class name.
func (i *IterInfo) ClassName() string { return i.className }

// Priority returns the iterator priority. Lower values run earlier in
// the stack (Accumulo convention).
func (i *IterInfo) Priority() int32 { return i.priority }

// Options returns a defensive copy of the option map.
func (i *IterInfo) Options() map[string]string {
	if i.options == nil {
		return nil
	}
	out := make(map[string]string, len(i.options))
	for k, v := range i.options {
		out[k] = v
	}
	return out
}

// ToThrift returns the wire form. Note: the generated `data.IterInfo`
// struct does not carry options — those travel in a parallel `ssio` map
// keyed by iterator name on the startScan call.
func (i *IterInfo) ToThrift() *data.IterInfo {
	return &data.IterInfo{
		Priority:  i.priority,
		ClassName: i.className,
		IterName:  i.name,
	}
}

// IterInfoFromThrift inverts ToThrift. Options must be merged in by the
// caller from the parallel ssio map (the wire IterInfo doesn't carry them).
func IterInfoFromThrift(t *data.IterInfo) (*IterInfo, error) {
	if t == nil {
		return nil, errors.New("cclient: nil IterInfo")
	}
	return NewIterInfo(t.IterName, t.ClassName, t.Priority, nil)
}

// IterInfosToThrift bulk-converts a slice. Returns nil on empty input
// so the optional Thrift field is omitted.
func IterInfosToThrift(in []*IterInfo) []*data.IterInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]*data.IterInfo, len(in))
	for i, x := range in {
		out[i] = x.ToThrift()
	}
	return out
}

// IterInfoSSIO returns the parallel `ssio` map that startScan expects:
// `iter-name -> {option-key -> option-value}`. Iterators with no options
// are omitted (the server treats absent and empty identically).
func IterInfoSSIO(in []*IterInfo) map[string]map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]string)
	for _, x := range in {
		if len(x.options) == 0 {
			continue
		}
		opts := make(map[string]string, len(x.options))
		for k, v := range x.options {
			opts[k] = v
		}
		out[x.name] = opts
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
