package sources

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fatih/semgroup"
	"github.com/gitleaks/go-gitdiff/gitdiff"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/betterleaks/betterleaks/sources/scm"
)

// GitCmd helps to work with Git's output.
type GitCmd struct {
	cmd           *exec.Cmd
	diffFilesCh   <-chan *gitdiff.File
	fastBatchesCh <-chan []fastGitFile
	errCh         <-chan error
	repoPath      string
	cancelFastLog context.CancelFunc
}

// gitBinary resolves the git executable used for all scan subprocesses.
// BETTERLEAKS_GIT_BIN points at an alternative build (e.g. one compiled
// with -O3/PGO and a static zlib-ng, ~17% less CPU on log -p workloads
// with byte-identical output). Defaults to "git" from PATH.
func gitBinary() string {
	if bin := os.Getenv("BETTERLEAKS_GIT_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}
	return "git"
}

// gitConfigIsolationEnv builds the environment for Git subprocesses.
// It prevents Git from reading user or system configuration files and
// applies scan-tuned performance settings.
func gitConfigIsolationEnv() []string {
	var nullDevice string
	if runtime.GOOS == "windows" {
		nullDevice = "NUL"
	} else {
		nullDevice = "/dev/null"
	}
	overrides := map[string]string{
		"GIT_CONFIG_GLOBAL":      nullDevice,
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_CONFIG_SYSTEM":      nullDevice,
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_TERMINAL_PROMPT":    "0",
	}

	env := os.Environ()
	// Replace or append each override key.
	for i, e := range env {
		for k, v := range overrides {
			if strings.HasPrefix(e, k+"=") {
				env[i] = k + "=" + v
				delete(overrides, k)
			}
		}
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}

	// Diff generation dominates `git log -p` cost on repos with long delta
	// chains; the default 96 MiB delta-base cache thrashes and re-inflates
	// the same base objects repeatedly (~40% slower in our measurements);
	// 128m recovers most of the win at modest per-process memory cost.
	// GIT_CONFIG_* is used instead of `-c` so every git subprocess
	// (log/diff/cat-file) inherits it without touching each call site.
	env = append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.deltaBaseCacheLimit",
		"GIT_CONFIG_VALUE_0=128m",
	)

	// git's history-scan CPU is dominated by zlib inflate. zlib-ng (built
	// with ZLIB_COMPAT) produces byte-identical streams ~15% faster
	// end-to-end on `git log -p`. Opt in by pointing BETTERLEAKS_GIT_ZLIB
	// at a compat libz; it is injected only into git subprocess
	// environments, never the scanner's own process.
	if lib := os.Getenv("BETTERLEAKS_GIT_ZLIB"); lib != "" {
		if _, err := os.Stat(lib); err == nil {
			env = setEnvVar(env, "LD_PRELOAD", lib)
		}
	}

	// glibc malloc grows one arena per thread by default; across a fleet of
	// parallel git workers that multiplies idle heap and page-fault churn.
	// git subprocesses are effectively single-threaded, so two arenas lose
	// nothing. Only set when the caller hasn't expressed a preference.
	if os.Getenv("MALLOC_ARENA_MAX") == "" {
		env = append(env, "MALLOC_ARENA_MAX=2")
	}
	return env
}

// setEnvVar replaces key's value in env or appends it when absent.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// blobReader provides a ReadCloser interface git cat-file blob to fetch
// a blob from a repo
type blobReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

// Close closes the underlying reader and then waits for the command to complete,
// releasing its resources.
func (br *blobReader) Close() error {
	// Discard the remaining data from the pipe to avoid blocking
	_, drainErr := io.Copy(io.Discard, br)
	// Close the pipe (should signal the command to stop if it hasn't already)
	closeErr := br.ReadCloser.Close()
	// Wait to prevent zombie processes.
	waitErr := br.cmd.Wait()
	// Return the first error encountered
	if drainErr != nil {
		return drainErr
	}
	if closeErr != nil {
		return closeErr
	}
	return waitErr
}

