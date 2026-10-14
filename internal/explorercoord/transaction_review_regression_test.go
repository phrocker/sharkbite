// Independent regression cases reproduced against transaction runtime 3600823.
package explorercoord

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/localwal"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func TestReviewRejectsUnsyncedRuntime(t *testing.T) {
	for _, mode := range []localwal.SyncMode{localwal.SyncNormal, localwal.SyncOff} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			config := testRuntimeConfig(t, testDirectory(t))
			config.EngineOptions.WALSyncMode = mode
			runtime, err := Open(config)
			if err == nil {
				defer runtime.Close()
				t.Fatalf("runtime accepted unsynced WAL mode %d", mode)
			}
		})
	}
}

func TestReviewCompletedHistoryDoesNotBlockStartup(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	var epoch coordination.Epoch
	var digest coordination.Digest
	for i := 0; i < 4; i++ {
		mode := guard.ModeMutate
		if i == 0 {
			mode = guard.ModeAbsentOrIdentical
		}
		intent := testIntent(t, config.Domain, "history", fmt.Sprint(i), fmt.Sprint(i), mode, epoch, digest)
		result, err := runtime.Publish(context.Background(), Request{Intent: intent})
		if err != nil {
			_ = runtime.Close()
			t.Fatal(err)
		}
		epoch, digest = result.Epoch, result.LogicalDigest
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	config.RecoveryLimit = 1
	config.RecoveryMaxPages = 2
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("completed history blocked startup with no unfinished work: %v", err)
	}
	defer reopened.Close()
}

func TestReviewDocumentRetryAfterDurableIntent(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	fired := false
	config.testStageHook = func(stage recoveryStage) error {
		if stage == recoveryStageIntent && !fired {
			fired = true
			return context.Canceled
		}
		return nil
	}
	embedded, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer embedded.Close()
	source := explorer.Source{
		URI: "file:///retry.md", Title: "Retry",
		MediaType: explorer.MediaTypeMarkdown, Content: "# Retry\n\nBody.\n",
	}
	options := explorer.IngestOptions{
		CreatedAt: time.Date(2026, time.September, 5, 18, 0, 0, 0, time.UTC),
	}
	if _, err := embedded.Explorer.IngestWithOptions(context.Background(), source, options); err == nil || !fired {
		t.Fatalf("expected injected post-intent error, fired=%v err=%v", fired, err)
	}
	if _, err := embedded.Explorer.IngestWithOptions(context.Background(), source, options); err != nil {
		t.Fatalf("identical document retry failed: %v", err)
	}
}
