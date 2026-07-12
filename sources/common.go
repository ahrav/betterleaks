package sources

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/mholt/archives"
)

const (
	maxPeekSize     = 25 * 1_000 // 25kb
	downloadTimeout = 5 * time.Minute
)

var isWhitespace [256]bool
var isWindows = runtime.GOOS == "windows"

func init() {
	// define whitespace characters
	isWhitespace[' '] = true
	isWhitespace['\t'] = true
	isWhitespace['\n'] = true
	isWhitespace['\r'] = true
}

// isArchive does a light check to see if the provided path is an archive or
// compressed file. The File source already does this, so this exists mainly
// to avoid expensive calls before sending things to the File source
func isArchive(ctx context.Context, path string) bool {
	format, _, err := archives.Identify(ctx, path, nil)
	return err == nil && format != nil
}

// shouldSkipAttrs evaluates the skip callback against attrs.
// Returns true if the fragment should be skipped.
// If no callback is set (nil), nothing is skipped.
func shouldSkipAttrs(skip SkipFunc, attrs map[string]string) bool {
	if skip == nil {
		return false
	}
	return skip(attrs)
}

// shouldSkipPath checks a path against the skip callback.
// Also handles the Windows forward-slash path normalization workaround.
func shouldSkipPath(skip SkipFunc, path string) bool {
	if skip == nil {
		logging.Trace().Str("path", path).Msg("not skipping path because skip func is nil")
		return false
	}
	attrs := map[string]string{AttrPath: path}
	if shouldSkipAttrs(skip, attrs) {
		return true
	}
	// TODO: Remove this Windows workaround in v9 (gitleaks/gitleaks#1641).
	if isWindows {
		attrs[AttrPath] = filepath.ToSlash(path)
		return shouldSkipAttrs(skip, attrs)
	}
	return false
}

// readUntilSafeBoundary consumes r until it finds two newlines separated only
// by whitespace, up to maxPeekSize. It works on buffered spans so the common
// long-line case does not pay one ReadByte and WriteByte call per byte.
func readUntilSafeBoundary(r *bufio.Reader, n int, maxPeekSize int, peekBuf *bytes.Buffer) error {
	if peekBuf.Len() == 0 {
		return nil
	}

	data := peekBuf.Bytes()
	if isWhitespace[data[len(data)-1]] {
		newlineCount := 0
		for i := len(data) - 1; i >= 0; i-- {
			switch {
			case data[i] == '\n':
				newlineCount++
				if newlineCount >= 2 {
					return nil
				}
			case isWhitespace[data[i]]:
			default:
				i = -1
			}
		}
	}

	newlineCount := 0
	process := func(b byte) bool {
		switch {
		case b == '\n':
			newlineCount++
			return newlineCount >= 2
		case isWhitespace[b]:
			return false
		default:
			newlineCount = 0
			return false
		}
	}
	if process(data[len(data)-1]) {
		return nil
	}

	for peekBuf.Len()-n < maxPeekSize {
		if r.Buffered() == 0 {
			if _, err := r.Peek(1); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}

		remaining := maxPeekSize - (peekBuf.Len() - n)
		available := min(r.Buffered(), remaining)
		chunk, err := r.Peek(available)
		if err != nil {
			return err
		}
		consume := len(chunk)
		for i, b := range chunk {
			if process(b) {
				consume = i + 1
				break
			}
		}
		if _, err := peekBuf.Write(chunk[:consume]); err != nil {
			return err
		}
		if _, err := r.Discard(consume); err != nil {
			return err
		}
		if consume < len(chunk) || newlineCount >= 2 {
			return nil
		}
	}
	return nil
}

type sourceDownloadOptions struct {
	URL             string
	Reader          io.ReadCloser
	HTTPClient      *http.Client
	Path            string
	Attrs           map[string]string
	BearerToken     string
	MaxArchiveDepth int
	ShouldSkip      SkipFunc
	TempPattern     string
}

// downloadAndScanSource downloads content from a URL or scans an existing reader via File.
func downloadAndScanSource(ctx context.Context, opts sourceDownloadOptions, yield FragmentsFunc) error {
	start := time.Now()
	reader := opts.Reader

	if reader == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
		if err != nil {
			return err
		}
		if opts.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+opts.BearerToken)
		}
		httpClient := opts.HTTPClient
		if httpClient == nil {
			httpClient = &http.Client{
				Timeout: downloadTimeout,
			}
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("download returned %s", resp.Status)
		}
		reader = resp.Body
	}
	defer reader.Close()

	tempPattern := opts.TempPattern
	if tempPattern == "" {
		tempPattern = "betterleaks-download-*"
	}
	tmp, err := os.CreateTemp("", tempPattern)
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	if _, err := io.Copy(tmp, reader); err != nil {
		return fmt.Errorf("download %s: %w", opts.Path, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	file := &File{
		Content:         tmp,
		Path:            opts.Path,
		MaxArchiveDepth: max(1, opts.MaxArchiveDepth),
		ShouldSkip:      opts.ShouldSkip,
	}
	err = file.Fragments(ctx, func(fragment Fragment, err error) error {
		if err == nil {
			for k, v := range opts.Attrs {
				if k == AttrResource || fragment.Attr(k) == "" {
					fragment.SetAttr(k, v)
				}
			}
		}
		return yield(fragment, err)
	})
	logging.Debug().Str("path", opts.Path).Str("scan_ms", time.Since(start).Round(time.Millisecond).String()).Msg("download scan complete")
	return err
}
