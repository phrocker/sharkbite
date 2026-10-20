// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// ErrExists reports that a bundle with the same digest is already published.
// It wraps storage.ErrExists so callers can test either sentinel.
var ErrExists = fmt.Errorf("weights bundle already published: %w", storage.ErrExists)

// ErrPublicationIndeterminate reports that durable publication could not be
// confirmed. The bundle may or may not be visible; the caller must inspect
// before retrying, never assume absence.
var ErrPublicationIndeterminate = errors.New("weights bundle publication indeterminate")

// Stage writes the manifest into a staged directory after verifying every
// member, so the directory is a complete bundle before it is published.
func Stage(root string, m Manifest) (string, error) {
	if err := Verify(root, m); err != nil {
		return "", err
	}
	encoded, err := m.Encode()
	if err != nil {
		return "", err
	}
	if err := writeExclusive(filepath.Join(root, ManifestName), encoded); err != nil {
		return "", err
	}
	return m.Digest()
}

// PublishLocal moves a staged bundle to dest without replacing anything that
// is already there. dest's parent must exist. The rename is atomic, so a
// reader sees either no bundle or a complete one. Publication is durable when
// PublishLocal returns nil; an error after the rename reports
// ErrPublicationIndeterminate.
func PublishLocal(staging, dest string) error {
	m, digest, err := Load(staging)
	if err != nil {
		return err
	}
	_ = m
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, digest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
	}
	if err := syncTree(staging); err != nil {
		return err
	}
	if err := renameNoReplace(staging, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, digest)
		}
		return err
	}
	if err := syncDirectory(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
	}
	return nil
}

// Publish copies a verified local bundle into backend under prefix/<digest>/.
// Member files are written first and the manifest last with exclusive
// creation, so a bundle is visible only once every member is durable. Any
// backend that offers exclusive creation can hold bundles; nothing beyond
// Create, CreateExclusive, and Open is used.
func Publish(ctx context.Context, backend storage.ExclusiveCreator, prefix, root string) (string, error) {
	m, digest, err := Load(root)
	if err != nil {
		return "", err
	}
	base := bundlePrefix(prefix, digest)
	if _, err := backend.Open(ctx, base+ManifestName); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, digest)
	}
	for _, f := range m.Files {
		src, err := os.Open(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrVerification, err)
		}
		err = writeObject(ctx, backend, base+f.Path, src, false)
		src.Close()
		if err != nil {
			return "", err
		}
	}
	encoded, err := m.Encode()
	if err != nil {
		return "", err
	}
	if err := writeObject(ctx, backend, base+ManifestName, bytes.NewReader(encoded), true); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return "", fmt.Errorf("%w: %s", ErrExists, digest)
		}
		return "", err
	}
	return digest, nil
}

// Fetch materializes a published bundle from backend into dest and verifies
// every digest before returning. dest must not exist. Verification failure
// leaves nothing at dest.
func Fetch(ctx context.Context, backend storage.Backend, prefix, digest, dest string) (Manifest, error) {
	if !validDigest(digest) {
		return Manifest{}, fmt.Errorf("%w: digest", ErrInvalidManifest)
	}
	base := bundlePrefix(prefix, digest)
	raw, err := readObject(ctx, backend, base+ManifestName, MaxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrVerification, err)
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return Manifest{}, err
	}
	actual, err := m.Digest()
	if err != nil {
		return Manifest{}, err
	}
	if actual != digest {
		return Manifest{}, fmt.Errorf("%w: manifest digest %s does not match %s", ErrVerification, actual, digest)
	}
	staging := dest + ".fetching"
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrVerification, err)
	}
	cleanup := func() { os.RemoveAll(staging) }
	for _, f := range m.Files {
		obj, err := backend.Open(ctx, base+f.Path)
		if err != nil {
			cleanup()
			return Manifest{}, fmt.Errorf("%w: missing %q: %v", ErrVerification, f.Path, err)
		}
		target := filepath.Join(staging, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			obj.Close()
			cleanup()
			return Manifest{}, fmt.Errorf("%w: %v", ErrVerification, err)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			obj.Close()
			cleanup()
			return Manifest{}, fmt.Errorf("%w: %v", ErrVerification, err)
		}
		_, copyErr := io.Copy(out, io.LimitReader(io.NewSectionReader(obj, 0, obj.Size()), f.Size+1))
		closeErr := errors.Join(out.Sync(), out.Close(), obj.Close())
		if copyErr != nil || closeErr != nil {
			cleanup()
			return Manifest{}, fmt.Errorf("%w: read %q", ErrVerification, f.Path)
		}
	}
	if err := writeExclusive(filepath.Join(staging, ManifestName), raw); err != nil {
		cleanup()
		return Manifest{}, err
	}
	if err := Verify(staging, m); err != nil {
		cleanup()
		return Manifest{}, err
	}
	if err := PublishLocal(staging, dest); err != nil {
		cleanup()
		return Manifest{}, err
	}
	return m, nil
}

func bundlePrefix(prefix, digest string) string {
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return digest + "/"
	}
	return prefix + "/" + digest + "/"
}

func writeObject(ctx context.Context, backend storage.ExclusiveCreator, path string, src io.Reader, exclusive bool) error {
	var w storage.Writer
	var err error
	if exclusive {
		w, err = backend.CreateExclusive(ctx, path)
	} else {
		w, err = backend.Create(ctx, path)
	}
	if err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	if _, err := io.Copy(w, src); err != nil {
		if aborter, ok := w.(storage.Aborter); ok {
			aborter.Abort()
		} else {
			w.Close()
		}
		return fmt.Errorf("publish %s: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}

func readObject(ctx context.Context, backend storage.Backend, path string, max int64) ([]byte, error) {
	obj, err := backend.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(obj, 0, obj.Size()), max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, max)
	}
	return b, nil
}

func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, filepath.Base(path))
		}
		return err
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	parentErr := syncDirectory(filepath.Dir(path))
	if err := errors.Join(writeErr, syncErr, closeErr, parentErr); err != nil {
		return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// syncTree fsyncs every file and directory under root, deepest first, so the
// rename that publishes the bundle never exposes unsynced contents.
func syncTree(root string) error {
	var dirs, files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		} else {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
	}
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, p := range dirs {
		if err := syncDirectory(p); err != nil {
			return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
		}
	}
	return nil
}
