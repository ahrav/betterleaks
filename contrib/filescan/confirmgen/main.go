// Command confirmgen creates synthetic corpora with deterministic paths and
// contents for the filesystem scanner confirmation tournament. Each generated
// filesystem instance is frozen after creation because inode allocation and
// physical layout remain filesystem-dependent.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	defaultSequentialBytes   = int64(256) << 30
	maxSmallFileBytes        = 32 << 10
	sequentialBufferBytes    = 8 << 20
	maxArchiveEntryFootprint = 8 << 10
	maxArchiveFixedFootprint = 1 << 20
)

var errOutputCommitted = errors.New("corpus committed but parent directory sync failed")

type corpusSpec struct {
	Profile         string
	Shape           string
	SmallFiles      int64
	Groups          int64
	SequentialFiles int64
	SequentialBytes int64
	Archives        int64
	ArchiveEntries  int64
}

type generationSummary struct {
	SchemaVersion         int              `json:"schema_version"`
	Generator             string           `json:"generator"`
	GeneratorSHA256       string           `json:"generator_sha256"`
	Profile               string           `json:"profile"`
	Seed                  uint64           `json:"seed"`
	Workers               int              `json:"workers"`
	GeneratedFiles        uint64           `json:"generated_files"`
	GeneratedLogicalBytes uint64           `json:"generated_logical_bytes"`
	Parameters            map[string]int64 `json:"parameters"`
}

type counters struct {
	files atomic.Uint64
	bytes atomic.Uint64
}

type fileJob struct {
	index int64
	path  string
	size  int64
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	flags := flag.NewFlagSet("filescan-confirmgen", flag.ExitOnError)
	out := flags.String("out", "", "new corpus directory")
	profile := flags.String("profile", "", "corpus profile")
	seed := flags.Uint64("seed", 0, "nonzero deterministic content seed")
	workers := flags.Int("workers", min(64, runtime.GOMAXPROCS(0)), "parallel file writers")
	files := flags.Int64("files", 0, "override the profile's small-file count")
	totalGiB := flags.Int64("total-gib", 0, "override the profile's sequential logical GiB")
	archives := flags.Int64("archives", 0, "override the profile's archive count")
	flags.Parse(os.Args[1:])
	if flags.NArg() != 0 || *out == "" || *profile == "" || *seed == 0 {
		fmt.Fprintln(os.Stderr, "usage: filescan-confirmgen -out NEW_DIRECTORY -profile PROFILE -seed NONZERO [flags]")
		os.Exit(2)
	}
	if *files < 0 || *totalGiB < 0 || *archives < 0 {
		fmt.Fprintln(os.Stderr, "filescan-confirmgen: override counts and sizes must be nonnegative")
		os.Exit(2)
	}
	spec, err := defaultSpec(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "filescan-confirmgen:", err)
		os.Exit(2)
	}
	if *files > 0 {
		spec.SmallFiles = *files
	}
	if *totalGiB > 0 {
		if *totalGiB > int64(^uint64(0)>>1)>>30 {
			fmt.Fprintln(os.Stderr, "filescan-confirmgen: total-gib is too large")
			os.Exit(2)
		}
		spec.SequentialBytes = *totalGiB << 30
	}
	if *archives > 0 {
		spec.Archives = *archives
	}
	if err := generateCorpus(ctx, *out, spec, *seed, *workers); err != nil {
		fmt.Fprintln(os.Stderr, "filescan-confirmgen:", err)
		os.Exit(1)
	}
}

func defaultSpec(profile string) (corpusSpec, error) {
	switch profile {
	case "monorepo-wide":
		return corpusSpec{Profile: profile, Shape: "wide", SmallFiles: 300_000, Groups: 8_192}, nil
	case "monorepo-deep":
		return corpusSpec{Profile: profile, Shape: "deep", SmallFiles: 300_000, Groups: 4_096}, nil
	case "package-cache":
		return corpusSpec{Profile: profile, Shape: "package", SmallFiles: 2_000_000, Groups: 20_000}, nil
	case "bounded-small":
		return corpusSpec{Profile: profile, Shape: "bounded", SmallFiles: 50_000, Groups: 512}, nil
	case "large-sequential":
		return corpusSpec{Profile: profile, Shape: "sequential", SequentialFiles: 32, SequentialBytes: defaultSequentialBytes}, nil
	case "mixed-enterprise":
		return corpusSpec{
			Profile: profile, Shape: "mixed", SmallFiles: 100_000, Groups: 2_048,
			SequentialFiles: 64, SequentialBytes: 2 << 30, Archives: 1_000, ArchiveEntries: 20,
		}, nil
	case "archive-heavy":
		return corpusSpec{Profile: profile, Shape: "archive", Archives: 5_000, ArchiveEntries: 20}, nil
	default:
		return corpusSpec{}, fmt.Errorf("unknown profile %q (choose %s)", profile, profileNames())
	}
}