// NewGitLogCmd returns `*DiffFilesCmd` with two channels: `<-chan *gitdiff.File` and `<-chan error`.
// Caller should read everything from channels until receiving a signal about their closure and call
// the `func (*DiffFilesCmd) Wait()` error in order to release resources.
//
// Deprecated: use NewGitLogCmdContext instead.
func NewGitLogCmd(source string, logOpts string) (*GitCmd, error) {
	return NewGitLogCmdContext(context.Background(), source, logOpts)
}

// NewGitLogCmdContext is the same as NewGitLogCmd but supports passing in a
// context to use for timeouts
func NewGitLogCmdContext(ctx context.Context, source string, logOpts string) (*GitCmd, error) {
	return newGitLogCmdContext(ctx, source, logOpts, false)
}

// newGitLogScanCmdContext starts a Git log command whose default-shaped
// stdout is consumed directly by Git.Fragments. It remains private because a
// native scan command intentionally has no public DiffFilesCh stream.
func newGitLogScanCmdContext(ctx context.Context, source string, logOpts string) (*GitCmd, error) {
	return newGitLogCmdContext(ctx, source, logOpts, true)
}

func newGitLogCmdContext(ctx context.Context, source string, logOpts string, directScan bool) (*GitCmd, error) {
	sourceClean := filepath.Clean(source)
	var cmd *exec.Cmd
	hasUserOpts := logOpts != ""
	if hasUserOpts {
		args := []string{"-C", sourceClean, "log", "-p", "-U0"}

		userArgs, err := splitGitLogOpts(logOpts)
		if err != nil {
			return nil, fmt.Errorf("invalid --log-opts: %w", err)
		}

		args = append(args, userArgs...)
		cmd = exec.CommandContext(ctx, gitBinary(), args...)
	} else {
		cmd = exec.CommandContext(ctx, gitBinary(), "-C", sourceClean, "log", "-p", "-U0",
			"--full-history", "--all", "--diff-filter=tuxdb")
	}
	cmd.Env = gitConfigIsolationEnv()

	logging.Debug().Msgf("executing: %s", cmd.String())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	errCh := make(chan error, 1)
	go listenForStdErr(stderr, errCh)

	// User --log-opts can change the stream format (--pretty, -U3, ...);
	// only the default betterleaks-shaped stream goes through the fast
	// parser (see fastParseGitLog).
	gitCmd := &GitCmd{
		cmd:      cmd,
		errCh:    errCh,
		repoPath: sourceClean,
	}
	var gitdiffFiles <-chan *gitdiff.File
	if hasUserOpts {
		gitdiffFiles, err = gitdiff.Parse(stdout)
	} else if directScan {
		configureFastGitScan(ctx, gitCmd, stdout)
	} else {
		gitdiffFiles, err = fastParseGitLog(stdout)
	}
	if err != nil {
		return nil, err
	}

	if gitdiffFiles != nil {
		gitCmd.diffFilesCh = gitdiffFiles
	}
	return gitCmd, nil
}

func configureFastGitScan(ctx context.Context, cmd *GitCmd, r io.Reader) {
	parserCtx, cancel := context.WithCancel(ctx)
	cmd.cancelFastLog = cancel
	cmd.fastBatchesCh = asyncFastGitLogBatches(parserCtx, r, fastGitBatchSize)
}

// splitGitLogOpts parses user-provided --log-opts with a small shell-inspired
// tokenizer.
//
// Supported behavior:
//   - whitespace splits arguments unless inside quotes
//   - single and double quotes group text and are removed from output
//   - backslash escapes the next rune outside single quotes
//   - unmatched quote or trailing backslash returns an error
//
// This is intentionally not a full shell parser: no variable expansion,
// command substitution, glob expansion, or other shell features. Also, a
// standalone empty quoted token (for example ”) is currently dropped.
func splitGitLogOpts(input string) ([]string, error) {
	var (
		args     []string
		curr     strings.Builder
		inSingle bool
		inDouble bool
		escaped  bool
	)

	flush := func() {
		if curr.Len() == 0 {
			return
		}
		args = append(args, curr.String())
		curr.Reset()
	}

	for _, r := range input {
		switch {
		case escaped:
			curr.WriteRune(r)
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case unicode.IsSpace(r) && !inSingle && !inDouble:
			flush()
		default:
			curr.WriteRune(r)
		}
	}

	if escaped {
		return nil, errors.New("unterminated escape in --log-opts")
	}
	if inSingle || inDouble {
		return nil, errors.New("unterminated quote in --log-opts")
	}

	flush()
	return args, nil
}

