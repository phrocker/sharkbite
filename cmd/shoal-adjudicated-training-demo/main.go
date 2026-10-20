// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/phrocker/shoal-oss/internal/adjudicatedfixture"
)

func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, we := f.Write(b)
	se := f.Sync()
	ce := f.Close()
	if err = errors.Join(we, se, ce); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
func prepare(dir, name string) error {
	r, eng, authority, ctx, err := adjudicatedfixture.Prepare(dir, name)
	if err != nil {
		return err
	}
	defer eng.Close()
	exporter, session, err := adjudicatedfixture.ExportSession(ctx, r, authority.Resolver(), r.Cohort().ID)
	if err != nil {
		return err
	}
	bundle, err := exporter.Export(ctx, r.Cohort().ID)
	if err != nil {
		return err
	}
	initial, final := session.Proofs()
	captureBytes, err := json.Marshal(struct {
		Schema  int                             `json:"schema"`
		Kind    string                          `json:"kind"`
		Initial adjudicatedfixture.CaptureProof `json:"initial"`
		Final   adjudicatedfixture.CaptureProof `json:"final"`
	}{1, "registered-inventory-export-capture", initial, final})
	if err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(dir, "cohort-capture.json"), captureBytes); err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(dir, "dataset.json"), bundle.Dataset); err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(dir, "manifest.json"), bundle.Manifest); err != nil {
		return err
	}
	fmt.Println(bundle.ManifestSHA256)
	return nil
}
func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use prepare --task source|review --out DIR, or inquire")
	}
	switch args[0] {
	case "prepare":
		f := flag.NewFlagSet("prepare", flag.ContinueOnError)
		task := f.String("task", "source", "source or review")
		out := f.String("out", "", "new output directory")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" || f.NArg() != 0 {
			return fmt.Errorf("--out new-directory required")
		}
		return prepare(*out, *task)
	case "inquire":
		return runInquiry(args[1:])
	default:
		return fmt.Errorf("unknown command")
	}
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