func validateGeneration(spec corpusSpec, workers int) error {
	if workers <= 0 {
		return errors.New("workers must be positive")
	}
	if spec.SmallFiles < 0 || spec.Groups < 0 || spec.SequentialFiles < 0 ||
		spec.SequentialBytes < 0 || spec.Archives < 0 || spec.ArchiveEntries < 0 {
		return errors.New("corpus counts and sizes must be nonnegative")
	}
	if spec.SmallFiles > 0 && spec.Groups <= 0 {
		return errors.New("small-file profile requires positive groups")
	}
	if spec.Groups > int64(^uint(0)>>1) {
		return fmt.Errorf("groups %d exceed the platform int range", spec.Groups)
	}
	if spec.SequentialBytes > 0 && spec.SequentialFiles <= 0 {
		return errors.New("sequential profile requires positive file count")
	}
	if spec.Archives > 0 && spec.ArchiveEntries <= 0 {
		return errors.New("archive profile requires positive entry count")
	}

	generatedFiles, ok := checkedAddUint64(uint64(spec.SmallFiles), uint64(spec.SequentialFiles))
	if ok {
		_, ok = checkedAddUint64(generatedFiles, uint64(spec.Archives))
	}
	if !ok {
		return errors.New("generated file count overflows uint64")
	}

	smallBytes, ok := checkedMulUint64(uint64(spec.SmallFiles), maxSmallFileBytes)
	if !ok {
		return errors.New("small-file logical-byte bound overflows uint64")
	}
	archiveBytes, err := archiveByteBound(spec.Archives, spec.ArchiveEntries)
	if err != nil {
		return err
	}
	totalBytes, ok := checkedAddUint64(smallBytes, uint64(spec.SequentialBytes))
	if ok {
		_, ok = checkedAddUint64(totalBytes, archiveBytes)
	}
	if !ok {
		return errors.New("generated logical-byte bound overflows uint64")
	}
	return nil
}

func archiveByteBound(archives, entries int64) (uint64, error) {
	if archives == 0 {
		return 0, nil
	}
	entryBytes, ok := checkedMulUint64(uint64(entries), maxArchiveEntryFootprint)
	if !ok {
		return 0, errors.New("per-archive byte bound overflows uint64")
	}
	perArchive, ok := checkedAddUint64(entryBytes, maxArchiveFixedFootprint)
	if !ok || perArchive > uint64(maxInt64()) {
		return 0, errors.New("per-archive byte bound exceeds int64")
	}
	total, ok := checkedMulUint64(uint64(archives), perArchive)
	if !ok {
		return 0, errors.New("archive logical-byte bound overflows uint64")
	}
	return total, nil
}

func checkedAddUint64(left, right uint64) (uint64, bool) {
	if left > ^uint64(0)-right {
		return 0, false
	}
	return left + right, true
}

func checkedMulUint64(left, right uint64) (uint64, bool) {
	if left != 0 && right > ^uint64(0)/left {
		return 0, false
	}
	return left * right, true
}

func maxInt64() int64 {
	return int64(^uint64(0) >> 1)
}

