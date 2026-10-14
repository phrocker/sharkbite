package compactjob

import (
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

func TestOptionsFromTableProperties(t *testing.T) {
	opts, err := OptionsFromTableProperties(map[string]string{
		"table.file.type":               "rf",
		"table.file.compress.type":      "gz",
		"table.file.compress.blocksize": "128K",
		"table.bloom.enabled":           "false",
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultCodec != block.CodecGzip || opts.DefaultBlockSize != 128<<10 {
		t.Fatalf("options = %+v", opts)
	}
}

func TestOptionsFromTablePropertiesFailsClosed(t *testing.T) {
	for name, properties := range map[string]map[string]string{
		"file type":      {"table.file.type": "parquet"},
		"codec":          {"table.file.compress.type": "zstd"},
		"crypto":         {"table.crypto.service": "example.Crypto"},
		"locality group": {"table.groups.enabled": "docs"},
		"sampler":        {"table.sampler": "example.Sampler"},
		"summarizer":     {"table.summarizer.count": "example.Summarizer"},
		"bloom":          {"table.bloom.enabled": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := OptionsFromTableProperties(properties, Limits{})
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("error = %v, want refusal", err)
			}
		})
	}
}
