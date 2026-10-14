package promotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
)

var (
	ErrIntentNotFound = errors.New("promotion: intent not found")
	ErrIntentConflict = errors.New("promotion: intent compare-and-swap conflict")
)

type Mode string

const (
	ModeCutover Mode = "cutover"
	ModeFanIn   Mode = "fan-in"
)

type State string

const (
	StateHandoffIntent     State = "HANDOFF_INTENT"
	StateDestinationFenced State = "DESTINATION_FENCED"
	StateLocalFrozen       State = "LOCAL_FROZEN"
	StateFateAllocated     State = "FATE_ALLOCATED"
	StateImportSubmitted   State = "IMPORT_SUBMITTED"
	StateImportVerified    State = "IMPORT_VERIFIED"
	StateLocalRetired      State = "LOCAL_RETIRED"
	StateAccumuloWritable  State = "ACCUMULO_WRITABLE"
)

type AuthorityToken struct {
	Domain     string `json:"domain"`
	Epoch      uint64 `json:"epoch"`
	Generation string `json:"generation"`
	Attempt    string `json:"attempt"`
}

func (t AuthorityToken) validate(name string) error {
	if t.Domain == "" || t.Epoch == 0 || t.Generation == "" || t.Attempt == "" {
		return fmt.Errorf("promotion: incomplete %s authority token", name)
	}
	return nil
}

type Request struct {
	ID                   string
	Mode                 Mode
	ParentPromotionID    string
	ProducerID           string
	SourceGeneration     uint64
	SourceAuthority      AuthorityToken
	DestinationAuthority AuthorityToken
	DestinationTable     string
	DestinationTableID   string
	BulkDir              string
	SetTime              bool
	Manifest             *engine.RFileExportManifest
}

type StagedArtifact struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Intent struct {
	Version              int                       `json:"version"`
	ID                   string                    `json:"id"`
	Revision             uint64                    `json:"revision"`
	State                State                     `json:"state"`
	Mode                 Mode                      `json:"mode"`
	ParentPromotionID    string                    `json:"parent_promotion_id,omitempty"`
	ProducerID           string                    `json:"producer_id,omitempty"`
	SourceGeneration     uint64                    `json:"source_generation"`
	SourceTable          string                    `json:"source_table"`
	ManifestSHA256       string                    `json:"manifest_sha256"`
	SourceAuthority      AuthorityToken            `json:"source_authority"`
	DestinationAuthority AuthorityToken            `json:"destination_authority"`
	DestinationTable     string                    `json:"destination_table"`
	DestinationTableID   string                    `json:"destination_table_id"`
	BulkDir              string                    `json:"bulk_dir"`
	SetTime              bool                      `json:"set_time"`
	Checkpoint           string                    `json:"checkpoint,omitempty"`
	MappingSHA256        string                    `json:"mapping_sha256,omitempty"`
	Staged               []StagedArtifact          `json:"staged,omitempty"`
	FateID               accumulo.BulkImportFateID `json:"fate_id"`
	SubmissionAttempts   uint64                    `json:"submission_attempts,omitempty"`
	FateStatus           string                    `json:"fate_status,omitempty"`
	FateFinished         bool                      `json:"fate_finished,omitempty"`
	CleanupComplete      bool                      `json:"cleanup_complete,omitempty"`
	CreatedAt            time.Time                 `json:"created_at"`
	UpdatedAt            time.Time                 `json:"updated_at"`
}

type IntentStore interface {
	Acquire(context.Context, string) (func() error, error)
	Load(context.Context, string) (*Intent, error)
	Create(context.Context, *Intent) error
	CompareAndSwap(context.Context, string, uint64, *Intent) error
}

type AuthorityController interface {
	FenceDestination(context.Context, *Intent) (AuthorityToken, string, error)
	FreezeSource(context.Context, *Intent) (string, error)
	VerifyImport(context.Context, *Intent, LoadMapping) error
	RetireSource(context.Context, *Intent) error
	ActivateDestination(context.Context, *Intent) error
	CompleteFanIn(context.Context, *Intent) error
}

