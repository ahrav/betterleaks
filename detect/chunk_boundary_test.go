package detect

import (
	"bytes"
	"context"
	"testing"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/sources"
)

func TestFileChunkBoundaryPreservesCrossBoundaryRule(t *testing.T) {
	cfg, err := config.ParseTOMLString(`
title = "chunk boundary test"

[[rules]]
id = "cross-boundary"
description = "cross-boundary"
regex = '''BEGIN(?s:.*?)END'''
keywords = ["BEGIN"]
`, "")
	if err != nil {
		t.Fatal(err)
	}

	content := append(bytes.Repeat([]byte{'x'}, 99_990), []byte("BEGIN\n")...)
	content = append(content, bytes.Repeat([]byte{'A'}, 8_000)...)
	content = append(content, []byte("\nEND\n\n")...)

	detector := NewDetector(cfg)
	source := &sources.File{Content: bytes.NewReader(content), Path: "fixture.txt"}
	count := 0
	for result := range detector.Run(context.Background(), source) {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("finding count = %d, want 1", count)
	}
}
