//go:build (linux && !android) || (darwin && !ios)

package local

import (
	"bytes"
	"fmt"
	"strings"
)

func preservePlatformXattrs(temp, target string) error {
	return reconcilePlatformXattrs(temp, target, platformXattrOperations())
}

type xattrOperations struct {
	list   func(string) ([]string, error)
	get    func(string, string) ([]byte, error)
	set    func(string, string, []byte, int) error
	remove func(string, string) error
}

func reconcilePlatformXattrs(temp, target string, ops xattrOperations) error {
	targetNames, err := ops.list(target)
	if err != nil {
		return fmt.Errorf("local: list extended attributes for %s: %w", target, err)
	}
	tempNames, err := ops.list(temp)
	if err != nil {
		return fmt.Errorf("local: list extended attributes for %s: %w", temp, err)
	}

	targetSet := make(map[string]struct{}, len(targetNames))
	tempSet := make(map[string]struct{}, len(tempNames))
	for _, name := range targetNames {
		if !preserveContentXattr(name) {
			continue
		}
		targetSet[name] = struct{}{}
	}
	for _, name := range tempNames {
		tempSet[name] = struct{}{}
	}
	for _, name := range tempNames {
		if _, ok := targetSet[name]; ok {
			continue
		}
		if err := ops.remove(temp, name); err != nil {
			return fmt.Errorf("local: remove inherited extended attribute %s from %s: %w", name, temp, err)
		}
	}
	for _, name := range targetNames {
		if !preserveContentXattr(name) {
			continue
		}
		value, err := ops.get(target, name)
		if err != nil {
			return fmt.Errorf("local: read extended attribute %s for %s: %w", name, target, err)
		}
		if _, ok := tempSet[name]; ok {
			tempValue, err := ops.get(temp, name)
			if err != nil {
				return fmt.Errorf("local: read extended attribute %s for %s: %w", name, temp, err)
			}
			if bytes.Equal(tempValue, value) {
				continue
			}
		}
		if err := ops.set(temp, name, value, 0); err != nil {
			return fmt.Errorf("local: preserve extended attribute %s for %s: %w", name, target, err)
		}
	}
	return nil
}

func preserveContentXattr(name string) bool {
	// Linux clears file capabilities when file contents change. The atomic
	// rewrite path should mirror that in-place truncation behavior rather than
	// restoring privilege-bearing content security labels onto rewritten bytes.
	switch name {
	case "security.capability", "security.ima", "security.evm":
		return false
	default:
		return true
	}
}

func splitXattrNames(raw []byte) []string {
	parts := strings.Split(string(raw), "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
