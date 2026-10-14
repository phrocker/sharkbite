package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

type connectorAPI interface {
	Close() error
	NewScanner(accumulo.Table, accumulo.ScannerOptions) (*accumulo.Scanner, error)
	NewBatchScanner(accumulo.Table, accumulo.ScannerOptions) (*accumulo.BatchScanner, error)
	NewBatchWriter(accumulo.Table, accumulo.BatchWriterOptions) (*accumulo.BatchWriter, error)
	Tables(context.Context) ([]accumulo.Table, error)
	TableExists(context.Context, string) (bool, error)
	CreateTable(context.Context, string) error
	DeleteTable(context.Context, string) error
	RenameTable(context.Context, string, string) error
	FlushTable(context.Context, string, bool) error
	SetTableProperty(context.Context, string, string, string) error
	RemoveTableProperty(context.Context, string, string) error
	EffectiveTableProperties(context.Context, string) (map[string]string, error)
}

type ownedConnector struct {
	connector connectorAPI
	instance  accumulo.Instance
	identity  accumulo.InstanceInfo
	principal string
	closed    atomic.Bool

	mu      sync.Mutex
	nextID  uint64
	cancels map[uint64]context.CancelFunc
	active  int
	idle    chan struct{}

	closeOnce sync.Once
	closeErr  error
}

func newOwnedConnector(connector connectorAPI, instance accumulo.Instance) *ownedConnector {
	idle := make(chan struct{})
	close(idle)
	var identity accumulo.InstanceInfo
	if instance != nil {
		identity = instance.Info()
	}
	principal := ""
	if source, ok := connector.(interface{ Principal() string }); ok {
		principal = source.Principal()
	}
	return &ownedConnector{
		connector: connector,
		instance:  instance,
		identity:  identity,
		principal: principal,
		nextID:    1,
		cancels:   make(map[uint64]context.CancelFunc),
		idle:      idle,
	}
}

func (c *ownedConnector) retain() (func(), error) {
	c.mu.Lock()
	c.ensureStateLocked()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, accumulo.ErrConnectorClosed
	}
	if c.active == 0 {
		c.idle = make(chan struct{})
	}
	c.active++
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.releaseActiveLocked()
			c.mu.Unlock()
		})
	}, nil
}

func (c *ownedConnector) begin(timeout time.Duration) (context.Context, func(), error) {
	c.mu.Lock()
	c.ensureStateLocked()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, nil, accumulo.ErrConnectorClosed
	}
	var id uint64
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id = c.nextID
		c.nextID++
		if c.nextID == 0 {
			c.nextID = 1
		}
		if id != 0 {
			if _, exists := c.cancels[id]; !exists {
				break
			}
		}
		id = 0
	}
	if id == 0 {
		c.mu.Unlock()
		return nil, nil, errors.New("shoal: connector operation space exhausted")
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout == 0 {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	if c.active == 0 {
		c.idle = make(chan struct{})
	}
	c.cancels[id] = cancel
	c.active++
	c.mu.Unlock()

	var once sync.Once
	done := func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.cancels, id)
			c.releaseActiveLocked()
			c.mu.Unlock()
			cancel()
		})
	}
	return ctx, done, nil
}

func (c *ownedConnector) close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.closeFirst()
	})
	return c.closeErr
}

func (c *ownedConnector) closeBounded(timeout time.Duration) error {
	c.closeOnce.Do(func() {
		c.closeErr = c.closeBoundedFirst(timeout)
	})
	return c.closeErr
}

func (c *ownedConnector) closeFirst() error {
	c.mu.Lock()
	c.ensureStateLocked()
	if !c.closed.Load() {
		c.closed.Store(true)
		for _, cancel := range c.cancels {
			cancel()
		}
	}
	idle := c.idle
	c.mu.Unlock()
	<-idle
	return c.finishClose()
}

