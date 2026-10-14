// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"encoding/json"
	"io"
)

// Bound reference counts before the strict typed decoder allocates wide slices.
// Exact field names, types, duplicates and required presence are then enforced
// by decodeStrict. This walker retains no history or reference collections.
func preflightAdjudication(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var walk func(string, int) error
	walk = func(field string, depth int) error {
		nodes++
		if nodes > 256*1024 || depth > 32 {
			return invalidAdjudication()
		}
		token, e := d.Token()
		if e != nil {
			return e
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			if s, ok := token.(string); ok && len(s) > 16384 {
				return invalidAdjudication()
			}
			return nil
		}
		switch delimiter {
		case '{':
			fields := 0
			for d.More() {
				fields++
				if fields > 64 {
					return invalidAdjudication()
				}
				key, e := d.Token()
				if e != nil {
					return e
				}
				name, ok := key.(string)
				if !ok || len(name) > 128 {
					return invalidAdjudication()
				}
				if e = walk(name, depth+1); e != nil {
					return e
				}
			}
		case '[':
			max := 0
			switch field {
			case "receipts":
				max = 128
			case "observation_receipt_ids":
				max = 256
			case "witness_ids":
				max = 1024
			case "on_behalf_of":
				max = 64
			default:
				return invalidAdjudication()
			}
			count := 0
			for d.More() {
				count++
				if count > max {
					return invalidAdjudication()
				}
				if e := walk("", depth+1); e != nil {
					return e
				}
			}
		default:
			return invalidAdjudication()
		}
		_, e = d.Token()
		return e
	}
	if e := walk("", 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return invalidAdjudication()
	}
	return nil
}
