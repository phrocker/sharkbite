// Command shoal-sql runs read-only shoalql queries against a local shoal
// engine directory. It supports a one-shot --query or an interactive REPL.
//
// The dialect is SELECT-only with semantic search as the headline feature:
//
//	SELECT id, content FROM events WHERE id >= 'evt:2024' LIMIT 20
//	SELECT id, content FROM events ORDER BY embedding <-> [0.1,0.2,...] LIMIT 10
//	SELECT id FROM events WHERE MATCH(content, 'retry timeout')
//	SELECT cf, count(*) AS n FROM events GROUP BY cf
//	SELECT expand(id, 'semantic') AS nbr FROM events WHERE id = 'evt:1'
//	SELECT * FROM events AS OF 1700000000000 WHERE id = 'evt:1'
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/shoalql"
	"github.com/phrocker/shoal-oss/internal/shoalql/enginebackend"
)

func main() {
	dir := flag.String("data", "", "shoal engine data directory (required)")
	table := flag.String("table", "graph", "physical engine table backing the graph tables")
	query := flag.String("query", "", "run a single query and exit; omit for an interactive REPL")
	explain := flag.Bool("explain", false, "print the physical plan and storage policy without executing")
	embeddingSpace := flag.String(
		"embedding-space", "",
		"required embedding-space identity for vector queries",
	)
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "shoal-sql: -data is required")
		os.Exit(2)
	}

	eng, err := engine.Open(*dir, engine.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "shoal-sql: open engine: %v\n", err)
		os.Exit(1)
	}
	defer eng.Close()

	cat := shoalql.NewGraphCatalog(*table)
	exec := shoalql.NewExecutor(enginebackend.New(eng))

	run := func(sql string) {
		if err := runQuery(
			context.Background(), eng, cat, exec, sql, *explain,
			*embeddingSpace,
		); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}

	if *query != "" {
		run(*query)
		return
	}

	fmt.Println("shoal-sql REPL — type a SELECT and press enter, or \\q to quit")
	sc := bufio.NewScanner(os.Stdin)
	fmt.Print("shoalql> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "\\q" || line == "quit" || line == "exit" {
			break
		}
		if line != "" {
			run(strings.TrimSuffix(line, ";"))
		}
		fmt.Print("shoalql> ")
	}
}

func runQuery(
	ctx context.Context,
	eng *engine.Engine,
	cat shoalql.Catalog,
	exec *shoalql.Executor,
	sql string,
	explain bool,
	embeddingSpace string,
) error {
	stmt, err := shoalql.Parse(sql)
	if err != nil {
		return err
	}
	binding, ok := cat.Binding(stmt.Table)
	if !ok {
		return fmt.Errorf("unknown table %q", stmt.Table)
	}
	plan, err := shoalql.PlanQuery(ctx, stmt, binding, shoalql.PlanOptions{
		Vector: shoalql.VectorOptions{EmbeddingSpace: embeddingSpace},
	})
	if err != nil {
		return err
	}
	if explain {
		policy, err := eng.TableStoragePolicy(plan.Table)
		if err != nil {
			return err
		}
		fmt.Printf("shape=%d table=%s workload=%s write_format=%s read_formats=%s mixed=%t role=%s\n",
			plan.Shape, plan.Table, policy.Workload, policy.WriteFormat,
			storageFormats(policy.ReadFormats), policy.Mixed, policy.Role)
		return nil
	}
	res, err := exec.Run(ctx, plan)
	if err != nil {
		return err
	}

	printResult(res)
	return nil
}

func printResult(res *shoalql.Result) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		fields := make([]string, len(row))
		for i, v := range row {
			fields[i] = v.String()
		}
		fmt.Fprintln(w, strings.Join(fields, "\t"))
	}
	w.Flush()
	fmt.Printf("(%d rows)\n", len(res.Rows))
}

func storageFormats(formats []engine.StorageFormat) string {
	if len(formats) == 0 {
		return "none"
	}
	values := make([]string, len(formats))
	for i, format := range formats {
		values[i] = string(format)
	}
	return strings.Join(values, ",")
}
