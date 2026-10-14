// Package metadatacas implements Accumulo-authoritative tablet metadata
// mutations using conditional ingest RPCs and ZooKeeper version CAS for root.
package metadatacas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strconv"
	"sync/atomic"

	gozk "github.com/go-zookeeper/zk"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/hostedingest"
	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/mincauthority"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/walauthority"
	"github.com/phrocker/shoal-oss/internal/zk"
)

var (
	ErrInvalidConfig = errors.New("metadatacas: invalid configuration")
	ErrStaleOwner    = errors.New("metadatacas: stale tablet owner")
	ErrRejected      = errors.New("metadatacas: conditional mutation rejected")
	ErrInconsistent  = errors.New("metadatacas: inconsistent authoritative metadata")
)

const rootTabletPath = "root_tablet"

type TabletReader interface {
	LocateTable(context.Context, string) ([]metadata.TabletInfo, error)
}

type RootLocator interface {
	RootTabletLocation(context.Context) (*zk.Location, error)
}

type ConditionalWriter interface {
	ConditionalWrite(context.Context, string, string, *data.TKeyExtent, *data.TConditionalMutation) (ingestclient.ConditionalStatus, error)
}

type RootStore interface {
	Get(string) ([]byte, *gozk.Stat, error)
	Set(string, []byte, int32) (*gozk.Stat, error)
}

type Config struct {
	Reader       TabletReader
	RootLocator  RootLocator
	Conditional  ConditionalWriter
	RootStore    RootStore
	Host         *tserver.Host
	InstancePath string
	Address      string
	Group        string
	Session      string
}

type Factory struct {
	cfg  Config
	next atomic.Int64
}

func (*Factory) SupportsSystemTablets() bool { return true }

func NewFactory(cfg Config) (*Factory, error) {
	if cfg.Reader == nil || cfg.RootLocator == nil || cfg.Conditional == nil ||
		cfg.RootStore == nil || cfg.Host == nil || cfg.InstancePath == "" ||
		cfg.Address == "" || cfg.Session == "" {
		return nil, ErrInvalidConfig
	}
	if cfg.Group == "" {
		cfg.Group = tserver.DefaultResourceGroup
	}
	f := &Factory{cfg: cfg}
	f.next.Store(1)
	return f, nil
}

func (f *Factory) Open(
	ctx context.Context,
	spec tabletloader.Specification,
	fence ingestrouter.Fence,
) (hostedingest.MetadataAuthority, error) {
	if spec.Generation != tabletloader.Generation(f.cfg.Session) ||
		fence.ServerGeneration != f.cfg.Session {
		return nil, ErrStaleOwner
	}
	authority := &Authority{
		factory: f,
		extent: ingestrouter.Extent{
			TableID:    spec.Extent.TableID,
			PrevEndRow: append([]byte(nil), spec.Extent.PrevEndRow...),
			EndRow:     append([]byte(nil), spec.Extent.EndRow...),
		},
		fence: fence,
	}
	if err := authority.claim(ctx); err != nil {
		return nil, err
	}
	return authority, nil
}

type Authority struct {
	factory *Factory
	extent  ingestrouter.Extent
	fence   ingestrouter.Fence
}

type condition struct {
	cf, cq, value []byte
}

type update struct {
	cf, cq, value []byte
	delete        bool
}

func (a *Authority) EnsureReference(
	ctx context.Context,
	extent ingestrouter.Extent,
	fence ingestrouter.Fence,
	ref walauthority.Reference,
) error {
	if err := a.validateIdentity(extent, fence); err != nil {
		return err
	}
	if err := validateReference(ref); err != nil {
		return err
	}
	status, err := a.mutate(ctx, a.ownerConditions(), []update{{
		cf: []byte(metadata.CFLog), cq: []byte(ref.Qualifier), value: []byte{},
	}})
	if status == ingestclient.ConditionalAccepted {
		return nil
	}
	present, readErr := a.HasReference(context.Background(), extent, fence, ref)
	if readErr == nil && present {
		return nil
	}
	if status == ingestclient.ConditionalRejected {
		return errors.Join(ErrRejected, err, readErr)
	}
	return errors.Join(err, readErr)
}