func generateCorpus(ctx context.Context, output string, spec corpusSpec, seed uint64, workers int) (err error) {
	if err := validateGeneration(spec, workers); err != nil {
		return err
	}
	if err := validateNoReplaceCommit(); err != nil {
		return err
	}

	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("output already exists: %s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	generatorSHA, err := executableSHA256()
	if err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, "."+filepath.Base(output)+".partial-")
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			err = errors.Join(err, removeTree(temporary))
		}
	}()

	var totals counters
	if spec.SmallFiles > 0 {
		if err := generateSmallFiles(ctx, temporary, spec, seed, workers, &totals); err != nil {
			return err
		}
	}
	if spec.SequentialBytes > 0 {
		if err := generateSequentialFiles(ctx, temporary, spec, seed, min(workers, 4), &totals); err != nil {
			return err
		}
	}
	if spec.Archives > 0 {
		if err := generateArchives(ctx, temporary, spec, seed, min(workers, 16), &totals); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	summary := generationSummary{
		SchemaVersion:         1,
		Generator:             "filescan-confirmgen-v2",
		GeneratorSHA256:       generatorSHA,
		Profile:               spec.Profile,
		Seed:                  seed,
		Workers:               workers,
		GeneratedFiles:        totals.files.Load(),
		GeneratedLogicalBytes: totals.bytes.Load(),
		Parameters: map[string]int64{
			"archive_entries":  spec.ArchiveEntries,
			"archives":         spec.Archives,
			"groups":           spec.Groups,
			"sequential_bytes": spec.SequentialBytes,
			"sequential_files": spec.SequentialFiles,
			"small_files":      spec.SmallFiles,
		},
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := writeExclusive(filepath.Join(temporary, "GENERATION.json"), encoded); err != nil {
		return err
	}
	if err := sealAndSyncDirectories(ctx, temporary); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	committed, commitErr := commitCorpus(temporary, output, parent)
	if committed {
		complete = true
	}
	return commitErr
}

func generateSmallFiles(
	ctx context.Context,
	root string,
	spec corpusSpec,
	seed uint64,
	workers int,
	totals *counters,
) error {
	directories := make([]string, int(spec.Groups))
	for group := range spec.Groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		directories[group] = filepath.Join(root, groupPath(spec.Shape, group))
		if err := os.MkdirAll(directories[group], 0o755); err != nil {
			return err
		}
	}
	extensions := []string{"go", "rs", "ts", "json", "md", "txt", "yaml", "proto"}
	sizes := []int64{1 << 10, 2 << 10, 4 << 10, 8 << 10, 16 << 10, 32 << 10}
	return runFileJobs(ctx, workers, func(produceCtx context.Context, jobs chan<- fileJob) error {
		for index := range spec.SmallFiles {
			mixed := mix64(seed ^ uint64(index))
			group := index % spec.Groups
			extension := extensions[mixed%uint64(len(extensions))]
			size := sizes[(mixed>>8)%uint64(len(sizes))]
			job := fileJob{
				index: index,
				path:  filepath.Join(directories[group], fmt.Sprintf("file-%09d.%s", index, extension)),
				size:  size,
			}
			select {
			case <-produceCtx.Done():
				return produceCtx.Err()
			case jobs <- job:
			}
		}
		return nil
	}, func(consumeCtx context.Context, job fileJob, scratch []byte) error {
		if err := consumeCtx.Err(); err != nil {
			return err
		}
		payload := smallPayload(scratch, spec.Profile, seed, job.index, int(job.size))
		if err := writeExclusive(job.path, payload); err != nil {
			return err
		}
		totals.files.Add(1)
		totals.bytes.Add(uint64(len(payload)))
		return nil
	})
}

func groupPath(shape string, group int64) string {
	switch shape {
	case "wide":
		return filepath.Join("components", fmt.Sprintf("component-%04d", group/16), fmt.Sprintf("subsystem-%02d", group%16), "src")
	case "deep":
		return filepath.Join(
			"platform", fmt.Sprintf("p-%02d", group%32), "layers", fmt.Sprintf("l-%02d", (group/32)%32),
			"groups", fmt.Sprintf("g-%02d", (group/1024)%16), "modules", fmt.Sprintf("module-%04d", group), "generated", "src",
		)
	case "package":
		return filepath.Join("cache", fmt.Sprintf("namespace-%03d", group/100), fmt.Sprintf("module-%05d", group), "@v", "src")
	default:
		return filepath.Join("tree", fmt.Sprintf("group-%05d", group), "content")
	}
}

func runFileJobs(
	ctx context.Context,
	workers int,
	produce func(context.Context, chan<- fileJob) error,
	consume func(context.Context, fileJob, []byte) error,
) error {
	if workers <= 0 {
		return errors.New("workers must be positive")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan fileJob)
	var firstErr error
	var errOnce sync.Once
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			scratch := make([]byte, maxSmallFileBytes)
			for {
				select {
				case <-workerCtx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					if err := consume(workerCtx, job, scratch); err != nil {
						wrapped := fmt.Errorf("create %s: %w", job.path, err)
						errOnce.Do(func() {
							firstErr = wrapped
							cancel()
						})
						return
					}
				}
			}
		})
	}
	produceErr := produce(workerCtx, jobs)
	close(jobs)
	group.Wait()
	if firstErr != nil {
		return firstErr
	}
	return produceErr
}

func smallPayload(scratch []byte, profile string, seed uint64, index int64, size int) []byte {
	prose := []byte("betterleaks synthetic confirmation corpus line with ordinary source text and no credentials\n")
	for offset := 0; offset < size; {
		offset += copy(scratch[offset:size], prose)
	}
	header := strconv.AppendUint(nil, seed, 10)
	header = append(header, ' ')
	header = strconv.AppendInt(header, index, 10)
	header = append(header, ' ')
	header = append(header, profile...)
	header = append(header, '\n')
	copy(scratch[:size], header)
	return scratch[:size]
}