// NewGitDiffCmd returns `*DiffFilesCmd` with two channels: `<-chan *gitdiff.File` and `<-chan error`.
// Caller should read everything from channels until receiving a signal about their closure and call
// the `func (*DiffFilesCmd) Wait()` error in order to release resources.
//
// Deprecated: use NewGitDiffCmdContext instead.
func NewGitDiffCmd(source string, staged bool) (*GitCmd, error) {
	return NewGitDiffCmdContext(context.Background(), source, staged)
}

// NewGitDiffCmdContext is the same as NewGitDiffCmd but supports passing in a
// context to use for timeouts
func NewGitDiffCmdContext(ctx context.Context, source string, staged bool) (*GitCmd, error) {
	sourceClean := filepath.Clean(source)
	var cmd *exec.Cmd
	cmd = exec.CommandContext(ctx, gitBinary(), "-C", sourceClean, "diff", "-U0", "--no-ext-diff", ".")
	if staged {
		cmd = exec.CommandContext(ctx, gitBinary(), "-C", sourceClean, "diff", "-U0", "--no-ext-diff",
			"--staged", ".")
	}
	cmd.Env = gitConfigIsolationEnv()
	logging.Debug().Msgf("executing: %s", cmd.String())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	errCh := make(chan error, 1)
	go listenForStdErr(stderr, errCh)

	gitdiffFiles, err := gitdiff.Parse(stdout)
	if err != nil {
		return nil, err
	}

	return &GitCmd{
		cmd:         cmd,
		diffFilesCh: gitdiffFiles,
		errCh:       errCh,
		repoPath:    sourceClean,
	}, nil
}

// DiffFilesCh returns a channel with *gitdiff.File.
func (c *GitCmd) DiffFilesCh() <-chan *gitdiff.File {
	return c.diffFilesCh
}

// ErrCh returns a channel that could produce an error if there is something in stderr.
func (c *GitCmd) ErrCh() <-chan error {
	return c.errCh
}

// Wait waits for the command to exit and waits for any copying to
// stdin or copying from stdout or stderr to complete.
//
// Wait also closes underlying stdout and stderr.
func (c *GitCmd) Wait() error {
	return c.cmd.Wait()
}

// String displays the command used for GitCmd
func (c *GitCmd) String() string {
	return c.cmd.String()
}

// NewBlobReader returns an io.ReadCloser that can be used to read a blob
// within the git repo used to create the GitCmd.
//
// The caller is responsible for closing the reader.
//
// Deprecated: use NewBlobReaderContext instead.
func (c *GitCmd) NewBlobReader(commit, path string) (io.ReadCloser, error) {
	return c.NewBlobReaderContext(context.Background(), commit, path)
}

// NewBlobReaderContext is the same as NewBlobReader but supports passing in a
// context to use for timeouts
func (c *GitCmd) NewBlobReaderContext(ctx context.Context, commit, path string) (io.ReadCloser, error) {
	gitArgs := []string{"-C", c.repoPath, "cat-file", "blob", commit + ":" + path}
	cmd := exec.CommandContext(ctx, gitBinary(), gitArgs...)
	cmd.Env = gitConfigIsolationEnv()
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start git command: %w", err)
	}
	return &blobReader{
		ReadCloser: stdout,
		cmd:        cmd,
	}, nil
}

