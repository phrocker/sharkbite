// Package embeddinginventory produces a read-only operator view of current
// Accumulo file entries and their verified sidecar state.
package embeddinginventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingsidecar"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
)

// TabletReader reads authoritative tablet file entries. The second read in
// Collect detects a replacement or split during sidecar verification.
type TabletReader interface {
	LocateTable(context.Context, string) ([]metadata.TabletInfo, error)
}

type FooterReader interface {
	ReadFooter(context.Context, string) (string, embeddingspace.FileState, error)
}

type Resolver interface {
	Resolve(context.Context, metadata.TabletInfo, embeddingsidecar.Identity,
		string, embeddingspace.FileState) (embeddingspace.FileState, error)
}

type Config struct {
	TableID  string
	Tablets  TabletReader
	Footers  FooterReader
	Sidecars Resolver
	Now      func() time.Time
}

type File struct {
	Path          string                   `json:"path"`
	Entry         string                   `json:"entry"`
	State         embeddingspace.FileState `json:"state"`
	UnknownReason string                   `json:"unknown_reason,omitempty"`
	DryRun        string                   `json:"dry_run"`
	// FooterHint is deliberately separate from State. It must never be
	// presented as a verified sidecar result.
	FooterHint embeddingspace.FileState `json:"footer_hint"`
	// Extent is operator context, not an authorization claim.
	Extent string `json:"extent"`
}

type Counts struct {
	HasEmbeddings int `json:"has_embeddings"`
	NoEmbeddings  int `json:"no_embeddings"`
	Unknown       int `json:"unknown"`
}

type Report struct {
	TableID           string    `json:"table_id"`
	At                time.Time `json:"at"`
	Demo              bool      `json:"demo"`
	Counts            Counts    `json:"counts"`
	Files             []File    `json:"files"`
	ApplyAvailable    bool      `json:"apply_available"`
	ApplyPrerequisite string    `json:"apply_prerequisite"`
}