type DurablePromoter interface {
	Promoter
	AllocateBulkImport(context.Context, string) (accumulo.BulkImportFateID, error)
	SubmitBulkImport(context.Context, accumulo.BulkImportFateID, string, string, string, accumulo.BulkImportOptions) error
	WaitBulkImport(context.Context, string, accumulo.BulkImportFateID) (string, error)
	FinishBulkImport(context.Context, string, accumulo.BulkImportFateID) error
}

type Machine struct {
	Store     IntentStore
	Authority AuthorityController
	Promoter  DurablePromoter
	Source    storage.Backend
	Stage     storage.Backend
	Now       func() time.Time
}

func (m *Machine) Run(ctx context.Context, req Request) (*Intent, error) {
	if m.Store == nil || m.Authority == nil || m.Promoter == nil || m.Source == nil || m.Stage == nil {
		return nil, errors.New("promotion: incomplete durable machine")
	}
	manifestHash, err := strictPromotionPreflight(req.Manifest)
	if err != nil {
		return nil, err
	}
	if err := validateRequest(req, manifestHash); err != nil {
		return nil, err
	}
	if err := validatePromotionDestination(m.Stage, req.DestinationTable, req.BulkDir); err != nil {
		return nil, err
	}
	if _, err := BuildLoadMapping(req.Manifest); err != nil {
		return nil, err
	}
	if _, err := RequiredDestinationSplits(req.Manifest); err != nil {
		return nil, err
	}
	if err := stagingPreflight(ctx, m.Source, req.Manifest, m.Stage, req.BulkDir); err != nil {
		return nil, err
	}
	id := req.ID
	if id == "" {
		id = deterministicIntentID(req, manifestHash)
	}
	release, err := m.Store.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	intent, err := m.Store.Load(ctx, id)
	if errors.Is(err, ErrIntentNotFound) {
		intent = &Intent{
			Version: 1, ID: id, Revision: 1, State: StateHandoffIntent,
			Mode: req.Mode, ParentPromotionID: req.ParentPromotionID,
			ProducerID: req.ProducerID, SourceGeneration: req.SourceGeneration,
			SourceTable: req.Manifest.SourceTable, ManifestSHA256: manifestHash,
			SourceAuthority: req.SourceAuthority, DestinationAuthority: req.DestinationAuthority,
			DestinationTable: req.DestinationTable, DestinationTableID: req.DestinationTableID,
			BulkDir: req.BulkDir, SetTime: req.SetTime, CreatedAt: now().UTC(), UpdatedAt: now().UTC(),
		}
		if err := m.Store.Create(ctx, intent); err != nil && !errors.Is(err, ErrIntentConflict) {
			return nil, err
		}
		intent, err = m.Store.Load(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	if err := intent.matches(req, manifestHash); err != nil {
		return nil, err
	}

	for steps := 0; steps < 24; steps++ {
		switch intent.State {
		case StateHandoffIntent:
			token, tableID, err := m.Authority.FenceDestination(ctx, intent)
			if err != nil {
				return intent, err
			}
			if err := token.validate("destination"); err != nil {
				return intent, err
			}
			if token.Epoch <= intent.SourceAuthority.Epoch {
				return intent, fmt.Errorf("promotion: destination epoch %d must fence source epoch %d", token.Epoch, intent.SourceAuthority.Epoch)
			}
			next := cloneIntent(intent)
			next.DestinationAuthority, next.DestinationTableID = token, tableID
			next.State = StateDestinationFenced
			intent, err = m.advance(ctx, intent, next, now)
		case StateDestinationFenced:
			checkpoint, freezeErr := m.Authority.FreezeSource(ctx, intent)
			if freezeErr != nil {
				return intent, freezeErr
			}
			if checkpoint == "" {
				return intent, errors.New("promotion: source freeze returned empty checkpoint")
			}
			next := cloneIntent(intent)
			next.Checkpoint, next.State = checkpoint, StateLocalFrozen
			intent, err = m.advance(ctx, intent, next, now)
		case StateLocalFrozen:
			if intent.MappingSHA256 == "" {
				mapping, artifacts, mappingHash, stageErr := m.stage(ctx, intent, req.Manifest)
				if stageErr != nil {
					return intent, stageErr
				}
				next := cloneIntent(intent)
				next.MappingSHA256, next.Staged = mappingHash, artifacts
				intent, err = m.advance(ctx, intent, next, now)
				if err == nil && len(mapping) == 0 {
					next = cloneIntent(intent)
					next.State = StateImportVerified
					intent, err = m.advance(ctx, intent, next, now)
				}
				break
			}
			id, allocErr := m.Promoter.AllocateBulkImport(ctx, intent.DestinationTable)
			if allocErr != nil {
				return intent, allocErr
			}
			next := cloneIntent(intent)
			next.FateID, next.State = id, StateFateAllocated
			intent, err = m.advance(ctx, intent, next, now)
		case StateFateAllocated:
			next := cloneIntent(intent)
			next.SubmissionAttempts++
			intent, err = m.advance(ctx, intent, next, now)
			if err != nil {
				break
			}
			if submitErr := m.Promoter.SubmitBulkImport(ctx, intent.FateID, intent.DestinationTable, intent.DestinationTableID, intent.BulkDir, accumulo.BulkImportOptions{SetTime: intent.SetTime}); submitErr != nil {
				return intent, submitErr
			}
			next = cloneIntent(intent)
			next.State = StateImportSubmitted
			intent, err = m.advance(ctx, intent, next, now)
		case StateImportSubmitted:
			status, waitErr := m.Promoter.WaitBulkImport(ctx, intent.DestinationTable, intent.FateID)
			if waitErr != nil {
				return intent, waitErr
			}
			mapping, mapErr := BuildLoadMapping(req.Manifest)
			if mapErr != nil {
				return intent, mapErr
			}
			if verifyErr := m.Authority.VerifyImport(ctx, intent, mapping); verifyErr != nil {
				return intent, verifyErr
			}
			next := cloneIntent(intent)
			next.FateStatus, next.State = status, StateImportVerified
			intent, err = m.advance(ctx, intent, next, now)
		case StateImportVerified:
			if intent.FateID.UUID != "" && !intent.FateFinished {
				if finishErr := m.Promoter.FinishBulkImport(ctx, intent.DestinationTable, intent.FateID); finishErr != nil {
					return intent, finishErr
				}
				next := cloneIntent(intent)
				next.FateFinished = true
				intent, err = m.advance(ctx, intent, next, now)
				break
			}
			if intent.Mode == ModeFanIn {
				if completeErr := m.Authority.CompleteFanIn(ctx, intent); completeErr != nil {
					return intent, completeErr
				}
				next := cloneIntent(intent)
				next.State = StateAccumuloWritable
				intent, err = m.advance(ctx, intent, next, now)
				break
			}
			if retireErr := m.Authority.RetireSource(ctx, intent); retireErr != nil {
				return intent, retireErr
			}
			next := cloneIntent(intent)
			next.State = StateLocalRetired
			intent, err = m.advance(ctx, intent, next, now)
		case StateLocalRetired:
			if activateErr := m.Authority.ActivateDestination(ctx, intent); activateErr != nil {
				return intent, activateErr
			}
			next := cloneIntent(intent)
			next.State = StateAccumuloWritable
			intent, err = m.advance(ctx, intent, next, now)
		case StateAccumuloWritable:
			if !intent.CleanupComplete {
				if cleanupErr := cleanupOwned(ctx, m.Stage, intent); cleanupErr != nil {
					return intent, cleanupErr
				}
				next := cloneIntent(intent)
				next.CleanupComplete = true
				intent, err = m.advance(ctx, intent, next, now)
				break
			}
			return intent, nil
		default:
			return intent, fmt.Errorf("promotion: unknown durable state %q", intent.State)
		}
		if err != nil {
			if !errors.Is(err, ErrIntentConflict) {
				return intent, err
			}
			intent, err = m.Store.Load(ctx, id)
			if err != nil {
				return nil, err
			}
		}
	}
	return intent, errors.New("promotion: state machine exceeded transition limit")
}

func (m *Machine) advance(ctx context.Context, current, next *Intent, now func() time.Time) (*Intent, error) {
	next.Revision = current.Revision + 1
	next.UpdatedAt = now().UTC()
	if err := m.Store.CompareAndSwap(ctx, current.ID, current.Revision, next); err != nil {
		return current, err
	}
	return next, nil
}

func (m *Machine) stage(ctx context.Context, intent *Intent, manifest *engine.RFileExportManifest) (LoadMapping, []StagedArtifact, string, error) {
	if _, err := BuildLoadMapping(manifest); err != nil {
		return nil, nil, "", err
	}
	if err := stagingPreflight(ctx, m.Source, manifest, m.Stage, intent.BulkDir); err != nil {
		return nil, nil, "", err
	}
	splits, err := RequiredDestinationSplits(manifest)
	if err != nil {
		return nil, nil, "", err
	}
	if len(splits) > 0 {
		if err := m.Promoter.AddTableSplitsForTable(ctx, accumulo.Table{Name: intent.DestinationTable, ID: intent.DestinationTableID}, splits); err != nil {
			return nil, nil, "", err
		}
		if err := verifyNoUnexpectedDestinationSplits(ctx, m.Promoter, intent.DestinationTable, splits); err != nil {
			return nil, nil, "", err
		}
	}
	if err := verifyDestinationTableIdentity(ctx, m.Promoter, intent.DestinationTable, intent.DestinationTableID); err != nil {
		return nil, nil, "", err
	}
	release, err := acquireStageBulkDir(ctx, m.Stage, intent.BulkDir)
	if err != nil {
		return nil, nil, "", err
	}
	defer release()
	mapping, err := stageBulkDirLocked(ctx, m.Source, manifest, m.Stage, intent.BulkDir)
	if err != nil {
		return nil, nil, "", err
	}
	loadmapPath := joinBulkPath(m.Stage, intent.BulkDir, "loadmap.json")
	mappingBytes, err := storage.ReadAll(ctx, m.Stage, loadmapPath)
	if err != nil {
		return nil, nil, "", err
	}
	artifacts := make([]StagedArtifact, 0, len(manifest.RFiles)+1)
	for _, rf := range manifest.RFiles {
		stagedPath := joinBulkPath(m.Stage, intent.BulkDir, path.Base(strings.ReplaceAll(rf.DestinationPath, `\`, `/`)))
		data, readErr := storage.ReadAll(ctx, m.Stage, stagedPath)
		if readErr != nil {
			return nil, nil, "", readErr
		}
		fileSum := sha256.Sum256(data)
		artifacts = append(artifacts, StagedArtifact{Path: stagedPath, Size: int64(len(data)), SHA256: hex.EncodeToString(fileSum[:])})
	}
	sum := sha256.Sum256(mappingBytes)
	artifacts = append(artifacts, StagedArtifact{
		Path: loadmapPath,
		Size: int64(len(mappingBytes)), SHA256: hex.EncodeToString(sum[:]),
	})
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return mapping, artifacts, hex.EncodeToString(sum[:]), nil
}

func strictPromotionPreflight(manifest *engine.RFileExportManifest) (string, error) {
	if manifest == nil {
		return "", errors.New("promotion: nil manifest")
	}
	for _, artifact := range manifest.RFiles {
		format := artifact.Format
		if format == "" {
			format = engine.ExportFormatRFile
		}
		role := artifact.Role
		if role == "" {
			role = engine.ExportRoleAuthoritative
		}
		if format != engine.ExportFormatRFile || role != engine.ExportRoleAuthoritative ||
			!strings.EqualFold(path.Ext(strings.ReplaceAll(artifact.DestinationPath, `\`, `/`)), ".rf") {
			return "", fmt.Errorf("promotion: artifact %q is %s/%s; Accumulo promotion accepts authoritative RFile artifacts only", artifact.DestinationPath, format, role)
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("promotion: encode source lineage: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateRequest(req Request, manifestHash string) error {
	if req.Mode != ModeCutover && req.Mode != ModeFanIn {
		return fmt.Errorf("promotion: invalid mode %q", req.Mode)
	}
	if err := req.SourceAuthority.validate("source"); err != nil {
		return err
	}
	if req.DestinationTable == "" || req.SourceGeneration == 0 {
		return errors.New("promotion: destination table and source generation are required")
	}
	if req.Mode == ModeFanIn && req.ParentPromotionID == "" {
		return errors.New("promotion: fan-in requires a parent cutover promotion")
	}
	id := req.ID
	expectedID := deterministicIntentID(req, manifestHash)
	if id == "" {
		id = expectedID
	} else if id != expectedID {
		return fmt.Errorf("promotion: identity %q does not match immutable inputs (want %q)", id, expectedID)
	}
	clean := strings.TrimRight(strings.ReplaceAll(req.BulkDir, `\`, `/`), "/")
	if path.Base(clean) != id {
		return fmt.Errorf("promotion: bulk directory must end with promotion identity %q", id)
	}
	return nil
}

func deterministicIntentID(req Request, manifestHash string) string {
	raw := strings.Join([]string{
		string(req.Mode), req.ParentPromotionID, req.ProducerID,
		fmt.Sprint(req.SourceGeneration), req.Manifest.SourceTable, manifestHash,
		req.SourceAuthority.Domain, fmt.Sprint(req.SourceAuthority.Epoch),
		req.SourceAuthority.Generation, req.DestinationTable,
	}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

func (i *Intent) matches(req Request, manifestHash string) error {
	if i.ManifestSHA256 != manifestHash || i.Mode != req.Mode ||
		i.ParentPromotionID != req.ParentPromotionID || i.ProducerID != req.ProducerID ||
		i.SourceGeneration != req.SourceGeneration || i.SourceTable != req.Manifest.SourceTable ||
		i.SourceAuthority != req.SourceAuthority || i.DestinationTable != req.DestinationTable ||
		i.BulkDir != req.BulkDir || i.SetTime != req.SetTime {
		return fmt.Errorf("promotion: identity %q already belongs to different immutable inputs", i.ID)
	}
	return nil
}

func cloneIntent(in *Intent) *Intent {
	out := *in
	out.Staged = append([]StagedArtifact(nil), in.Staged...)
	return &out
}

func cleanupOwned(ctx context.Context, backend storage.Backend, intent *Intent) error {
	remover, ok := backend.(storage.Remover)
	if !ok {
		return nil
	}
	for _, artifact := range intent.Staged {
		data, err := storage.ReadAll(ctx, backend, artifact.Path)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != artifact.Size || hex.EncodeToString(sum[:]) != artifact.SHA256 {
			continue
		}
		if err := remover.Remove(ctx, artifact.Path); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
}

type MemoryIntentStore struct {
	mu      sync.Mutex
	intents map[string]*Intent
	locks   map[string]*sync.Mutex
}

func NewMemoryIntentStore() *MemoryIntentStore {
	return &MemoryIntentStore{intents: map[string]*Intent{}, locks: map[string]*sync.Mutex{}}
}

func (s *MemoryIntentStore) Acquire(ctx context.Context, id string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	lock := s.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[id] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	return func() error { lock.Unlock(); return nil }, nil
}

func (s *MemoryIntentStore) Load(_ context.Context, id string) (*Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, ok := s.intents[id]
	if !ok {
		return nil, ErrIntentNotFound
	}
	return cloneIntent(intent), nil
}

func (s *MemoryIntentStore) Create(_ context.Context, intent *Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.intents[intent.ID]; ok {
		return ErrIntentConflict
	}
	s.intents[intent.ID] = cloneIntent(intent)
	return nil
}

func (s *MemoryIntentStore) CompareAndSwap(_ context.Context, id string, revision uint64, next *Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.intents[id]
	if !ok {
		return ErrIntentNotFound
	}
	if current.Revision != revision {
		return ErrIntentConflict
	}
	s.intents[id] = cloneIntent(next)
	return nil
}
