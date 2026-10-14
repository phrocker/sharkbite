package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/phrocker/shoal-oss/internal/deployment"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "shoalctl:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("expected command: plan, status, transition, embedding-backfill, or policy")
	}
	switch args[0] {
	case "plan":
		return runPlan(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "transition":
		return runTransition(args[1:], stdout, stderr)
	case "embedding-backfill":
		return runEmbeddingBackfill(args[1:], stdout, stderr)
	case "policy":
		return runPolicy(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q; expected plan, status, transition, embedding-backfill, or policy", args[0])
	}
}

func runPlan(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	modeValue := flags.String("mode", "", "deployment mode: single, distributed, or accumulo")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("plan accepts no positional arguments")
	}
	mode, err := deployment.ParseMode(*modeValue)
	if err != nil {
		return err
	}
	plan, err := deployment.PlanFor(mode)
	if err != nil {
		return err
	}
	printPlan(stdout, plan)
	return nil
}

func runStatus(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("state", "", "path to deployment state JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("status accepts no positional arguments")
	}
	if *path == "" {
		return errors.New("--state is required")
	}
	state, err := deployment.Load(*path)
	if err != nil {
		return err
	}
	printState(stdout, state)
	return nil
}

func runTransition(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("transition", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("state", "", "path to deployment state JSON")
	targetValue := flags.String("to", "", "desired deployment mode")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("transition accepts no positional arguments")
	}
	if *path == "" {
		return errors.New("--state is required")
	}
	target, err := deployment.ParseMode(*targetValue)
	if err != nil {
		return err
	}
	state, err := deployment.Load(*path)
	if err != nil {
		return err
	}
	result, err := deployment.Transition(state, target)
	if err != nil {
		return err
	}
	if !result.Idempotent {
		if err := deployment.Save(*path, result.State); err != nil {
			return err
		}
	}

	fmt.Fprintf(stdout, "accepted: %t\n", !result.Idempotent)
	fmt.Fprintf(stdout, "idempotent: %t\n", result.Idempotent)
	printState(stdout, result.State)
	printList(stdout, "requirements", result.Requirements)
	printList(stdout, "warnings", result.Warnings)
	return nil
}

func printPlan(output io.Writer, plan deployment.Plan) {
	fmt.Fprintf(output, "mode: %s\n", plan.Mode)
	fmt.Fprintf(output, "authority: %s\n", plan.Authority)
	printList(output, "components", plan.Components)
	printList(output, "requirements", plan.Requirements)
	printList(output, "warnings", plan.Warnings)
}

func printState(output io.Writer, state deployment.State) {
	fmt.Fprintf(output, "current mode: %s\n", state.CurrentMode)
	fmt.Fprintf(output, "desired mode: %s\n", state.DesiredMode)
	fmt.Fprintf(output, "phase: %s\n", state.Phase)
	fmt.Fprintf(output, "authority: %s\n", state.Authority)
	fmt.Fprintf(output, "generation: %d\n", state.Generation)
	fmt.Fprintf(output, "last validation: %s\n", state.LastValidation)
}

func printList(output io.Writer, heading string, values []string) {
	fmt.Fprintf(output, "%s:\n", heading)
	if len(values) == 0 {
		fmt.Fprintln(output, "- none")
		return
	}
	for _, value := range values {
		fmt.Fprintf(output, "- %s\n", value)
	}
}