func (a *Authority) HasReference(
	ctx context.Context,
	extent ingestrouter.Extent,
	fence ingestrouter.Fence,
	ref walauthority.Reference,
) (bool, error) {
	if err := a.validateIdentity(extent, fence); err != nil {
		return false, err
	}
	info, err := a.readOwned(ctx)
	if err != nil {
		return false, err
	}
	for _, entry := range info.Logs {
		if string(entry.RawQualifier) == ref.Qualifier && entry.Path == ref.Path && entry.UUID == ref.ID {
			return true, nil
		}
	}
	return false, nil
}

func (a *Authority) RemoveReference(
	ctx context.Context,
	extent ingestrouter.Extent,
	fence ingestrouter.Fence,
	ref walauthority.Reference,
) error {
	if err := a.validateIdentity(extent, fence); err != nil {
		return err
	}
	if err := validateReference(ref); err != nil {
		return err
	}
	conditions := append(a.ownerConditions(), condition{
		cf: []byte(metadata.CFLog), cq: []byte(ref.Qualifier), value: []byte{},
	})
	status, err := a.mutate(ctx, conditions, []update{{
		cf: []byte(metadata.CFLog), cq: []byte(ref.Qualifier), delete: true,
	}})
	if status == ingestclient.ConditionalAccepted {
		return nil
	}
	present, readErr := a.HasReference(context.Background(), extent, fence, ref)
	if readErr == nil && !present {
		return nil
	}
	if status == ingestclient.ConditionalRejected {
		return errors.Join(ErrRejected, err, readErr)
	}
	return errors.Join(err, readErr)
}

// Release atomically removes this exact current location and lock generation.
// A lost response is reconciled by rereading the row; another owner's
// location is never treated as this release succeeding.
func (a *Authority) Release(
	ctx context.Context,
	extent ingestrouter.Extent,
	fence ingestrouter.Fence,
) error {
	if err := a.validateIdentity(extent, fence); err != nil {
		return err
	}
	status, err := a.mutate(ctx, a.ownerConditions(), []update{
		{cf: []byte(metadata.CFCurrentLocation), cq: []byte(a.factory.cfg.Session), delete: true},
		{cf: []byte(metadata.CFServer), cq: []byte(metadata.CQLock), delete: true},
	})
	if status == ingestclient.ConditionalAccepted {
		return nil
	}
	info, readErr := a.read(context.Background())
	if readErr == nil && info.Location == nil && info.FutureLocation == nil {
		return nil
	}
	if status == ingestclient.ConditionalRejected {
		return errors.Join(ErrRejected, err, readErr)
	}
	return errors.Join(err, readErr)
}

func (a *Authority) References(
	ctx context.Context,
	extent ingestrouter.Extent,
) ([]walauthority.Reference, error) {
	if !extent.Equal(a.extent) {
		return nil, ErrStaleOwner
	}
	info, err := a.readOwned(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]walauthority.Reference, 0, len(info.Logs))
	for _, entry := range info.Logs {
		refs = append(refs, walauthority.Reference{
			ID: entry.UUID, Path: entry.Path, Qualifier: string(entry.RawQualifier),
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Qualifier < refs[j].Qualifier })
	return refs, nil
}