// listenForStdErr listens for stderr output from git, prints it to stdout,
// sends to errCh and closes it.
func listenForStdErr(stderr io.ReadCloser, errCh chan<- error) {
	defer close(errCh)

	var errLines []string

	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		// if git throws one of the following errors:
		//
		//  exhaustive rename detection was skipped due to too many files.
		//  you may want to set your diff.renameLimit variable to at least
		//  (some large number) and retry the command.
		//
		//	inexact rename detection was skipped due to too many files.
		//  you may want to set your diff.renameLimit variable to at least
		//  (some large number) and retry the command.
		//
		//  Auto packing the repository in background for optimum performance.
		//  See "git help gc" for manual housekeeping.
		//
		// we skip exiting the program as git log -p/git diff will continue
		// to send data to stdout and finish executing. This next bit of
		// code prevents gitleaks from stopping mid scan if this error is
		// encountered
		if strings.Contains(scanner.Text(),
			"exhaustive rename detection was skipped") ||
			strings.Contains(scanner.Text(),
				"inexact rename detection was skipped") ||
			strings.Contains(scanner.Text(),
				"you may want to set your diff.renameLimit") ||
			strings.Contains(scanner.Text(),
				"See \"git help gc\" for manual housekeeping") ||
			strings.Contains(scanner.Text(),
				"Auto packing the repository in background for optimum performance") {
			logging.Warn().Msg(scanner.Text())
		} else {
			line := scanner.Text()
			logging.Error().Msgf("[git] %s", line)
			errLines = append(errLines, line)
		}
	}

	if len(errLines) > 0 {
		errCh <- fmt.Errorf("git stderr: %s", strings.Join(errLines, "; "))
	}
}

// Git is a source for yielding fragments from a git repo
type Git struct {
	Cmd             *GitCmd
	ShouldSkip      SkipFunc
	Platform        scm.Platform
	RemoteURL       string
	Sema            *semgroup.Group
	MaxArchiveDepth int
}

func rawAddedText(tf *gitdiff.TextFragment) string {
	if len(tf.Lines) == 1 && tf.Lines[0].Op == gitdiff.OpAdd {
		return tf.Lines[0].Line
	}
	return tf.Raw(gitdiff.OpAdd)
}

type gitScanFile struct {
	fastHeader    *fastGitHeader
	diffHeader    *gitdiff.PatchHeader
	newName       string
	isDelete      bool
	isBinary      bool
	native        bool
	fastFragments []fastGitFragment
	diffFragments []*gitdiff.TextFragment
}

// Fragments yields fragments from a git repo
func (s *Git) Fragments(ctx context.Context, yield FragmentsFunc) error {
	defer func() {
		if err := s.Cmd.Wait(); err != nil {
			logging.Debug().Err(err).Str("cmd", s.Cmd.String()).Msg("command aborted")
		}
	}()
	if s.Cmd.cancelFastLog != nil {
		defer s.Cmd.cancelFastLog()
	}

	var wg sync.WaitGroup
	var err error
	if s.Cmd.fastBatchesCh != nil {
		err = s.consumeFastBatches(ctx, yield, &wg)
	} else {
		err = s.consumeDiffFiles(ctx, yield, &wg)
	}
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		wg.Wait()
		return nil
	}
}

func (s *Git) consumeFastBatches(ctx context.Context, yield FragmentsFunc, wg *sync.WaitGroup) error {
	fastBatchesCh := s.Cmd.fastBatchesCh
	errCh := s.Cmd.ErrCh()
	for fastBatchesCh != nil || errCh != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case batch, open := <-fastBatchesCh:
			if !open {
				fastBatchesCh = nil
				continue
			}
			for _, f := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				s.scheduleGitFile(ctx, yield, wg, gitScanFile{
					fastHeader:    f.header,
					newName:       f.newName,
					isDelete:      f.isDelete,
					isBinary:      f.isBinary,
					native:        true,
					fastFragments: f.fragments,
				})
			}
		case err, open := <-errCh:
			if !open {
				errCh = nil
				continue
			}
			return yield(Fragment{}, err)
		}
	}
	return nil
}