func generateSequentialFiles(
	ctx context.Context,
	root string,
	spec corpusSpec,
	seed uint64,
	workers int,
	totals *counters,
) error {
	directory := filepath.Join(root, "sequential")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return runFileJobs(ctx, workers, func(produceCtx context.Context, jobs chan<- fileJob) error {
		for index := range spec.SequentialFiles {
			size := spec.SequentialBytes / spec.SequentialFiles
			if index < spec.SequentialBytes%spec.SequentialFiles {
				size++
			}
			job := fileJob{index: index, path: filepath.Join(directory, fmt.Sprintf("artifact-%04d.log", index)), size: size}
			select {
			case <-produceCtx.Done():
				return produceCtx.Err()
			case jobs <- job:
			}
		}
		return nil
	}, func(consumeCtx context.Context, job fileJob, _ []byte) error {
		value := byte('a' + mix64(seed^uint64(job.index))%20)
		if err := writeRepeated(consumeCtx, job.path, value, job.size); err != nil {
			return err
		}
		totals.files.Add(1)
		totals.bytes.Add(uint64(job.size))
		return nil
	})
}

func generateArchives(
	ctx context.Context,
	root string,
	spec corpusSpec,
	seed uint64,
	workers int,
	totals *counters,
) error {
	directory := filepath.Join(root, "archives")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return runFileJobs(ctx, workers, func(produceCtx context.Context, jobs chan<- fileJob) error {
		for index := range spec.Archives {
			job := fileJob{index: index, path: filepath.Join(directory, fmt.Sprintf("bundle-%06d.zip", index))}
			select {
			case <-produceCtx.Done():
				return produceCtx.Err()
			case jobs <- job:
			}
		}
		return nil
	}, func(consumeCtx context.Context, job fileJob, _ []byte) error {
		size, err := writeArchive(consumeCtx, job.path, spec.ArchiveEntries, seed, job.index)
		if err != nil {
			return err
		}
		totals.files.Add(1)
		totals.bytes.Add(uint64(size))
		return nil
	})
}

func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	return finishRegularFile(path, file, writeErr)
}

func writeRepeated(ctx context.Context, path string, value byte, size int64) error {
	if size < 0 {
		return errors.New("repeated file size must be nonnegative")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	buffer := bytes.Repeat([]byte{value}, sequentialBufferBytes)
	remaining := size
	var writeErr error
	for remaining > 0 {
		if writeErr = ctx.Err(); writeErr != nil {
			break
		}
		length := min(int64(len(buffer)), remaining)
		var written int
		written, writeErr = file.Write(buffer[:length])
		if writeErr == nil && int64(written) != length {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			break
		}
		remaining -= int64(written)
	}
	if writeErr == nil {
		writeErr = ctx.Err()
	}
	return finishRegularFile(path, file, writeErr)
}

func writeArchive(ctx context.Context, path string, entries int64, seed uint64, archiveIndex int64) (int64, error) {
	if entries < 0 {
		return 0, errors.New("archive entry count must be nonnegative")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	archive := zip.NewWriter(file)
	payload := make([]byte, 4<<10)
	for index := range entries {
		if err = ctx.Err(); err != nil {
			break
		}
		for offset := range payload {
			payload[offset] = byte('a' + mix64(seed^uint64(archiveIndex)<<32^uint64(index)^uint64(offset))%20)
		}
		header := &zip.FileHeader{Name: fmt.Sprintf("entries/entry-%04d.txt", index), Method: zip.Store}
		header.SetMode(0o444)
		entry, createErr := archive.CreateHeader(header)
		if createErr != nil {
			err = createErr
			break
		}
		written, writeErr := entry.Write(payload)
		if writeErr == nil && written != len(payload) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			err = writeErr
			break
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	closeArchiveErr := archive.Close()
	if err = finishRegularFile(path, file, errors.Join(err, closeArchiveErr)); err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return 0, err
	}
	return info.Size(), nil
}

func finishRegularFile(path string, file *os.File, priorErr error) error {
	var modeErr error
	var syncErr error
	if priorErr == nil {
		modeErr = file.Chmod(0o444)
	}
	if priorErr == nil && modeErr == nil {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(priorErr, modeErr, syncErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

func sealAndSyncDirectories(ctx context.Context, root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0o555); err != nil {
				return err
			}
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncDirectory(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

func removeTree(root string) error {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}

func commitCorpus(temporary, output, parent string) (bool, error) {
	if err := renameNoReplace(temporary, output); err != nil {
		return false, fmt.Errorf("commit corpus %s: %w", output, err)
	}
	if err := syncDirectory(parent); err != nil {
		return true, errors.Join(
			fmt.Errorf("%w: %s", errOutputCommitted, output),
			fmt.Errorf("sync parent directory %s: %w", parent, err),
		)
	}
	return true, nil
}

func executableSHA256() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func mix64(value uint64) uint64 {
	value += 0x9e3779b97f4a7c15
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func profileNames() string {
	return strings.Join([]string{
		"archive-heavy", "bounded-small", "large-sequential", "mixed-enterprise",
		"monorepo-deep", "monorepo-wide", "package-cache",
	}, ", ")
}
