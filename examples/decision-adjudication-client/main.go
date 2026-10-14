// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"net/http"
	"os"
	"time"
)

func run() error {
	base := flag.String("base-url", "", "authenticated Shoal host")
	key := flag.String("key", "", "stable idempotency key; POST only")
	file := flag.String("proposal-file", "", "bounded schema-1 adjudication proposal JSON; POST mode")
	target := flag.String("target-id", "", "decoded target ID; GET history mode")
	flag.Parse()
	post := *file != ""
	if *base == "" || flag.NArg() != 0 || (post && (*key == "" || *target != "")) || (!post && (*target == "" || *key != "")) {
		return errors.New("requires --base-url and either --proposal-file plus --key or --target-id")
	}
	token := os.Getenv("SHOAL_BEARER_TOKEN")
	c, e := api.NewClient(api.Config{BaseURL: *base, HTTPClient: &http.Client{Timeout: time.Minute}, Token: func(context.Context) (string, error) { return token, nil }})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var raw []byte
	if post {
		f, e := os.Open(*file)
		if e != nil {
			return e
		}
		body, readErr := io.ReadAll(io.LimitReader(f, api.MaxAdjudicationRequestBytes+1))
		closeErr := f.Close()
		if e = errors.Join(readErr, closeErr); e != nil {
			return e
		}
		p, e := api.DecodeAdjudicationRequest(body)
		if e != nil {
			return e
		}
		r, e := c.Adjudicate(ctx, p, []byte(*key))
		if e != nil {
			if errors.Is(e, api.ErrIndeterminate) {
				return fmt.Errorf("%w; retain and retry the exact proposal and key, including the original expected head and version", e)
			}
			return e
		}
		raw, e = api.EncodeAdjudicationReceipt(r)
		if e != nil {
			return e
		}
	} else {
		h, e := c.AdjudicationHistory(ctx, shoal.ID(*target))
		if e != nil {
			return e
		}
		raw, e = api.EncodeAdjudicationHistory(h)
		if e != nil {
			return e
		}
	}
	_, e = fmt.Println(string(raw))
	return e
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