func (a *Authority) Commit(
	ctx context.Context,
	request mincauthority.MetadataCommit,
) (mincauthority.CommitOutcome, error) {
	if err := a.validateIdentity(request.Extent, request.Fence); err != nil {
		return mincauthority.CommitRejected, err
	}

	fileQualifier, fileValue, err := encodeFile(request.File)
	if err != nil {
		return mincauthority.CommitRejected, err
	}
	conditions := a.ownerConditions()
	updates := []update{{
		cf: []byte(metadata.CFFile), cq: fileQualifier, value: fileValue,
	}}
	fileEmbedding, err := encodeFileEmbedding(request.File.Embedding)
	if err != nil {
		return mincauthority.CommitRejected, err
	}
	updates = append(updates, update{
		cf: []byte(metadata.CFFileEmbedding), cq: fileQualifier, value: fileEmbedding,
	})
	if request.TabletTime != "" {
		info, readErr := a.readOwned(ctx)
		if readErr != nil {
			return mincauthority.CommitUnknown, readErr
		}
		if err := validateTabletTimeAdvance(info.Time, request.TabletTime); err != nil {
			return mincauthority.CommitRejected, err
		}
		var current []byte
		if info.Time != "" {
			current = []byte(info.Time)
		}
		conditions = append(conditions, condition{
			cf: []byte(metadata.CFServer), cq: []byte(metadata.CQTime), value: current,
		})
		updates = append(updates, update{
			cf: []byte(metadata.CFServer), cq: []byte(metadata.CQTime),
			value: []byte(request.TabletTime),
		})
	}
	for _, ref := range request.RemoveWALs {
		if err := validateReference(ref); err != nil {
			return mincauthority.CommitRejected, err
		}
		conditions = append(conditions, condition{
			cf: []byte(metadata.CFLog), cq: []byte(ref.Qualifier), value: []byte{},
		})
		updates = append(updates, update{
			cf: []byte(metadata.CFLog), cq: []byte(ref.Qualifier), delete: true,
		})
	}
	status, commitErr := a.mutate(ctx, conditions, updates)
	switch status {
	case ingestclient.ConditionalAccepted:
		return mincauthority.CommitApplied, commitErr
	}
	applied, readErr := a.commitApplied(context.Background(), request)
	if readErr == nil && applied {
		return mincauthority.CommitApplied, commitErr
	}
	if status == ingestclient.ConditionalRejected {
		return mincauthority.CommitRejected, errors.Join(commitErr, readErr)
	}
	return mincauthority.CommitUnknown, errors.Join(commitErr, readErr)
}

func (a *Authority) UpdateFlushID(ctx context.Context, flushID int64) error {
	if flushID < 0 {
		return ErrInvalidConfig
	}
	status, err := a.mutate(ctx, a.ownerConditions(), []update{{
		cf: []byte(metadata.CFServer), cq: []byte(metadata.CQFlushID),
		value: []byte(strconv.FormatInt(flushID, 10)),
	}})
	if status == ingestclient.ConditionalAccepted {
		return nil
	}
	if status == ingestclient.ConditionalRejected {
		return errors.Join(ErrRejected, err)
	}
	return err
}

func (a *Authority) commitApplied(
	ctx context.Context,
	request mincauthority.MetadataCommit,
) (bool, error) {
	state, err := a.Read(ctx, a.extent)
	if err != nil {
		return false, err
	}
	found := false
	for _, current := range state.Files {
		if current.Path == request.File.Path {
			if current.Size != request.File.Size || current.Entries != request.File.Entries ||
				normalizedEmbedding(current.Embedding) != normalizedEmbedding(request.File.Embedding) {
				return false, ErrInconsistent
			}
			found = true
		}
	}
	if !found {
		return false, nil
	}
	if request.TabletTime != "" && state.Time != request.TabletTime {
		return false, nil
	}
	for _, ref := range request.RemoveWALs {
		for _, current := range state.WALs {
			if current == ref {
				return false, nil
			}
		}
	}
	return true, nil
}

