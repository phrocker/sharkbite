package storage

import (
	"regexp"
	"strings"
)

var backendURLRootRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):\/\/`)

// PathSchemeProvider optionally declares the URL-style path schemes a backend
// expects. This lets callers disambiguate roots like x://bucket/path from a
// Windows drive spelling such as C://data when the backend's scheme is only
// one character long.
type PathSchemeProvider interface {
	BackendPathSchemes() []string
}

// LocalPathSemanticProvider marks backends whose paths should always be
// interpreted with local-filesystem semantics, regardless of whether the
// spelling resembles a backend URI such as hdfs:/bulk.
type LocalPathSemanticProvider interface {
	UsesLocalPathSemantics() bool
}

type backendUnwrapper interface {
	InnerBackend() Backend
}

// ExplicitPathScheme returns the backend-style scheme for path when path is
// explicitly qualified. HDFS's authorityless hdfs:/... form counts as an
// explicit scheme too, case-insensitively. Single-character schemes are
// treated as backend URLs only when backend declares that scheme through
// PathSchemeProvider; otherwise they remain local-path spellings so Windows
// drive roots like C://data keep local semantics.
func ExplicitPathScheme(backend Backend, path string) string {
	if UsesLocalPathSemantics(backend) {
		return ""
	}
	if hasAuthoritylessHDFSScheme(path) {
		return "hdfs"
	}
	matches := backendURLRootRe.FindStringSubmatch(path)
	if len(matches) != 2 {
		return ""
	}
	scheme := strings.ToLower(matches[1])
	if len(scheme) != 1 || backendDeclaresScheme(backend, scheme) {
		return scheme
	}
	return ""
}

func hasAuthoritylessHDFSScheme(path string) bool {
	return len(path) >= len("hdfs:/") && strings.EqualFold(path[:len("hdfs:/")], "hdfs:/")
}

// UsesBackendPathJoin reports whether root should join children with literal
// "/" semantics instead of filepath.Join. It shares ExplicitPathScheme's
// single-character-scheme policy so joiners can preserve valid x:// roots
// without misclassifying Windows drive spellings like C://data as remote.
func UsesBackendPathJoin(backend Backend, root string) bool {
	return ExplicitPathScheme(backend, root) != ""
}

// UsesLocalPathSemantics reports whether backend declares that its paths are
// local filesystem paths, even when their spelling happens to resemble a
// backend URI.
func UsesLocalPathSemantics(backend Backend) bool {
	backend = unwrapBackend(backend)
	provider, ok := backend.(LocalPathSemanticProvider)
	return ok && provider.UsesLocalPathSemantics()
}

func backendDeclaresScheme(backend Backend, scheme string) bool {
	backend = unwrapBackend(backend)
	provider, ok := backend.(PathSchemeProvider)
	if !ok {
		return false
	}
	for _, candidate := range provider.BackendPathSchemes() {
		if strings.EqualFold(candidate, scheme) {
			return true
		}
	}
	return false
}

func unwrapBackend(backend Backend) Backend {
	for backend != nil {
		unwrapper, ok := backend.(backendUnwrapper)
		if !ok {
			break
		}
		inner := unwrapper.InnerBackend()
		if inner == nil || inner == backend {
			break
		}
		backend = inner
	}
	return backend
}
