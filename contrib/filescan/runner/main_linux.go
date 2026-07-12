//go:build linux

// Command filescan-runner executes a fixed-horizon, correctness-gated
// filesystem benchmark plan on Linux.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const metricsFDEnvironment = "BETTERLEAKS_FILESCAN_METRICS_FD"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "filescan-runner:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("filescan-runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	planPath := flags.String("plan", "", "immutable benchmark plan JSON")
	resultsPath := flags.String("results", "", "new raw JSONL result path")
	printScheduleDigests := flags.Bool("print-schedule-digests", false, "print seeded per-stratum schedule digests for a draft plan")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *planPath == "" || (*resultsPath == "" && !*printScheduleDigests) {
		return errors.New("-plan and -results are required unless -print-schedule-digests is set")
	}
	if *printScheduleDigests {
		loaded, _, err := decodePlan(*planPath)
		if err != nil {
			return err
		}
		digests, err := scheduleDigestsForDraft(loaded)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(digests)
	}
	loaded, planDigest, err := loadPlan(*planPath)
	if err != nil {
		return err
	}
	summary, err := executePlan(ctx, loaded, planDigest, *resultsPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}
