// Command filescan-manifest creates and verifies filesystem benchmark corpus
// manifests.
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

	"github.com/betterleaks/betterleaks/contrib/filescan/internal/corpus"
	"github.com/betterleaks/betterleaks/contrib/filescan/internal/protocol"
)

const maxMetricsBytes = 16 << 20

var errVerificationFailed = errors.New("corpus verification failed")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "filescan-manifest:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: filescan-manifest <create|verify|oracle> [flags]")
	}
	switch args[1] {
	case "create":
		return runCreate(ctx, args[2:], stdout, stderr)
	case "verify":
		return runVerify(ctx, args[2:], stdout, stderr)
	case "oracle":
		return runOracle(ctx, args[2:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown subcommand %q", args[1])
	}
}

func runCreate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var options corpus.CreateOptions
	var objects stringList
	flags.StringVar(&options.Root, "root", "", "corpus root directory")
	flags.StringVar(&options.Output, "out", "", "output manifest path")
	flags.StringVar(&options.ID, "id", "", "stable corpus identifier")
	flags.StringVar(&options.WorkloadClass, "class", "", "workload class")
	flags.StringVar(&options.Role, "role", "", "calibration, confirmation, or correctness")
	flags.StringVar(&options.Source.Origin, "origin", "", "source URL or acquisition description")
	flags.StringVar(&options.Source.Revision, "revision", "", "exact source revision or snapshot")
	flags.StringVar(&options.Source.License, "license", "", "corpus license identifier")
	flags.Var(&objects, "object", "source object identifier; repeatable")
	flags.BoolVar(&options.Force, "force", false, "atomically replace existing outputs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("create: unexpected arguments: %v", flags.Args())
	}
	options.Source.Objects = append([]string(nil), objects...)
	manifest, err := corpus.Create(ctx, options)
	if err != nil {
		return err
	}
	return writeJSON(stdout, manifest)
}

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "corpus manifest path")
	root := flags.String("root", "", "override corpus root")
	mode := flags.String("mode", "all", "live, portable, sidecar, or all")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("verify: unexpected arguments: %v", flags.Args())
	}
	if *manifestPath == "" {
		return errors.New("verify: -manifest is required")
	}
	result, err := corpus.Verify(ctx, *manifestPath, *root, *mode)
	if err != nil {
		return err
	}
	if err := writeJSON(stdout, result); err != nil {
		return err
	}
	if !result.Valid {
		return errVerificationFailed
	}
	return nil
}

func runOracle(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("oracle", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "corpus manifest path")
	metricsPath := flags.String("metrics", "-", "scanner metrics JSON path, or - for stdin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("oracle: unexpected arguments: %v", flags.Args())
	}
	if *manifestPath == "" {
		return errors.New("oracle: -manifest is required")
	}
	reader := stdin
	var file *os.File
	if *metricsPath != "-" {
		var err error
		file, err = os.Open(*metricsPath)
		if err != nil {
			return fmt.Errorf("open scanner metrics: %w", err)
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxMetricsBytes+1))
	if err != nil {
		return fmt.Errorf("read scanner metrics: %w", err)
	}
	if len(data) > maxMetricsBytes {
		return errors.New("scanner metrics exceeds 16 MiB")
	}
	metrics, err := protocol.DecodeScannerMetrics(data)
	if err != nil {
		return err
	}
	manifest, err := corpus.SetOracle(ctx, *manifestPath, metrics)
	if err != nil {
		return err
	}
	return writeJSON(stdout, manifest)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

type stringList []string

func (list *stringList) String() string {
	return fmt.Sprint([]string(*list))
}

func (list *stringList) Set(value string) error {
	*list = append(*list, value)
	return nil
}