func (a *Authority) Read(
	ctx context.Context,
	extent ingestrouter.Extent,
) (mincauthority.MetadataState, error) {
	if !extent.Equal(a.extent) {
		return mincauthority.MetadataState{}, ErrStaleOwner
	}
	info, err := a.readOwned(ctx)
	if err != nil {
		return mincauthority.MetadataState{}, err
	}
	state := mincauthority.MetadataState{
		Files: make([]mincauthority.DataFile, 0, len(info.Files)),
		WALs:  make([]walauthority.Reference, 0, len(info.Logs)),
		Time:  info.Time,
	}
	for _, file := range info.Files {
		state.Files = append(state.Files, mincauthority.DataFile{
			Path: file.Path, Format: "rfile", Size: file.Size, Entries: file.NumEntries,
			StartRow: []byte(file.StartRow), EndRow: []byte(file.EndRow),
			Embedding: file.Embedding,
		})
	}
	for _, entry := range info.Logs {
		state.WALs = append(state.WALs, walauthority.Reference{
			ID: entry.UUID, Path: entry.Path, Qualifier: string(entry.RawQualifier),
		})
	}
	return state, nil
}

func (a *Authority) claim(ctx context.Context) error {
	info, err := a.read(ctx)
	if err != nil {
		return err
	}
	if ownerMatches(info, a.factory.cfg.Address, a.factory.cfg.Session, a.lockID()) {
		return nil
	}
	if info.FutureLocation == nil ||
		info.FutureLocation.HostPort != a.factory.cfg.Address ||
		info.FutureLocation.Session != a.factory.cfg.Session ||
		info.Location != nil {
		return ErrStaleOwner
	}
	conditions := []condition{
		{cf: []byte(metadata.CFFutureLocation), cq: []byte(a.factory.cfg.Session), value: []byte(a.factory.cfg.Address)},
		{cf: []byte(metadata.CFCurrentLocation), cq: []byte(a.factory.cfg.Session), value: nil},
		a.prevRowCondition(),
	}
	if info.ServerLock == "" {
		conditions = append(conditions, condition{
			cf: []byte(metadata.CFServer), cq: []byte(metadata.CQLock), value: nil,
		})
	} else {
		conditions = append(conditions, condition{
			cf: []byte(metadata.CFServer), cq: []byte(metadata.CQLock),
			value: []byte(info.ServerLock),
		})
	}
	updates := []update{
		{cf: []byte(metadata.CFCurrentLocation), cq: []byte(a.factory.cfg.Session), value: []byte(a.factory.cfg.Address)},
		{cf: []byte(metadata.CFFutureLocation), cq: []byte(a.factory.cfg.Session), delete: true},
		{cf: []byte(metadata.CFServer), cq: []byte(metadata.CQLock), value: []byte(a.lockID())},
	}
	status, mutateErr := a.mutate(ctx, conditions, updates)
	if status == ingestclient.ConditionalAccepted {
		return nil
	}
	reconciled, readErr := a.read(context.Background())
	if readErr == nil && ownerMatches(reconciled, a.factory.cfg.Address, a.factory.cfg.Session, a.lockID()) {
		return nil
	}
	return errors.Join(ErrStaleOwner, mutateErr, readErr)
}

func (a *Authority) mutate(
	ctx context.Context,
	conditions []condition,
	updates []update,
) (ingestclient.ConditionalStatus, error) {
	if a.extent.TableID == metadata.RootTableID {
		return a.mutateRoot(ctx, conditions, updates)
	}
	row, err := metadata.EncodeTabletRow(a.extent.TableID, a.extent.EndRow)
	if err != nil {
		return ingestclient.ConditionalUnknown, err
	}
	address, tableID, target, err := a.target(ctx, row)
	if err != nil {
		return ingestclient.ConditionalUnknown, err
	}
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		return ingestclient.ConditionalUnknown, err
	}
	for _, item := range updates {
		if item.delete {
			mutation.DeleteLatest(item.cf, item.cq, nil)
		} else {
			mutation.PutLatest(item.cf, item.cq, nil, item.value)
		}
	}
	wireMutation, err := mutation.ToThrift()
	if err != nil {
		return ingestclient.ConditionalUnknown, err
	}
	wireConditions := make([]*data.TCondition, 0, len(conditions))
	for _, item := range conditions {
		var value []byte
		if item.value != nil {
			value = make([]byte, len(item.value))
			copy(value, item.value)
		}
		wireConditions = append(wireConditions, &data.TCondition{
			Cf: append([]byte(nil), item.cf...), Cq: append([]byte(nil), item.cq...),
			Cv: []byte{}, Val: value,
		})
	}
	id := a.factory.next.Add(1)
	return a.factory.cfg.Conditional.ConditionalWrite(ctx, address, tableID, target,
		&data.TConditionalMutation{Conditions: wireConditions, Mutation: wireMutation, ID: id})
}

