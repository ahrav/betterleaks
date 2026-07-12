package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"sync"
)

const maxCapturedOutputBytes = 4 << 20

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func commandEnvironment(inherit bool, base, candidate, extra map[string]string) ([]string, map[string]string, string) {
	values := make(map[string]string)
	if inherit {
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				values[key] = value
			}
		}
	}
	overrides := make(map[string]string, len(base)+len(candidate)+len(extra))
	for _, source := range []map[string]string{base, candidate, extra} {
		for key, value := range source {
			values[key] = value
			overrides[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment, overrides, sha256Hex([]byte(strings.Join(environment, "\x00")))
}

type boundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		keep := min(remaining, len(data))
		buffer.data = append(buffer.data, data[:keep]...)
	}
	if len(data) > remaining {
		buffer.truncated = true
	}
	return len(data), nil
}

func (buffer *boundedBuffer) snapshot() (string, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.data), buffer.truncated
}
