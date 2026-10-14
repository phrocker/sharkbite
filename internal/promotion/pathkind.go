package promotion

import (
	pathpkg "path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/phrocker/shoal-oss/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var windowsDrivePathRe = regexp.MustCompile(`^[A-Za-z]:(?:[\\/].*)?$`)
var publicationCaseFold = cases.Fold()

// Win32 short names permit a wider ASCII punctuation set than the original
// [A-Za-z0-9_] matcher used here. Accepting this conservative superset avoids
// false negatives for literal short-name spellings such as LONG$F~1.RF. "~" is
// intentionally excluded from the body-character set below because this matcher
// reserves it for the generated-short-name ordinal separator; components
// containing "~" are conservatively still treated as alias-family candidates by
// parseDOS83LiteralComponent / dos83AliasFamilyComponent rather than as
// definitely-plain 8.3 names.
const dos83BodyPunctuation = "!#$%&'()-@^_`{}"

type localPublicationIdentity struct {
	normalizedKey string
	prefix        string
	components    []localPublicationComponentIdentity
}

type localPublicationComponentIdentity struct {
	normalized          string
	dos83Ext            string
	isDOS83Literal      bool
	hasDOS83AliasFamily bool
}

func looksLikeWindowsDrivePath(path string) bool {
	return windowsDrivePathRe.MatchString(path)
}

// pathUsesBackendSeparatorJoin reports whether path is a backend URL
// root that must be joined with a literal "/" (mirroring engine's own
// joinBackendPath) rather than filepath.Join. This is deliberately
// generic -- any scheme://... path, not only the four backends this
// package knows how to canonicalize (s3/gs/az/hdfs) -- so a custom or
// future backend whose paths use their own URI scheme (for example a
// test-only or in-memory backend) still joins and validates correctly
// instead of silently falling through to a native filepath.Join that
// would collapse the scheme's "//" or use OS-native separators.
// pathLooksURLLike already excludes Windows drive paths such as
// "C://data" from looking like a URL. hdfs:/ (single slash, no
// authority) is also treated as backend-style even though it has no
// "://" substring, matching Hadoop's own authorityless HDFS URI form.
func pathUsesBackendSeparatorJoin(path string) bool {
	return pathUsesBackendSeparatorJoinOnBackend(nil, path)
}

func pathLooksURLLike(path string) bool {
	return pathLooksURLLikeOnBackend(nil, path)
}

func pathUsesBackendSeparatorJoinOnBackend(backend storage.Backend, path string) bool {
	return storage.UsesBackendPathJoin(backend, path)
}

func pathLooksURLLikeOnBackend(backend storage.Backend, path string) bool {
	return storage.ExplicitPathScheme(backend, path) != ""
}

func normalizeWindowsPublicationLabel(value string) string {
	return publicationCaseFold.String(norm.NFC.String(value))
}

// localTargetUsesWindowsADSOnBackend reports whether path would be an unsafe
// NTFS alternate-data-stream write target when interpreted as a local Windows
// destination on backend. It is intentionally lexical and conservative: any
// additional ":" inside a path component is rejected, regardless of whether it
// spells the unnamed stream (`:$DATA`, `::$DATA`) or a named stream
// (`:stream[:$DATA]`). Only the drive-letter separator is exempt.
func localTargetUsesWindowsADSOnBackend(backend storage.Backend, path string) bool {
	return storage.UsesLocalPathSemantics(backend) && windowsLocalPathContainsADS(path)
}

func windowsLocalPathContainsADS(path string) bool {
	for _, component := range splitWindowsLocalPathComponents(path) {
		if strings.Contains(component, ":") {
			return true
		}
	}
	return false
}

func splitWindowsLocalPathComponents(path string) []string {
	path = stripWindowsExtendedLengthPrefix(path)
	if hasWindowsDriveLetterPrefix(path) {
		path = path[2:]
	}
	path = strings.TrimLeft(path, `/\`)
	if path == "" {
		return nil
	}
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
}

// stripWindowsExtendedLengthPrefix removes the Win32 "\\?\" extended-length
// prefix so splitWindowsLocalPathComponents doesn't have to special-case it
// separately from the ordinary spelling of the same path: "\\?\C:\bulk\A.rf"
// becomes "C:\bulk\A.rf" (its drive letter is then trimmed the same way as
// any ordinary drive path, instead of surviving as a lone "C:" component
// that windowsLocalPathContainsADS would otherwise misread as an alternate
// data stream marker) and "\\?\UNC\server\share\A.rf" becomes
// "server\share\A.rf" (the same components an ordinary
// "\\server\share\A.rf" UNC path already produces). Any other extended-length
// form (for example a volume-GUID path) is left with its leading segment
// intact, which is harmless here since this function only looks for stray
// colons and none of those forms contain one.
func stripWindowsExtendedLengthPrefix(path string) string {
	rest, ok := strings.CutPrefix(path, `\\?\`)
	if !ok {
		return path
	}
	segment, remainder, hasMore := strings.Cut(rest, `\`)
	if strings.EqualFold(segment, "UNC") {
		return remainder
	}
	if hasMore {
		return segment + `\` + remainder
	}
	return segment
}

func hasWindowsDriveLetterPrefix(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	return (path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')
}

func normalizeLocalPathForAlias(path string) string {
	if normalized, ok := normalizeWindowsDrivePath(path); ok {
		return normalized
	}
	return filepath.Clean(path)
}

func normalizeWindowsDrivePath(path string) (string, bool) {
	if !looksLikeWindowsDrivePath(path) {
		return "", false
	}

	drive := strings.ToUpper(path[:1]) + ":"
	rest := strings.ReplaceAll(path[2:], `\`, `/`)
	rest = strings.TrimLeft(rest, "/")

	cleaned := pathpkg.Clean("/" + rest)
	if cleaned == "." {
		cleaned = "/"
	}
	return drive + strings.ReplaceAll(cleaned, "/", `\`), true
}

func isWindowsDriveRootPath(path string) bool {
	normalized, ok := normalizeWindowsDrivePath(path)
	return ok && len(normalized) == 3 && normalized[1] == ':' && normalized[2] == '\\'
}

// normalizeLocalPublicationComponent normalizes a single local path
// component for collision-safe publication-key comparison. NFC
// normalization and case-folding apply on every platform because they
// conservatively catch aliases on filesystems (Windows, and macOS's
// default HFS+/APFS) that fold case and normalize Unicode spellings;
// treating two components as equal when they might not be on some
// other filesystem only causes an unnecessary staging rejection, never
// a missed collision. Trailing-dot/space stripping is different: Win32
// silently discards trailing dots and spaces from every path component
// (so "A.rf", "A.rf.", and "A.rf " all name the same not-yet-created
// NTFS file), but POSIX filesystems store them as literal, significant
// bytes -- "A.rf." is a genuinely different filename from "A.rf" on
// Linux and macOS. Applying that stripping unconditionally would
// therefore treat truly distinct destination files as aliases outside
// Windows, so it is gated to runtime.GOOS == "windows".
func normalizeLocalPublicationComponent(component string) string {
	component = normalizeWindowsPublicationLabel(component)
	if runtime.GOOS == "windows" {
		component = strings.TrimRight(component, " .")
	}
	return component
}

func buildLocalPublicationIdentity(path string) localPublicationIdentity {
	prefix, parts := splitLocalPath(path)
	components := make([]localPublicationComponentIdentity, len(parts))
	normalizedParts := make([]string, len(parts))
	for i, part := range parts {
		components[i] = buildLocalPublicationComponentIdentity(part)
		normalizedParts[i] = components[i].normalized
	}
	normalizedPrefix := normalizeLocalPublicationPrefix(prefix)
	return localPublicationIdentity{
		normalizedKey: normalizedPrefix + strings.Join(normalizedParts, "/"),
		prefix:        normalizedPrefix,
		components:    components,
	}
}

func normalizeLocalPublicationPrefix(prefix string) string {
	if normalized, ok := normalizeWindowsUNCPublicationPrefix(prefix); ok {
		return normalized
	}
	prefix = strings.ReplaceAll(prefix, `\`, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}

func normalizeWindowsUNCPublicationPrefix(prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}
	prefix = strings.ReplaceAll(prefix, `\`, "/")
	hasRootSeparator := strings.HasSuffix(prefix, "/")
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "", false
	}

	if remainder, ok := strings.CutPrefix(prefix, "//?/"); ok {
		segment, rest, hasRest := strings.Cut(remainder, "/")

		if strings.EqualFold(segment, "unc") {
			// filepath.VolumeName on a real "\\?\UNC\server\share\..."
			// path returns only the bare "\\?\UNC" marker -- the server
			// and share names that follow are ordinary path components
			// split out separately, and already get folded the same
			// way any other component does. That leaves nothing else
			// to fold here, so the marker alone must normalize to the
			// same "//" the ordinary "\\server\share\..." form
			// contributes once its own server/share segments are moved
			// into normalizeWindowsUNCServerShare below, so extended
			// and ordinary UNC paths to the same share converge on one
			// publication identity. If a caller instead supplies the
			// server and share inline (as this function's own unit
			// tests do, for convenience), fold them here too so that
			// shape keeps working exactly as before.
			if !hasRest {
				return "//", true
			}
			return normalizeWindowsUNCServerShare(rest)
		}

		// A bare extended-length drive prefix, e.g. "\\?\C:", is the
		// only other form filepath.VolumeName recognizes under "\\?\".
		// Fold its case the same way the ordinary "C:" prefix already
		// is, so "\\?\c:\bulk\A.rf" and "C:\bulk\A.rf" resolve to the
		// identical publication identity instead of silently aliasing
		// a not-yet-created write target under two different keys.
		if !hasRest && hasWindowsDriveLetterPrefix(segment) && len(segment) == 2 {
			return strings.ToUpper(segment) + "/", true
		}

		return "", false
	}

	if remainder, ok := strings.CutPrefix(prefix, "//"); ok {
		return normalizeWindowsUNCServerShare(remainder)
	}

	// A bare drive-letter prefix with no root separator (for example
	// "c:", which splitLocalPath produces for a drive-relative path
	// like "c:bulk\A.rf") is not covered by normalizeWindowsDrivePath,
	// which only recognizes drive paths that have a separator
	// immediately after the colon; a drive-relative path falls through
	// to filepath.Clean instead, which never changes case. Fold its
	// case here too, so "c:bulk\A.rf" and "C:bulk\A.rf" -- which
	// Windows resolves against the same drive's current directory --
	// converge on the same publication prefix instead of aliasing a
	// not-yet-created write target under two different keys. Whether
	// the original prefix carried a root separator is preserved
	// (rather than always adding or dropping one), so this doesn't
	// conflate a drive-relative prefix with an absolute "C:\..." one,
	// which names a genuinely different location.
	if hasWindowsDriveLetterPrefix(prefix) && len(prefix) == 2 {
		upper := strings.ToUpper(prefix)
		if hasRootSeparator {
			upper += "/"
		}
		return upper, true
	}

	return "", false
}

// normalizeWindowsUNCServerShare folds a "server/share" (and any
// further path segments already merged into the same string) UNC
// remainder into the shared publication-prefix shape used for both
// the ordinary "\\server\share\..." spelling and, when the caller
// supplies server/share inline, the extended "\\?\UNC\server\share\..."
// spelling -- so the two converge on the same normalized prefix.
func normalizeWindowsUNCServerShare(remainder string) (string, bool) {
	parts := strings.Split(remainder, "/")
	if len(parts) < 2 {
		return "", false
	}
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		normalized = append(normalized, normalizeWindowsPublicationLabel(part))
	}
	if len(normalized) < 2 {
		return "", false
	}
	return "//" + strings.Join(normalized, "/") + "/", true
}

func buildLocalPublicationComponentIdentity(component string) localPublicationComponentIdentity {
	normalized := normalizeLocalPublicationComponent(component)
	identity := localPublicationComponentIdentity{normalized: normalized}
	if runtime.GOOS != "windows" {
		return identity
	}
	if _, ext, ok := parseDOS83LiteralComponent(normalized); ok {
		identity.dos83Ext = ext
		identity.isDOS83Literal = true
		return identity
	}
	if _, ext, ok := dos83AliasFamilyComponent(normalized); ok {
		identity.dos83Ext = ext
		identity.hasDOS83AliasFamily = true
	}
	return identity
}

// dos83PathAliases conservatively rejects Windows DOS 8.3 short-name
// ambiguities for not-yet-created local paths. It intentionally catches
// only literal short-name spellings (for example LONGFI~1.RF) against a
// longer component that could generate a short name with the same
// extension; two long components sharing a six-character prefix are not
// treated as aliases because NTFS assigns them distinct ~n ordinals
// rather than one path truncating the other. The literal-vs-family
// comparison itself does not require the stem prefixes to match, because
// NTFS can assign a hash-based short-name stem (instead of the simple
// six-character truncation) once a directory has enough short-name
// collisions, and that hashed stem cannot be predicted from the long
// name's own characters.
func dos83PathAliases(left, right localPublicationIdentity) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if left.prefix != right.prefix || len(left.components) != len(right.components) {
		return false
	}
	usedDOS83 := false
	for i := range left.components {
		if left.components[i].normalized == right.components[i].normalized {
			continue
		}
		if !dos83ComponentsAlias(left.components[i], right.components[i]) {
			return false
		}
		usedDOS83 = true
	}
	return usedDOS83
}

func dos83ComponentsAlias(left, right localPublicationComponentIdentity) bool {
	return dos83LiteralMatchesFamily(left, right) || dos83LiteralMatchesFamily(right, left)
}

func dos83LiteralMatchesFamily(literal, family localPublicationComponentIdentity) bool {
	// literal.dos83Prefix reflects only NTFS's simple "first six
	// characters plus a numeric ~n ordinal" short-name scheme. Once a
	// directory has enough same-prefix collisions, NTFS switches that
	// long component to a hashed stem that bears no predictable
	// relationship to its own leading characters, so requiring an exact
	// stem-prefix match here would silently miss that hash-based alias.
	// This check only ever runs while both candidate paths are still
	// absent (os.Stat can't disambiguate them yet) and the caller has
	// already confirmed both components share the same parent directory
	// and path depth, so conservatively treat any literal short-name
	// spelling as a possible alias of any same-extension long name that
	// still needs a short name, rather than trying to predict which of
	// NTFS's two undocumented stem-generation algorithms would apply.
	return literal.isDOS83Literal &&
		family.hasDOS83AliasFamily &&
		literal.dos83Ext == family.dos83Ext
}

func parseDOS83LiteralComponent(component string) (string, string, bool) {
	stem, ext := splitDOS83Component(component)
	tilde := strings.LastIndexByte(stem, '~')
	if tilde <= 0 || tilde > 6 || tilde >= len(stem)-1 {
		return "", "", false
	}
	prefix := stem[:tilde]
	ordinal := stem[tilde+1:]
	if !isDOS83Token(prefix, 1, 6) || strings.Contains(prefix, "~") {
		return "", "", false
	}
	if ordinal[0] == '0' || !isASCIIUnsignedInteger(ordinal) {
		return "", "", false
	}
	if !isDOS83Token(ext, 0, 3) {
		return "", "", false
	}
	return prefix, ext, true
}

func dos83AliasFamilyComponent(component string) (string, string, bool) {
	stem, ext := splitDOS83Component(component)
	if isPlainDOS83Component(stem, ext) {
		return "", "", false
	}
	prefix := sanitizeDOS83Token(stem)
	if prefix == "" {
		return "", "", false
	}
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	ext = sanitizeDOS83Token(ext)
	if len(ext) > 3 {
		ext = ext[:3]
	}
	return prefix, ext, true
}

func splitDOS83Component(component string) (string, string) {
	lastDot := strings.LastIndexByte(component, '.')
	if lastDot <= 0 {
		return component, ""
	}
	return component[:lastDot], component[lastDot+1:]
}

func isPlainDOS83Component(stem, ext string) bool {
	if strings.Contains(stem, ".") {
		return false
	}
	return isDOS83Token(stem, 1, 8) && isDOS83Token(ext, 0, 3)
}

func isDOS83Token(token string, minLen, maxLen int) bool {
	if len(token) < minLen || len(token) > maxLen {
		return false
	}
	for i := 0; i < len(token); i++ {
		if !isDOS83Char(token[i]) {
			return false
		}
	}
	return true
}

func sanitizeDOS83Token(token string) string {
	var builder strings.Builder
	builder.Grow(len(token))
	for i := 0; i < len(token); i++ {
		if isDOS83Char(token[i]) {
			builder.WriteByte(token[i])
		}
	}
	return builder.String()
}

func isDOS83Char(b byte) bool {
	if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') {
		return true
	}
	return strings.ContainsRune(dos83BodyPunctuation, rune(b))
}

func isASCIIUnsignedInteger(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