func (a *Authority) target(
	ctx context.Context,
	row []byte,
) (string, string, *data.TKeyExtent, error) {
	return locateMetadataTarget(ctx, a.factory.cfg.Reader, a.factory.cfg.RootLocator, a.extent.TableID, row)
}

// locateMetadataTarget resolves which server holds the metadata row a
// mutation for tableID must be sent to.
//
// Rows for the metadata table itself live in the root tablet; rows for
// every other table live in the metadata table, in whichever tablet
// contains the row.
func locateMetadataTarget(
	ctx context.Context,
	reader TabletReader,
	locator RootLocator,
	tableID string,
	row []byte,
) (string, string, *data.TKeyExtent, error) {
	if tableID == metadata.MetadataTableID {
		location, err := locator.RootTabletLocation(ctx)
		if err != nil || location == nil {
			return "", "", nil, errors.Join(errors.New("metadatacas: root tablet unavailable"), err)
		}
		return location.HostPort, metadata.RootTableID,
			&data.TKeyExtent{Table: []byte(metadata.RootTableID)}, nil
	}
	tablets, err := reader.LocateTable(ctx, metadata.MetadataTableID)
	if err != nil {
		return "", "", nil, err
	}
	for _, tablet := range tablets {
		extent := ingestrouter.Extent{
			TableID: tablet.TableID, PrevEndRow: tablet.PrevRow, EndRow: tablet.EndRow,
		}
		if extent.Contains(row) && tablet.Location != nil {
			return tablet.Location.HostPort, metadata.MetadataTableID, &data.TKeyExtent{
				Table: []byte(metadata.MetadataTableID),
				// bytes.Clone, not append-to-nil, which collapses an
				// empty-but-present bound to nil. A nil prevEndRow means
				// the tablet has no lower bound; an empty one means the
				// bound is the empty row. Losing that after a split on
				// the empty row addresses the extent at a different
				// tablet than the one the row was found in, so every
				// conditional write against it is rejected forever.
				PrevEndRow: bytes.Clone(tablet.PrevRow),
				EndRow:     bytes.Clone(tablet.EndRow),
			}, nil
		}
	}
	return "", "", nil, errors.New("metadatacas: metadata tablet not located")
}

func (a *Authority) readOwned(ctx context.Context) (metadata.TabletInfo, error) {
	info, err := a.read(ctx)
	if err != nil {
		return metadata.TabletInfo{}, err
	}
	if !ownerMatches(info, a.factory.cfg.Address, a.factory.cfg.Session, a.lockID()) {
		return metadata.TabletInfo{}, ErrStaleOwner
	}
	return info, nil
}

func (a *Authority) read(ctx context.Context) (metadata.TabletInfo, error) {
	if a.extent.TableID == metadata.RootTableID {
		return a.readRoot(ctx)
	}
	tablets, err := a.factory.cfg.Reader.LocateTable(ctx, a.extent.TableID)
	if err != nil {
		return metadata.TabletInfo{}, err
	}
	for _, tablet := range tablets {
		if a.extent.Equal(ingestrouter.Extent{
			TableID: tablet.TableID, PrevEndRow: tablet.PrevRow, EndRow: tablet.EndRow,
		}) {
			return tablet, nil
		}
	}
	return metadata.TabletInfo{}, errors.New("metadatacas: tablet metadata missing")
}