func (s *Git) consumeDiffFiles(ctx context.Context, yield FragmentsFunc, wg *sync.WaitGroup) error {
	diffFilesCh := s.Cmd.DiffFilesCh()
	errCh := s.Cmd.ErrCh()
	for diffFilesCh != nil || errCh != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, open := <-diffFilesCh:
			if !open {
				diffFilesCh = nil
				continue
			}
			s.scheduleGitFile(ctx, yield, wg, gitScanFile{
				diffHeader:    f.PatchHeader,
				newName:       f.NewName,
				isDelete:      f.IsDelete,
				isBinary:      f.IsBinary,
				diffFragments: f.TextFragments,
			})
		case err, open := <-errCh:
			if !open {
				errCh = nil
				continue
			}
			return yield(Fragment{}, err)
		}
	}
	return nil
}

func (s *Git) scheduleGitFile(ctx context.Context, yield FragmentsFunc, wg *sync.WaitGroup, scanFile gitScanFile) {
	if scanFile.isDelete {
		return
	}

	yieldAsArchive := false
	if scanFile.isBinary {
		if !isArchive(ctx, scanFile.newName) {
			return
		}
		yieldAsArchive = true
	}

	var (
		commitSHA         string
		commitMessage     string
		commitDate        time.Time
		commitAuthorName  string
		commitAuthorEmail string
		hasHeader         bool
		hasAuthor         bool
	)
	if scanFile.native {
		if h := scanFile.fastHeader; h != nil {
			hasHeader = true
			commitSHA = h.sha
			commitMessage = h.message
			commitDate = h.authorDate
			if h.author.valid {
				hasAuthor = true
				commitAuthorName = h.author.name
				commitAuthorEmail = h.author.email
			}
		}
	} else if h := scanFile.diffHeader; h != nil {
		hasHeader = true
		commitSHA = h.SHA
		commitMessage = h.Message()
		commitDate = h.AuthorDate
		if h.Author != nil {
			hasAuthor = true
			commitAuthorName = h.Author.Name
			commitAuthorEmail = h.Author.Email
		}
	}

	commitAttrs := make(map[string]string)
	if hasHeader {
		commitAttrs[AttrGitSHA] = commitSHA
		commitAttrs[AttrGitMessage] = commitMessage
		commitAttrs[AttrResource] = ResourceGitPatchContent
		commitAttrs[AttrPath] = scanFile.newName
		if s.RemoteURL != "" {
			commitAttrs[AttrGitRemoteURL] = s.RemoteURL
			commitAttrs[AttrGitPlatform] = s.Platform.String()
		}
		if !commitDate.IsZero() {
			commitAttrs[AttrGitDate] = commitDate.UTC().Format(time.RFC3339)
		}
		if hasAuthor {
			commitAttrs[AttrGitAuthorName] = commitAuthorName
			commitAttrs[AttrGitAuthorEmail] = commitAuthorEmail
		}

		if shouldSkipAttrs(s.ShouldSkip, commitAttrs) {
			logging.Trace().
				Str("commit", commitSHA).
				Str("path", scanFile.newName).
				Msg("skipping diff entry: global prefilter")
			return
		}
	}

	wg.Add(1)
	s.Sema.Go(func() error {
		defer wg.Done()

		if yieldAsArchive {
			blob, err := s.Cmd.NewBlobReaderContext(ctx, commitSHA, scanFile.newName)
			if err != nil {
				logging.Error().Err(err).Msg("could not read archive blob")
				return nil
			}

			file := File{
				Content:         blob,
				Path:            scanFile.newName,
				MaxArchiveDepth: s.MaxArchiveDepth,
				ShouldSkip:      s.ShouldSkip,
			}

			err = file.Fragments(ctx, func(fragment Fragment, err error) error {
				attrs := maps.Clone(commitAttrs)
				maps.Copy(attrs, fragment.Attributes)
				fragment.Attributes = attrs
				return yield(fragment, err)
			})
			if err := blob.Close(); err != nil {
				logging.Debug().Err(err).Msg("blobReader.Close() returned an error")
			}
			return err
		}

		if scanFile.native {
			for _, textFragment := range scanFile.fastFragments {
				fragment := Fragment{
					Raw:        textFragment.raw,
					StartLine:  int(textFragment.newPosition),
					Attributes: commitAttrs,
				}
				fragment.SetAttr(AttrPath, scanFile.newName)
				if err := yield(fragment, nil); err != nil {
					return err
				}
			}
			return nil
		}

		for _, textFragment := range scanFile.diffFragments {
			if textFragment == nil {
				return nil
			}
			fragment := Fragment{
				Raw:        rawAddedText(textFragment),
				StartLine:  int(textFragment.NewPosition),
				Attributes: commitAttrs,
			}
			fragment.SetAttr(AttrPath, scanFile.newName)
			if err := yield(fragment, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResolveRemote resolves the SCM platform and remote URL for the given source.
// It replaces the deprecated NewRemoteInfo/NewRemoteInfoContext functions.
func ResolveRemote(ctx context.Context, platform scm.Platform, source string) (scm.Platform, string) {
	if platform == scm.NoPlatform {
		return platform, ""
	}

	remoteUrl, err := getRemoteUrl(ctx, source)
	if err != nil {
		if strings.Contains(err.Error(), "No remote configured") {
			logging.Debug().Msg("skipping finding links: repository has no configured remote.")
			platform = scm.NoPlatform
		} else {
			logging.Error().Err(err).Msg("skipping finding links: unable to parse remote URL")
		}
		return platform, ""
	}

	if platform == scm.UnknownPlatform {
		platform = platformFromHost(remoteUrl)
		if platform == scm.UnknownPlatform {
			logging.Info().
				Str("host", remoteUrl.Hostname()).
				Msg("Unknown SCM platform. Use --platform to include links in findings.")
		} else {
			logging.Debug().
				Str("host", remoteUrl.Hostname()).
				Str("platform", platform.String()).
				Msg("SCM platform parsed from host")
		}
	}

	return platform, remoteUrl.String()
}

var sshUrlpat = regexp.MustCompile(`^git@([a-zA-Z0-9.-]+):(?:\d{1,5}/)?([\w/.-]+?)(?:\.git)?$`)

func getRemoteUrl(ctx context.Context, source string) (*url.URL, error) {
	// This will return the first remote — typically, "origin".
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--quiet", "--get-url")
	cmd.Env = gitConfigIsolationEnv()
	if source != "." {
		cmd.Dir = source
	}

	stdout, err := cmd.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, fmt.Errorf("command failed (%d): %w, stderr: %s", exitError.ExitCode(), err, string(bytes.TrimSpace(exitError.Stderr)))
		}
		return nil, err
	}

	remoteUrl := string(bytes.TrimSpace(stdout))
	if matches := sshUrlpat.FindStringSubmatch(remoteUrl); matches != nil {
		remoteUrl = fmt.Sprintf("https://%s/%s", matches[1], matches[2])
	}
	remoteUrl = strings.TrimSuffix(remoteUrl, ".git")

	parsedUrl, err := url.Parse(remoteUrl)
	if err != nil {
		return nil, fmt.Errorf("unable to parse remote URL: %w", err)
	}

	// Remove any user info.
	parsedUrl.User = nil
	return parsedUrl, nil
}

func platformFromHost(u *url.URL) scm.Platform {
	switch strings.ToLower(u.Hostname()) {
	case "github.com":
		return scm.GitHubPlatform
	case "gitlab.com":
		return scm.GitLabPlatform
	case "dev.azure.com", "visualstudio.com":
		return scm.AzureDevOpsPlatform
	case "gitea.com", "code.forgejo.org", "codeberg.org":
		return scm.GiteaPlatform
	case "bitbucket.org":
		return scm.BitbucketPlatform
	default:
		return scm.UnknownPlatform
	}
}
