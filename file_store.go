package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// FileStore keeps one JSON checkpoint per execution in a trusted local directory.
// It requires a filesystem with atomic rename/link and file/directory fsync
// semantics (tested on Linux). There is no distributed writer coordination.
type FileStore struct {
	directory string
}

func NewFileStore(directory string) (*FileStore, error) {
	if directory == "" {
		return nil, fmt.Errorf("%w: store directory is required", ErrInvalidRequest)
	}
	path, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	// The parent must already exist; syncing it makes creation of the store
	// directory durable without silently relying on unsynced ancestor directories.
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("checkpoint store path is not a directory: %s", path)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return &FileStore{directory: path}, nil
}

func (s *FileStore) Create(ctx context.Context, checkpoint Checkpoint) error {
	return s.write(ctx, checkpoint, true)
}

func (s *FileStore) Save(ctx context.Context, checkpoint Checkpoint) error {
	return s.write(ctx, checkpoint, false)
}

func (s *FileStore) Load(ctx context.Context, executionID string) (Checkpoint, error) {
	if err := checkStoreContext(ctx); err != nil {
		return Checkpoint{}, err
	}
	if executionID == "" {
		return Checkpoint{}, fmt.Errorf("%w: execution ID is required", ErrInvalidCheckpoint)
	}
	data, err := os.ReadFile(s.path(executionID))
	if errors.Is(err, os.ErrNotExist) {
		return Checkpoint{}, fmt.Errorf("%w: %q", ErrExecutionNotFound, executionID)
	}
	if err != nil {
		return Checkpoint{}, err
	}
	if err := ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %w", ErrInvalidCheckpoint, err)
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return Checkpoint{}, err
	}
	if checkpoint.ExecutionID != executionID {
		return Checkpoint{}, fmt.Errorf("%w: execution ID mismatch", ErrInvalidCheckpoint)
	}
	return checkpoint, nil
}

func (s *FileStore) write(ctx context.Context, checkpoint Checkpoint, create bool) error {
	if err := checkStoreContext(ctx); err != nil {
		return err
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	target := s.path(checkpoint.ExecutionID)
	if !create {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %q", ErrExecutionNotFound, checkpoint.ExecutionID)
		} else if err != nil {
			return err
		}
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.directory, ".checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if create {
		// Link publishes a complete file only if the ID is unused. Unlike a
		// check followed by rename, concurrent Create calls cannot overwrite it.
		if err := os.Link(temp.Name(), target); errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %q", ErrExecutionExists, checkpoint.ExecutionID)
		} else if err != nil {
			return err
		}
	} else if err := os.Rename(temp.Name(), target); err != nil {
		return err
	}
	return syncDirectory(s.directory)
}

func (s *FileStore) path(executionID string) string {
	// IDs never become paths, including IDs supplied by an external caller.
	return filepath.Join(s.directory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(executionID))))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func checkStoreContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	return ctx.Err()
}

func validateCheckpoint(checkpoint Checkpoint) error {
	if checkpoint.SchemaVersion != 1 && checkpoint.SchemaVersion != CheckpointSchemaVersion {
		return fmt.Errorf("%w: unsupported schema version %d", ErrInvalidCheckpoint, checkpoint.SchemaVersion)
	}
	if checkpoint.ExecutionID == "" || checkpoint.ExecutionID != checkpoint.Request.ExecutionID || checkpoint.ExecutionID != checkpoint.Result.ExecutionID {
		return fmt.Errorf("%w: inconsistent execution ID", ErrInvalidCheckpoint)
	}
	if checkpoint.Request.Prompt == "" || checkpoint.MaxSteps <= 0 || checkpoint.ModelIterations < 0 || checkpoint.ModelIterations > checkpoint.MaxSteps {
		return fmt.Errorf("%w: invalid request or iteration budget", ErrInvalidCheckpoint)
	}
	status := StatusCreated
	for _, transition := range checkpoint.Result.Transitions {
		if transition.From != status || !validTransition(status, transition.To) {
			return fmt.Errorf("%w: invalid transition history", ErrInvalidCheckpoint)
		}
		status = transition.To
	}
	if status != checkpoint.Result.Status {
		return fmt.Errorf("%w: status does not match transition history", ErrInvalidCheckpoint)
	}
	if checkpoint.SchemaVersion == CheckpointSchemaVersion {
		if err := validateRecoveryState(checkpoint); err != nil {
			return err
		}
	}
	// encoding/json replaces invalid UTF-8 silently. Reject it so a successful
	// write cannot change prompts, tool arguments, outputs, or execution IDs.
	values := []string{checkpoint.ExecutionID, checkpoint.Request.Prompt, checkpoint.Result.Output, checkpoint.Error}
	addCall := func(call ToolCall) {
		values = append(values, call.ID, call.Name, call.Arguments)
	}
	if checkpoint.PendingTool != nil {
		addCall(*checkpoint.PendingTool)
	}
	for _, step := range checkpoint.Result.Steps {
		addCall(step.Call)
		values = append(values, step.Result.CallID, step.Result.Output)
	}
	for _, value := range values {
		if !utf8.ValidString(value) {
			return fmt.Errorf("%w: checkpoint strings must be valid UTF-8", ErrInvalidCheckpoint)
		}
	}
	return nil
}
