// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// A host must explicitly mount the dataset API with its trusted export authority.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
func write(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, we := f.Write(b)
	return errors.Join(we, f.Sync(), f.Close(), syncDir(filepath.Dir(path)))
}
func run() error {
	base := flag.String("base-url", "", "authenticated Shoal host")
	id := flag.String("cohort-id", "", "registered cohort ID")
	dir := flag.String("output-dir", "", "new local output directory")
	flag.Parse()
	if *base == "" || *id == "" || *dir == "" || flag.NArg() != 0 {
		return errors.New("requires --base-url --cohort-id --output-dir")
	}
	token := os.Getenv("SHOAL_BEARER_TOKEN")
	c, e := api.NewClient(api.Config{BaseURL: *base, HTTPClient: &http.Client{Timeout: time.Minute}, Token: func(context.Context) (string, error) { return token, nil }})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, e := c.ExportDataset(ctx, shoal.ID(*id))
	if e != nil {
		return e
	}
	if e = os.Mkdir(*dir, 0700); e != nil {
		return e
	}
	if e = syncDir(filepath.Dir(*dir)); e != nil {
		return e
	}
	if e = write(filepath.Join(*dir, "dataset.json"), out.Dataset); e != nil {
		return e
	}
	if e = write(filepath.Join(*dir, "manifest.json"), out.Manifest); e != nil {
		return e
	}
	fmt.Println(out.ManifestSHA256)
	return nil
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
