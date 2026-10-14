package promotion

import (
	"fmt"
	"net/url"
	pathpkg "path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/storage"
)

func validatePromotionDestination(dst storage.Backend, tableName, bulkDir string) error {
	if err := validateTableName(tableName); err != nil {
		return err
	}
	if err := validateBulkDirOnBackend(dst, bulkDir); err != nil {
		return err
	}
	return validateDestinationWritable(dst)
}

func validateTableName(tableName string) error {
	trimmed := strings.TrimSpace(tableName)
	if trimmed == "" {
		return fmt.Errorf("%w: empty table name", accumulo.ErrInvalidTableName)
	}
	if trimmed != tableName {
		return fmt.Errorf("%w: %q has leading or trailing whitespace", accumulo.ErrInvalidTableName, tableName)
	}
	return nil
}

func validateBulkDir(bulkDir string) error {
	return validateBulkDirOnBackend(nil, bulkDir)
}

func validateBulkDirOnBackend(dst storage.Backend, bulkDir string) error {
	trimmed := strings.TrimSpace(bulkDir)
	if trimmed == "" {
		return fmt.Errorf("%w: empty bulk directory", accumulo.ErrInvalidBulkDir)
	}
	if trimmed != bulkDir {
		return fmt.Errorf("%w: %q has leading or trailing whitespace", accumulo.ErrInvalidBulkDir, bulkDir)
	}
	if isBackendRootOnBackend(dst, trimmed) {
		return fmt.Errorf("%w: backend root %q", accumulo.ErrInvalidBulkDir, bulkDir)
	}
	return nil
}

// validateDestinationWritable confirms dst implements
// storage.WritableBackend before any Accumulo-facing step runs. A nil
// dst is rejected the same way as any other non-writable backend: a
// nil storage.Backend interface value implements no methods at all, and a
// typed-nil pointer can retain the WritableBackend method set while still
// panicking on use. Both forms return storage.ErrReadOnly rather than being
// treated as usable destinations.
// (validateBulkDir's own nil dst argument is unrelated: it is a
// deliberate placeholder passed only to validateBulkDirOnBackend/
// isBackendRootOnBackend for callers that have no specific backend to
// scope path checks to, and never reaches this function.)
//
// Without this, a multi-tablet manifest against a read-only dst would
// let Promote's conn.AddTableSplitsForTable mutate the real destination
// table's splits before the first storage.Copy call inside
// StageBulkDir ever discovers storage.ErrReadOnly -- an Accumulo-facing
// mutation this package otherwise takes care never to make before
// every check that can be made without one has already passed (see
// Promote's own doc comment). StageBulkDir also calls this directly, so
// a caller invoking it without going through Promote gets the same
// early, clear failure instead of one buried inside its first
// storage.Copy call.
func validateDestinationWritable(dst storage.Backend) error {
	if _, ok := dst.(storage.WritableBackend); !ok || isNilBackend(dst) {
		return fmt.Errorf("%w: destination backend cannot be written to", storage.ErrReadOnly)
	}
	return nil
}

func isNilBackend(dst storage.Backend) bool {
	if dst == nil {
		return true
	}
	value := reflect.ValueOf(dst)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isBackendRoot(bulkDir string) bool {
	return isBackendRootOnBackend(nil, bulkDir)
}

func isBackendRootOnBackend(dst storage.Backend, bulkDir string) bool {
	if pathUsesBackendSeparatorJoinOnBackend(dst, bulkDir) {
		u, err := url.Parse(bulkDir)
		if err != nil {
			return false
		}
		if strings.EqualFold(storage.ExplicitPathScheme(dst, bulkDir), "hdfs") {
			return hdfsURIPathIsRoot(u.Path)
		}
		return strings.Trim(u.Path, "/\\") == ""
	}
	if isWindowsDriveRootPath(bulkDir) {
		return true
	}
	clean := filepath.Clean(bulkDir)
	sep := string(filepath.Separator)
	if clean == sep {
		return true
	}
	vol := filepath.VolumeName(clean)
	return vol != "" && clean == vol+sep
}

// hdfsURIPathIsRoot reports whether an HDFS URI's path component names the
// backend root once dot segments ("." and "..") are resolved per
// HDFS/POSIX path semantics. A root alias like "/tmp/.." -- which HDFS
// itself resolves to "/" -- would not trim to empty as a raw string, so
// this scheme cannot use the plain strings.Trim check used for other
// backend URL schemes. pathpkg.Clean maps an empty path to ".", which
// (like "") is mapped to "/" here so a bare authority root (hdfs://nn,
// with no path segment at all) is still recognized as root. This
// normalization is intentionally scoped to the HDFS scheme by the
// caller: object-store and other custom backend URL schemes must not
// resolve dot segments this way, since their keys may legitimately
// contain literal "." or ".." characters that are not directory-
// traversal tokens.
func hdfsURIPathIsRoot(rawPath string) bool {
	cleaned := pathpkg.Clean(rawPath)
	if cleaned == "." || cleaned == "" {
		cleaned = "/"
	}
	return strings.Trim(cleaned, "/\\") == ""
}