func (c *ownedConnector) closeBoundedFirst(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c.mu.Lock()
	c.ensureStateLocked()
	if !c.closed.Load() {
		c.closed.Store(true)
		for _, cancel := range c.cancels {
			cancel()
		}
	}
	idle := c.idle
	c.mu.Unlock()
	select {
	case <-idle:
		closeResult := make(chan error, 1)
		go func() {
			closeResult <- c.finishClose()
		}()
		select {
		case err := <-closeResult:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		started := make(chan struct{})
		go c.finishCloseAfterTimeout(idle, started)
		<-started
		return ctx.Err()
	}
}

func (c *ownedConnector) finishCloseAfterTimeout(
	idle <-chan struct{},
	started chan<- struct{},
) {
	if started != nil {
		close(started)
	}
	<-idle
	_ = c.finishClose()
}

func (c *ownedConnector) finishClose() error {
	var err error
	if c.connector != nil {
		err = errors.Join(err, closeResource("connector", c.connector.Close))
	}
	if c.instance != nil {
		err = errors.Join(err, closeResource("instance", c.instance.Close))
	}
	return err
}

func closeResource(name string, close func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("shoal: internal panic closing %s: %v", name, recovered)
		}
	}()
	return close()
}

func (c *ownedConnector) ensureStateLocked() {
	if c.nextID == 0 {
		c.nextID = 1
	}
	if c.cancels == nil {
		c.cancels = make(map[uint64]context.CancelFunc)
	}
	if c.idle == nil {
		c.idle = make(chan struct{})
		close(c.idle)
	}
}

func (c *ownedConnector) releaseActiveLocked() {
	c.active--
	if c.active == 0 {
		close(c.idle)
	}
}

func (c *ownedConnector) isClosed() bool {
	return c != nil && c.closed.Load()
}

type connectorRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*ownedConnector
}

func newConnectorRegistry() *connectorRegistry {
	return &connectorRegistry{
		nextID: 1,
		items:  make(map[uint64]*ownedConnector),
	}
}

func (r *connectorRegistry) add(connector *ownedConnector) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id == 0 {
			continue
		}
		if _, exists := r.items[id]; !exists {
			r.items[id] = connector
			return id, true
		}
	}
	return 0, false
}

func (r *connectorRegistry) get(id uint64) (*ownedConnector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	connector, ok := r.items[id]
	return connector, ok
}

func (r *connectorRegistry) remove(id uint64) (*ownedConnector, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	connector, ok := r.items[id]
	if ok {
		delete(r.items, id)
	}
	return connector, ok
}

var connectors = newConnectorRegistry()

type configurationRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*accumulo.Configuration
}

func newConfigurationRegistry() *configurationRegistry {
	return &configurationRegistry{nextID: 1, items: make(map[uint64]*accumulo.Configuration)}
}

func (r *configurationRegistry) add(configuration *accumulo.Configuration) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id != 0 {
			if _, exists := r.items[id]; !exists {
				r.items[id] = configuration
				return id, true
			}
		}
	}
	return 0, false
}

func (r *configurationRegistry) get(id uint64) (*accumulo.Configuration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	configuration, ok := r.items[id]
	return configuration, ok
}

func (r *configurationRegistry) remove(id uint64) {
	r.mu.Lock()
	delete(r.items, id)
	r.mu.Unlock()
}

var configurations = newConfigurationRegistry()

type ownedCancellation struct {
	mu        sync.Mutex
	cancelled bool
	closed    bool
	nextID    uint64
	cancels   map[uint64]context.CancelFunc
	active    sync.WaitGroup
}

func newOwnedCancellation() *ownedCancellation {
	return &ownedCancellation{
		nextID:  1,
		cancels: make(map[uint64]context.CancelFunc),
	}
}

func (c *ownedCancellation) attach(parent context.Context) (context.Context, func(), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, accumulo.ErrConnectorClosed
	}
	ctx, cancel := context.WithCancel(parent)
	if c.cancelled {
		c.mu.Unlock()
		cancel()
		return ctx, func() {}, nil
	}
	var id uint64
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id = c.nextID
		c.nextID++
		if c.nextID == 0 {
			c.nextID = 1
		}
		if id != 0 {
			if _, exists := c.cancels[id]; !exists {
				break
			}
		}
		id = 0
	}
	if id == 0 {
		c.mu.Unlock()
		cancel()
		return nil, nil, errors.New("shoal: cancellation operation space exhausted")
	}
	c.cancels[id] = cancel
	c.active.Add(1)
	c.mu.Unlock()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.cancels, id)
			c.mu.Unlock()
			cancel()
			c.active.Done()
		})
	}, nil
}