// Collect reads a table twice. Known states come only from the sidecar's
// verified resolver; neither native metadata nor a footer hint promotes one.
// A file changed between reads is reported unknown even if its first read
// resolved. No read-only snapshot can promise that a file remains current
// after the response; each refresh checks again.
func Collect(ctx context.Context, cfg Config) (Report, error) {
	if cfg.TableID == "" || cfg.Tablets == nil || cfg.Footers == nil || cfg.Sidecars == nil {
		return Report{}, errors.New("embedding inventory: table, tablets, footers and sidecars are required")
	}
	if cfg.TableID == metadata.RootTableID {
		return Report{}, errors.New("embedding inventory: root tablet backfill is unsupported")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	first, err := cfg.Tablets.LocateTable(ctx, cfg.TableID)
	if err != nil {
		return Report{}, fmt.Errorf("embedding inventory: locate table: %w", err)
	}
	if len(first) == 0 {
		return Report{}, fmt.Errorf("embedding inventory: table %q has no tablets", cfg.TableID)
	}
	report := Report{
		TableID: cfg.TableID, At: cfg.Now().UTC(), Files: []File{},
		ApplyAvailable:    false,
		ApplyPrerequisite: "Applying changes requires verified publication for each live file and a complete source scan; use these findings to plan that work.",
	}
	// Keep the byte-exact identity for the post-verification metadata read.
	type observed struct {
		extent ingestrouter.Extent
		path   string
		qual   []byte
		value  []byte
	}
	var identities []observed
	for _, tablet := range first {
		if tablet.TableID != cfg.TableID || !tablet.PrevRowSet {
			return Report{}, errors.New("embedding inventory: incomplete tablet metadata")
		}
		extent := ingestrouter.Extent{
			TableID:    tablet.TableID,
			PrevEndRow: bytes.Clone(tablet.PrevRow), EndRow: bytes.Clone(tablet.EndRow),
		}
		for _, file := range tablet.Files {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			item := File{
				Path: file.Path, Entry: string(file.RawQualifier),
				State: embeddingspace.Unknown(), FooterHint: embeddingspace.Unknown(),
				Extent: fmt.Sprintf("%q .. %q", tablet.PrevRow, tablet.EndRow),
			}
			identities = append(identities, observed{
				extent: extent, path: file.Path,
				qual: bytes.Clone(file.RawQualifier), value: bytes.Clone(file.RawValue),
			})
			digest, footer, readErr := cfg.Footers.ReadFooter(ctx, file.Path)
			if readErr != nil {
				if err := ctx.Err(); err != nil {
					return Report{}, err
				}
				item.UnknownReason = "The file footer could not be verified: " + readErr.Error()
				item.DryRun = "Restore access to a stable file snapshot, then refresh."
			} else {
				item.FooterHint = footer
				item.State, err = cfg.Sidecars.Resolve(ctx, tablet, embeddingsidecar.Identity{
					Extent: extent, FilePath: file.Path,
					Qualifier: bytes.Clone(file.RawQualifier), Value: bytes.Clone(file.RawValue),
					FooterSHA256: digest,
				}, digest, footer)
				if err != nil {
					if contextErr := ctx.Err(); contextErr != nil {
						return Report{}, contextErr
					}
					item.State = embeddingspace.Unknown()
					item.UnknownReason = "Sidecar verification is unavailable: " + err.Error()
				} else if !item.State.Known() {
					item.State = embeddingspace.Unknown()
					item.UnknownReason = "No matching, authenticated sidecar publication could be verified for this live file."
				}
				item.DryRun = dryRunFinding(item)
			}
			report.Files = append(report.Files, item)
		}
	}
	second, err := cfg.Tablets.LocateTable(ctx, cfg.TableID)
	if err != nil {
		return Report{}, fmt.Errorf("embedding inventory: recheck table: %w", err)
	}
	for i, identity := range identities {
		if !stillLive(second, identity.extent, identity.path, identity.qual, identity.value) {
			report.Files[i].State = embeddingspace.Unknown()
			report.Files[i].UnknownReason = "The file entry or tablet extent changed during verification."
			report.Files[i].DryRun = "Refresh to inspect the replacement file."
		}
		count(&report.Counts, report.Files[i].State)
	}
	sort.Slice(report.Files, func(i, j int) bool {
		if report.Files[i].Extent != report.Files[j].Extent {
			return report.Files[i].Extent < report.Files[j].Extent
		}
		return report.Files[i].Entry < report.Files[j].Entry
	})
	return report, nil
}

func stillLive(tablets []metadata.TabletInfo, extent ingestrouter.Extent,
	path string, qualifier, value []byte) bool {
	for _, tablet := range tablets {
		if tablet.TableID != extent.TableID || !tablet.PrevRowSet ||
			!sameBytes(tablet.PrevRow, extent.PrevEndRow) ||
			!sameBytes(tablet.EndRow, extent.EndRow) {
			continue
		}
		for _, file := range tablet.Files {
			if file.Path == path && bytes.Equal(file.RawQualifier, qualifier) &&
				bytes.Equal(file.RawValue, value) {
				return true
			}
		}
	}
	return false
}

func sameBytes(a, b []byte) bool { return (a == nil) == (b == nil) && bytes.Equal(a, b) }

func count(c *Counts, state embeddingspace.FileState) {
	switch state.State {
	case embeddingspace.StateHasEmbeddings:
		c.HasEmbeddings++
	case embeddingspace.StateNoEmbeddings:
		c.NoEmbeddings++
	default:
		c.Unknown++
	}
}

func dryRunFinding(file File) string {
	if file.State.Known() {
		return "Verified; no backfill needed."
	}
	switch file.FooterHint.State {
	case embeddingspace.StateHasEmbeddings:
		return "The footer suggests embeddings, but the sidecar is unverified. Verify ownership and publication before backfill."
	case embeddingspace.StateNoEmbeddings:
		return "A no-embeddings footer is not proof. A complete scan and operator approval are required."
	default:
		return "No known state in the footer. Inspect or rescan the source file before backfill."
	}
}
