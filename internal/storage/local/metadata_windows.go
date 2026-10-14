//go:build windows

package local

func preservePlatformMetadata(_, _ string) error {
	return nil
}