func (c *ownedCancellation) cancel() {
	c.mu.Lock()
	if !c.cancelled {
		c.cancelled = true
		for _, cancel := range c.cancels {
			cancel()
		}
	}
	c.mu.Unlock()
}

func (c *ownedCancellation) isCancelled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled
}

func (c *ownedCancellation) close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancelled = true
		for _, cancel := range c.cancels {
			cancel()
		}
	}
	c.mu.Unlock()
	c.active.Wait()
}

type cancellationRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*ownedCancellation
}

func newCancellationRegistry() *cancellationRegistry {
	return &cancellationRegistry{
		nextID: 1,
		items:  make(map[uint64]*ownedCancellation),
	}
}

func (r *cancellationRegistry) add(cancellation *ownedCancellation) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id != 0 {
			if _, exists := r.items[id]; !exists {
				r.items[id] = cancellation
				return id, true
			}
		}
	}
	return 0, false
}

func (r *cancellationRegistry) get(id uint64) (*ownedCancellation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cancellation, ok := r.items[id]
	return cancellation, ok
}

func (r *cancellationRegistry) remove(id uint64) (*ownedCancellation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancellation, ok := r.items[id]
	delete(r.items, id)
	return cancellation, ok
}

var cancellations = newCancellationRegistry()

type ownedScanner struct {
	single *accumulo.Scanner
	batch  *accumulo.BatchScanner
	owner  *ownedConnector

	streamOne  func(context.Context, *accumulo.Range) (scanCursorSource, error)
	streamMany func(context.Context, []*accumulo.Range) (scanCursorSource, error)

	mu      sync.Mutex
	closed  bool
	nextID  uint64
	cancels map[uint64]context.CancelFunc
	active  sync.WaitGroup
}

func newOwnedScanner(
	single *accumulo.Scanner,
	batch *accumulo.BatchScanner,
	owner *ownedConnector,
) *ownedScanner {
	scanner := &ownedScanner{
		single:  single,
		batch:   batch,
		owner:   owner,
		nextID:  1,
		cancels: make(map[uint64]context.CancelFunc),
	}
	scanner.streamOne = func(ctx context.Context, scanRange *accumulo.Range) (scanCursorSource, error) {
		return scanner.single.Stream(ctx, scanRange)
	}
	scanner.streamMany = func(ctx context.Context, ranges []*accumulo.Range) (scanCursorSource, error) {
		return scanner.batch.Stream(ctx, ranges)
	}
	return scanner
}

func (s *ownedScanner) begin(timeout time.Duration) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, accumulo.ErrConnectorClosed
	}

	var ownerDone func()
	if s.owner != nil {
		var err error
		ownerDone, err = s.owner.retain()
		if err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
	}
	var (
		id     uint64
		ctx    context.Context
		cancel context.CancelFunc
	)
	var err error
	defer func() {
		if err != nil && ownerDone != nil {
			ownerDone()
		}
	}()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id = s.nextID
		s.nextID++
		if s.nextID == 0 {
			s.nextID = 1
		}
		if id != 0 {
			if _, exists := s.cancels[id]; !exists {
				break
			}
		}
		id = 0
	}
	if id == 0 {
		s.mu.Unlock()
		err = errors.New("shoal: scanner operation space exhausted")
		return nil, nil, err
	}
	if timeout == 0 {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	s.cancels[id] = cancel
	s.active.Add(1)
	s.mu.Unlock()

	var once sync.Once
	done := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.cancels, id)
			s.mu.Unlock()
			cancel()
			s.active.Done()
			if ownerDone != nil {
				ownerDone()
			}
		})
	}
	return ctx, done, nil
}

