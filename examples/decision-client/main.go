// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// decision-client invokes a request already registered by a trusted Shoal host.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision/api"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	base := flag.String("url", "", "Shoal host origin, e.g. https://shoal.example")
	idText := flag.String("request", "", "canonical base64url registered request ID")
	keyText := flag.String("key", "", "canonical base64url durable idempotency key; reuse for reconciliation")
	evaluate := flag.Bool("evaluate", false, "evaluate the registered request; otherwise read its receipt")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	id, err := api.DecodeID(*idText)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	key, err := api.DecodeKey(*keyText)
	if err != nil {
		return fmt.Errorf("key: %w", err)
	}
	client, err := api.NewClient(api.Config{
		BaseURL: *base, HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Token: func(context.Context) (string, error) { return os.Getenv("SHOAL_TOKEN"), nil },
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var response api.Response
	if *evaluate {
		response, err = client.Evaluate(ctx, id, key)
	} else {
		response, err = client.Read(ctx, id, key)
	}
	if errors.Is(err, api.ErrIndeterminate) {
		return errors.New("decision outcome indeterminate; retain the same request and key and reconcile with an authorized read; keep full review enabled")
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(response)
}
