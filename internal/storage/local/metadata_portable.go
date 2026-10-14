//go:build plan9 || js || wasip1

package local

func preservePlatformMetadata(_, _ string) error {
	return nil
}
