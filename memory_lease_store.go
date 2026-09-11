package harness

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type memoryExecutionLease struct {
	token     uint64
	expiresAt time.Time
}

// MemoryLeaseStore is an in-memory reference implementation of lease fencing.
// It is intended for tests, examples, and learning. State is process-local and
// disappears on restart, so it is not a distributed durability backend.
type MemoryLeaseStore struct {
	mu            sync.Mutex
	leaseDuration time.Duration
	now           func() time.Time
	records       map[string]Checkpoint
	leases        map[string]memoryExecutionLease
	lastToken     map[string]uint64
}

func NewMemoryLeaseStore(leaseDuration time.Duration) (*MemoryLeaseStore, error) {
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("%w: lease duration must be positive", ErrInvalidRequest)
	}
	return &MemoryLeaseStore{
		leaseDuration: leaseDuration,
		now:           time.Now,
		records:       make(map[string]Checkpoint),
		leases:        make(map[string]memoryExecutionLease),
		lastToken:     make(map[string]uint64),
	}, nil
}

func (s *MemoryLeaseStore) AcquireExecutionLease(ctx context.Context, executionID string) (uint64, func(), error) {
	if err := checkMemoryLeaseContext(ctx); err != nil {
		return 0, nil, err
	}
	if executionID == "" {
		return 0, nil, fmt.Errorf("%w: execution ID is required", ErrInvalidRequest)
	}

	s.mu.Lock()
	now := s.now()
	if lease, ok := s.leases[executionID]; ok && now.Before(lease.expiresAt) {
		s.mu.Unlock()
		return 0, nil, ErrExecutionBusy
	}
	token := s.lastToken[executionID] + 1
	if token == 0 {
		s.mu.Unlock()
		return 0, nil, fmt.Errorf("%w: fencing token exhausted", ErrExecutionFenced)
	}
	s.lastToken[executionID] = token
	s.leases[executionID] = memoryExecutionLease{token: token, expiresAt: now.Add(s.leaseDuration)}
	s.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if lease, ok := s.leases[executionID]; ok && lease.token == token {
				delete(s.leases, executionID)
			}
		})
	}
	return token, release, nil
}

// Plain writes are intentionally rejected. A MemoryLeaseStore exists to model
// fencing, so bypassing its lease token would make the reference unsafe.
func (s *MemoryLeaseStore) Create(context.Context, Checkpoint) error {
	return ErrExecutionLeaseRequired
}

func (s *MemoryLeaseStore) Save(context.Context, Checkpoint) error {
	return ErrExecutionLeaseRequired
}

func (s *MemoryLeaseStore) CreateFenced(ctx context.Context, checkpoint Checkpoint, fencingToken uint64) error {
	return s.writeFenced(ctx, checkpoint, fencingToken, true)
}

func (s *MemoryLeaseStore) SaveFenced(ctx context.Context, checkpoint Checkpoint, fencingToken uint64) error {
	return s.writeFenced(ctx, checkpoint, fencingToken, false)
}

func (s *MemoryLeaseStore) Load(ctx context.Context, executionID string) (Checkpoint, error) {
	if err := checkMemoryLeaseContext(ctx); err != nil {
		return Checkpoint{}, err
	}
	if executionID == "" {
		return Checkpoint{}, fmt.Errorf("%w: execution ID is required", ErrInvalidCheckpoint)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.records[executionID]
	if !ok {
		return Checkpoint{}, fmt.Errorf("%w: %q", ErrExecutionNotFound, executionID)
	}
	return cloneCheckpoint(checkpoint), nil
}

func (s *MemoryLeaseStore) writeFenced(ctx context.Context, checkpoint Checkpoint, fencingToken uint64, create bool) error {
	if err := checkMemoryLeaseContext(ctx); err != nil {
		return err
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if fencingToken == 0 {
		return ErrExecutionLeaseRequired
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	lease, ok := s.leases[checkpoint.ExecutionID]
	if !ok || lease.token != fencingToken || !now.Before(lease.expiresAt) {
		return fmt.Errorf("%w: execution %q token %d", ErrExecutionFenced, checkpoint.ExecutionID, fencingToken)
	}
	current, exists := s.records[checkpoint.ExecutionID]
	if create {
		if exists {
			return fmt.Errorf("%w: %q", ErrExecutionExists, checkpoint.ExecutionID)
		}
	} else {
		if !exists {
			return fmt.Errorf("%w: %q", ErrExecutionNotFound, checkpoint.ExecutionID)
		}
		if current.Revision > 0 && (checkpoint.Revision == 0 || checkpoint.Revision <= current.Revision) {
			return fmt.Errorf("%w: execution %q has revision %d, attempted %d", ErrCheckpointConflict, checkpoint.ExecutionID, current.Revision, checkpoint.Revision)
		}
	}
	s.records[checkpoint.ExecutionID] = cloneCheckpoint(checkpoint)
	return nil
}

func checkMemoryLeaseContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	return ctx.Err()
}