func (s *ownedScanner) beginCancelable(
	timeout time.Duration,
	cancellation *ownedCancellation,
) (context.Context, func(), error) {
	ctx, scannerDone, err := s.begin(timeout)
	if err != nil {
		return nil, nil, err
	}
	if cancellation == nil {
		return ctx, scannerDone, nil
	}
	ctx, cancellationDone, err := cancellation.attach(ctx)
	if err != nil {
		scannerDone()
		return nil, nil, err
	}
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancellationDone()
			scannerDone()
		})
	}, nil
}

func (s *ownedScanner) beginStream(
	timeout time.Duration,
	cancellation *ownedCancellation,
) (context.Context, func(), error) {
	ctx, scannerDone, err := s.begin(timeout)
	if err != nil {
		return nil, nil, err
	}

	var (
		ownerDone func()
		stopOwner func() bool
		cancel    context.CancelFunc
	)
	if s.owner != nil {
		ownerCtx, done, ownerErr := s.owner.begin(timeout)
		if ownerErr != nil {
			scannerDone()
			return nil, nil, ownerErr
		}
		ownerDone = done
		ctx, cancel = context.WithCancel(ctx)
		stopOwner = context.AfterFunc(ownerCtx, cancel)
	}

	var cancellationDone func()
	if cancellation != nil {
		ctx, cancellationDone, err = cancellation.attach(ctx)
		if err != nil {
			if stopOwner != nil {
				stopOwner()
				cancel()
				ownerDone()
			}
			scannerDone()
			return nil, nil, err
		}
	}

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			if cancellationDone != nil {
				cancellationDone()
			}
			if stopOwner != nil {
				stopOwner()
				cancel()
				ownerDone()
			}
			scannerDone()
		})
	}, nil
}

func (s *ownedScanner) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, cancel := range s.cancels {
			cancel()
		}
	}
	s.mu.Unlock()
	s.active.Wait()
}

type scannerRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*ownedScanner
}

func newScannerRegistry() *scannerRegistry {
	return &scannerRegistry{
		nextID: 1,
		items:  make(map[uint64]*ownedScanner),
	}
}

func (r *scannerRegistry) add(scanner *ownedScanner) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id == 0 {
			continue
		}
		if _, exists := r.items[id]; !exists {
			r.items[id] = scanner
			return id, true
		}
	}
	return 0, false
}

func (r *scannerRegistry) get(id uint64) (*ownedScanner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	scanner, ok := r.items[id]
	return scanner, ok
}

func (r *scannerRegistry) remove(id uint64) (*ownedScanner, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	scanner, ok := r.items[id]
	if ok {
		delete(r.items, id)
	}
	return scanner, ok
}

var scanners = newScannerRegistry()
var batchScanners = newScannerRegistry()

type scanCursorRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*ownedScanCursor
}

func newScanCursorRegistry() *scanCursorRegistry {
	return &scanCursorRegistry{
		nextID: 1,
		items:  make(map[uint64]*ownedScanCursor),
	}
}

func (r *scanCursorRegistry) add(cursor *ownedScanCursor) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id != 0 {
			if _, exists := r.items[id]; !exists {
				r.items[id] = cursor
				return id, true
			}
		}
	}
	return 0, false
}

func (r *scanCursorRegistry) get(id uint64) (*ownedScanCursor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cursor, ok := r.items[id]
	return cursor, ok
}

func (r *scanCursorRegistry) remove(id uint64) (*ownedScanCursor, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cursor, ok := r.items[id]
	delete(r.items, id)
	return cursor, ok
}

var scanCursors = newScanCursorRegistry()

type mutationRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*accumulo.Mutation
}

func newMutationRegistry() *mutationRegistry {
	return &mutationRegistry{
		nextID: 1,
		items:  make(map[uint64]*accumulo.Mutation),
	}
}

func (r *mutationRegistry) add(mutation *accumulo.Mutation) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id == 0 {
			continue
		}
		if _, exists := r.items[id]; !exists {
			r.items[id] = mutation
			return id, true
		}
	}
	return 0, false
}

