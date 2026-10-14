// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func run() error {
	base := flag.String("base-url", "", "authenticated Shoal host")
	key := flag.String("key", "", "stable idempotency key; retain with the exact report")
	file := flag.String("observation-file", "", "bounded schema-1 outcome request JSON; POST mode")
	request := flag.String("request-id", "", "decoded registered request ID; GET mode")
	prediction := flag.String("prediction-id", "", "decoded prediction ID; GET mode")
	flag.Parse()
	post := *file != ""
	if *base == "" || *key == "" || flag.NArg() != 0 || (post && (*request != "" || *prediction != "")) || (!post && (*request == "" || *prediction == "")) {
		return errors.New("requires --base-url --key and either --observation-file or --request-id plus --prediction-id")
	}
	token := os.Getenv("SHOAL_BEARER_TOKEN")
	c, e := api.NewClient(api.Config{BaseURL: *base, HTTPClient: &http.Client{Timeout: time.Minute}, Token: func(context.Context) (string, error) { return token, nil }})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var receipt api.OutcomeReceipt
	if post {
		f, e := os.Open(*file)
		if e != nil {
			return e
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, api.MaxOutcomeRequestBytes+1))
		closeErr := f.Close()
		if e = errors.Join(readErr, closeErr); e != nil {
			return e
		}
		observation, e := api.DecodeOutcomeRequest(raw)
		if e != nil {
			return e
		}
		receipt, e = c.AppendOutcome(ctx, observation, []byte(*key))
		if e != nil {
			if errors.Is(e, api.ErrIndeterminate) {
				return fmt.Errorf("%w; retain this key and exact body, then retry POST to reconcile; GET does not repair publication", e)
			}
			return e
		}
	} else {
		receipt, e = c.ReadOutcome(ctx, shoal.ID(*request), shoal.ID(*prediction), []byte(*key))
		if e != nil {
			return e
		}
	}
	raw, e := api.EncodeOutcomeReceipt(receipt)
	if e != nil {
		return e
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