func (a *Authority) ownerConditions() []condition {
	return []condition{
		{cf: []byte(metadata.CFCurrentLocation), cq: []byte(a.factory.cfg.Session), value: []byte(a.factory.cfg.Address)},
		{cf: []byte(metadata.CFServer), cq: []byte(metadata.CQLock), value: []byte(a.lockID())},
		a.prevRowCondition(),
	}
}

func (a *Authority) prevRowCondition() condition {
	return condition{
		cf: []byte(metadata.CFTabletSection), cq: []byte(metadata.CQPrevRow),
		value: metadata.EncodePrevEndRow(a.extent.PrevEndRow),
	}
}

func (a *Authority) validateIdentity(extent ingestrouter.Extent, fence ingestrouter.Fence) error {
	if !extent.Equal(a.extent) || fence != a.fence ||
		fence.ServerGeneration != a.factory.cfg.Session {
		return ErrStaleOwner
	}
	return nil
}

func (a *Authority) lockID() string {
	lock, ok := a.factory.cfg.Host.Lock()
	if !ok {
		return ""
	}
	lockPath, err := tserver.TabletServerLockIDPath(a.factory.cfg.Group, a.factory.cfg.Address)
	if err != nil {
		return ""
	}
	return path.Join(lockPath, lock.String()) + "$" + a.factory.cfg.Session
}

func ownerMatches(info metadata.TabletInfo, address, session, lock string) bool {
	return info.Location != nil && info.FutureLocation == nil &&
		info.Location.HostPort == address && info.Location.Session == session &&
		lock != "" && info.ServerLock == lock
}

func validateReference(ref walauthority.Reference) error {
	if ref.ID == "" || ref.Path == "" || ref.Qualifier != "-/"+ref.Path {
		return errors.New("metadatacas: invalid WAL reference")
	}
	return nil
}

func validateTabletTimeAdvance(current, next string) error {
	parse := func(value string) (byte, int64, error) {
		if len(value) < 2 || (value[0] != 'M' && value[0] != 'L') {
			return 0, 0, ErrInconsistent
		}
		number, err := strconv.ParseInt(value[1:], 10, 64)
		if err != nil || number < 0 {
			return 0, 0, ErrInconsistent
		}
		return value[0], number, nil
	}
	nextType, nextValue, err := parse(next)
	if err != nil {
		return err
	}
	if current == "" {
		return nil
	}
	currentType, currentValue, err := parse(current)
	if err != nil {
		return err
	}
	if currentType != nextType || nextValue < currentValue {
		return ErrInconsistent
	}
	return nil
}

func encodeFile(file mincauthority.DataFile) ([]byte, []byte, error) {
	if file.Path == "" || file.Size < 0 || file.Entries < 0 {
		return nil, nil, errors.New("metadatacas: invalid data file")
	}
	qualifier, err := json.Marshal(struct {
		Path     string `json:"path"`
		StartRow string `json:"startRow"`
		EndRow   string `json:"endRow"`
	}{Path: file.Path})
	if err != nil {
		return nil, nil, err
	}
	return qualifier, []byte(strconv.FormatInt(file.Size, 10) + "," +
		strconv.FormatInt(file.Entries, 10)), nil
}

func encodeFileEmbedding(state embeddingspace.FileState) ([]byte, error) {
	state = normalizedEmbedding(state)
	return embeddingspace.Encode(state)
}