func (r *mutationRegistry) get(id uint64) (*accumulo.Mutation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mutation, ok := r.items[id]
	return mutation, ok
}

func (r *mutationRegistry) remove(id uint64) (*accumulo.Mutation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mutation, ok := r.items[id]
	if ok {
		delete(r.items, id)
	}
	return mutation, ok
}

var mutations = newMutationRegistry()

type batchWriterAPI interface {
	Add(context.Context, *accumulo.Mutation) error
	Flush(context.Context) error
	Close(context.Context) error
}

type ownedBatchWriter struct {
	writer batchWriterAPI
	owner  *ownedConnector

	mu      sync.Mutex
	closed  bool
	nextID  uint64
	cancels map[uint64]context.CancelFunc
	active  int
	idle    chan struct{}

	closeOnce sync.Once
	closeErr  error
}

func newOwnedBatchWriter(writer batchWriterAPI, owner *ownedConnector) *ownedBatchWriter {
	idle := make(chan struct{})
	close(idle)
	return &ownedBatchWriter{
		writer:  writer,
		owner:   owner,
		nextID:  1,
		cancels: make(map[uint64]context.CancelFunc),
		idle:    idle,
	}
}

func (w *ownedBatchWriter) begin(timeout time.Duration) (context.Context, func(), error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, nil, accumulo.ErrBatchWriterClosed
	}
	if w.owner.isClosed() {
		w.mu.Unlock()
		return nil, nil, accumulo.ErrConnectorClosed
	}
	var id uint64
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id = w.nextID
		w.nextID++
		if w.nextID == 0 {
			w.nextID = 1
		}
		if id != 0 {
			if _, exists := w.cancels[id]; !exists {
				break
			}
		}
		id = 0
	}
	if id == 0 {
		w.mu.Unlock()
		return nil, nil, errors.New("shoal: batch writer operation space exhausted")
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout == 0 {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	if w.active == 0 {
		w.idle = make(chan struct{})
	}
	w.cancels[id] = cancel
	w.active++
	w.mu.Unlock()

	var once sync.Once
	done := func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.cancels, id)
			w.active--
			if w.active == 0 {
				close(w.idle)
			}
			w.mu.Unlock()
			cancel()
		})
	}
	return ctx, done, nil
}

func (w *ownedBatchWriter) close(timeout time.Duration) error {
	w.closeOnce.Do(func() {
		w.closeErr = w.closeFirst(timeout)
	})
	return w.closeErr
}

func (w *ownedBatchWriter) closeFirst(timeout time.Duration) error {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout == 0 {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()

	w.mu.Lock()
	if !w.closed {
		w.closed = true
		for _, cancel := range w.cancels {
			cancel()
		}
	}
	idle := w.idle
	w.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
		go w.finishCloseAfterTimeout(idle)
		return ctx.Err()
	}
	return w.writer.Close(ctx)
}

func (w *ownedBatchWriter) finishCloseAfterTimeout(idle <-chan struct{}) {
	timer := time.NewTimer(batchWriterFreeTimeout)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), batchWriterFreeTimeout)
	defer cancel()
	_ = w.writer.Close(ctx)
}

type batchWriterRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*ownedBatchWriter
}

func newBatchWriterRegistry() *batchWriterRegistry {
	return &batchWriterRegistry{
		nextID: 1,
		items:  make(map[uint64]*ownedBatchWriter),
	}
}

func (r *batchWriterRegistry) add(writer *ownedBatchWriter) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id == 0 {
			continue
		}
		if _, exists := r.items[id]; !exists {
			r.items[id] = writer
			return id, true
		}
	}
	return 0, false
}

func (r *batchWriterRegistry) get(id uint64) (*ownedBatchWriter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	writer, ok := r.items[id]
	return writer, ok
}

func (r *batchWriterRegistry) remove(id uint64) (*ownedBatchWriter, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	writer, ok := r.items[id]
	if ok {
		delete(r.items, id)
	}
	return writer, ok
}

var batchWriters = newBatchWriterRegistry()
