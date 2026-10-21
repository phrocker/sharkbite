// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/tableschema"
)

// cmdSchema records and reads well-known table schema descriptors
// (proto/schema.proto) in the reserved _shoal_schema table.
//
//	shoal-embed schema put  --table graph --file graph-schema.json
//	shoal-embed schema get  --table graph [--version N]
//	shoal-embed schema list
func cmdSchema(args []string) {
	if len(args) == 0 {
		schemaUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "put":
		cmdSchemaPut(args[1:])
	case "get":
		cmdSchemaGet(args[1:])
	case "list":
		cmdSchemaList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "schema: unknown subcommand %q\n", args[0])
		schemaUsage()
		os.Exit(1)
	}
}

func schemaUsage() {
	fmt.Fprintf(os.Stderr, `Usage: shoal-embed schema <put|get|list> [flags]

  put   Record a descriptor (JSON, shoal.schema.v1.TableSchema) as the newest
        version for the table it names; --file or stdin. The described table
        must exist. The reserved %s table is created on the first put.
  get   Print the newest (or --version N) descriptor recorded for --table.
  list  Print every described table with its newest schema id and version.

`, tableschema.Table)
}

func openSchemaEngine(fs *flag.FlagSet, args []string, command string) (*engine.Engine, func() error) {
	backend := registerBackendFlags(fs)
	dataDir := fs.String("data", defaultDataDir(), "data directory")
	fs.Parse(args)
	eng, closeEngine, err := backend.openEngine(context.Background(), *dataDir, engine.Options{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		die("schema %s: %v", command, err)
	}
	return eng, closeEngine
}

func cmdSchemaPut(args []string) {
	fs := flag.NewFlagSet("schema put", flag.ExitOnError)
	tableName := fs.String("table", "", "table the descriptor must describe (required)")
	file := fs.String("file", "", "descriptor JSON file (default: stdin)")
	eng, closeEngine := openSchemaEngine(fs, args, "put")
	defer closeEngine()

	if *tableName == "" {
		die("schema put: --table is required")
	}
	var (
		data []byte
		err  error
	)
	if *file == "" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, tableschema.MaxDescriptorBytes+1))
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		die("schema put: read descriptor: %v", err)
	}
	schema, err := tableschema.Decode(data)
	if err != nil {
		die("schema put: %v", err)
	}
	if schema.GetTable() != *tableName {
		die("schema put: descriptor describes table %q, --table names %q", schema.GetTable(), *tableName)
	}
	entry, err := tableschema.New(eng).Put(schema)
	if err != nil {
		die("schema put: %v", err)
	}
	fmt.Printf("recorded %s schema %s version %d (%d bytes)\n",
		entry.Table, entry.Schema.GetSchemaId(), entry.Version, len(entry.Bytes))
}

func cmdSchemaGet(args []string) {
	fs := flag.NewFlagSet("schema get", flag.ExitOnError)
	tableName := fs.String("table", "", "described table (required)")
	version := fs.Int64("version", 0, "a recorded version (default: the newest)")
	eng, closeEngine := openSchemaEngine(fs, args, "get")
	defer closeEngine()

	if *tableName == "" {
		die("schema get: --table is required")
	}
	store := tableschema.New(eng)
	var (
		entry tableschema.Entry
		err   error
	)
	if *version == 0 {
		entry, err = store.Get(*tableName)
	} else {
		entry, err = store.GetVersion(*tableName, *version)
	}
	if err != nil {
		die("schema get: %v", err)
	}
	pretty, err := tableschema.Indent(entry.Bytes)
	if err != nil {
		die("schema get: %v", err)
	}
	os.Stdout.Write(pretty)
	fmt.Println()
}

func cmdSchemaList(args []string) {
	fs := flag.NewFlagSet("schema list", flag.ExitOnError)
	eng, closeEngine := openSchemaEngine(fs, args, "list")
	defer closeEngine()

	entries, err := tableschema.New(eng).Described()
	if err != nil {
		die("schema list: %v", err)
	}
	if len(entries) == 0 {
		fmt.Println("no described tables")
		return
	}
	fmt.Printf("%-24s %-32s %8s\n", "TABLE", "SCHEMA", "VERSION")
	for _, entry := range entries {
		fmt.Printf("%-24s %-32s %8d\n", entry.Table, entry.Schema.GetSchemaId(), entry.Version)
	}
}

// schemaColumn renders a table's manifest pointer for the status listing.
func schemaColumn(pointer *engine.SchemaPointer) string {
	if pointer == nil {
		return "-"
	}
	return fmt.Sprintf("%s@%d", pointer.ID, pointer.Version)
}
