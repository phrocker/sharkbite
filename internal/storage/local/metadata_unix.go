//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package local

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func preservePlatformMetadata(temp, target string) error {
	targetInfo, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("local: inspect existing metadata for %s: %w", target, err)
	}
	tempInfo, err := os.Lstat(temp)
	if err != nil {
		return fmt.Errorf("local: inspect temporary metadata for %s: %w", temp, err)
	}
	targetStat, ok := targetInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("local: inspect ownership for %s: unsupported stat payload %T", target, targetInfo.Sys())
	}
	tempStat, ok := tempInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("local: inspect ownership for %s: unsupported stat payload %T", temp, tempInfo.Sys())
	}
	if tempStat.Uid != targetStat.Uid || tempStat.Gid != targetStat.Gid {
		if err := os.Lchown(temp, int(targetStat.Uid), int(targetStat.Gid)); err != nil {
			return fmt.Errorf("local: preserve ownership for %s: %w", target, err)
		}
	}
	return preservePlatformXattrs(temp, target)
}
