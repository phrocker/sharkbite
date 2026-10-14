//go:build aix || android || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris

package local

func preservePlatformXattrs(_, _ string) error {
	return nil
}