// normalizedEmbedding fills in the state for a commit that declared
// none.
//
// The column this feeds is durable evidence about a file, so an
// undeclared commit is recorded as unknown rather than as the positive
// assertion no_embeddings. An operator who really does know a pipeline
// emits no vectors declares that upstream — see
// mincauthority.Config.DefaultEmbedding — instead of having it invented
// here, where nothing has any basis for the claim.
func normalizedEmbedding(state embeddingspace.FileState) embeddingspace.FileState {
	if state == (embeddingspace.FileState{}) {
		return embeddingspace.Unknown()
	}
	// Anything else is returned untouched, including a malformed partial
	// state. Only the exact zero value means "declared nothing";
	// normalizing a partial state would both persist something other
	// than what the caller supplied, by hiding it from Encode's
	// validation, and make a malformed reconciliation request compare
	// equal to a stored unknown.
	return state
}

type rootMetadata struct {
	Version      int                          `json:"version"`
	ColumnValues map[string]map[string]string `json:"columnValues"`
}

func (a *Authority) readRoot(ctx context.Context) (metadata.TabletInfo, error) {
	if err := ctx.Err(); err != nil {
		return metadata.TabletInfo{}, err
	}
	root, _, err := a.loadRoot()
	if err != nil {
		return metadata.TabletInfo{}, err
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return metadata.TabletInfo{}, err
	}
	return metadata.DecodeRootTabletMetadata(encoded)
}

func (a *Authority) mutateRoot(
	ctx context.Context,
	conditions []condition,
	updates []update,
) (ingestclient.ConditionalStatus, error) {
	for {
		if err := ctx.Err(); err != nil {
			return ingestclient.ConditionalUnknown, err
		}
		root, version, err := a.loadRoot()
		if err != nil {
			return ingestclient.ConditionalUnknown, err
		}
		if !rootConditions(root, conditions) {
			return ingestclient.ConditionalRejected, nil
		}
		applyRoot(root, updates)
		encoded, err := json.Marshal(root)
		if err != nil {
			return ingestclient.ConditionalUnknown, err
		}
		_, err = a.factory.cfg.RootStore.Set(a.rootPath(), encoded, version)
		if err == nil {
			return ingestclient.ConditionalAccepted, nil
		}
		if errors.Is(err, gozk.ErrBadVersion) {
			continue
		}
		// A connection failure after Set is ambiguous. The authority's caller
		// reconciles by rereading the exact owner/file/WAL state.
		return ingestclient.ConditionalUnknown, err
	}
}

func (a *Authority) loadRoot() (rootMetadata, int32, error) {
	data, stat, err := a.factory.cfg.RootStore.Get(a.rootPath())
	if err != nil {
		return rootMetadata{}, 0, err
	}
	var root rootMetadata
	if err := json.Unmarshal(data, &root); err != nil {
		return rootMetadata{}, 0, err
	}
	if root.Version != 1 || root.ColumnValues == nil || stat == nil {
		return rootMetadata{}, 0, ErrInconsistent
	}
	return root, stat.Version, nil
}

func (a *Authority) rootPath() string {
	return path.Join(a.factory.cfg.InstancePath, rootTabletPath)
}

func rootConditions(root rootMetadata, conditions []condition) bool {
	for _, item := range conditions {
		value, ok := root.ColumnValues[string(item.cf)][string(item.cq)]
		if item.value == nil {
			if ok {
				return false
			}
		} else if !ok || !bytes.Equal([]byte(value), item.value) {
			return false
		}
	}
	return true
}

func applyRoot(root rootMetadata, updates []update) {
	for _, item := range updates {
		family := string(item.cf)
		qualifier := string(item.cq)
		if item.delete {
			delete(root.ColumnValues[family], qualifier)
			if len(root.ColumnValues[family]) == 0 {
				delete(root.ColumnValues, family)
			}
			continue
		}
		if root.ColumnValues[family] == nil {
			root.ColumnValues[family] = make(map[string]string)
		}
		root.ColumnValues[family][qualifier] = string(item.value)
	}
}

var _ hostedingest.MetadataAuthority = (*Authority)(nil)
