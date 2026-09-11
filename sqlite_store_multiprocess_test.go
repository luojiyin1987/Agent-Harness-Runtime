package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sqliteProcessHelperEnv = "HARNESS_SQLITE_PROCESS_HELPER"

func TestSQLiteCheckpointStoreMultiProcessLeaseContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	const executionID = "multiprocess-execution"
	leaseDuration := 4 * time.Second

	ownerAReady := filepath.Join(t.TempDir(), "owner-a.token")
	ownerA := startSQLiteProcessHelper(t, "owner", path, executionID, leaseDuration, ownerAReady, 0)
	t.Cleanup(func() { killSQLiteProcessHelper(t, ownerA) })
	tokenA := waitForSQLiteProcessToken(t, ownerA, ownerAReady)

	runSQLiteProcessHelper(t, "busy", path, executionID, leaseDuration, "", 0)

	killSQLiteProcessHelper(t, ownerA)
	time.Sleep(leaseDuration + 500*time.Millisecond)

	ownerBReady := filepath.Join(t.TempDir(), "owner-b.token")
	ownerB := startSQLiteProcessHelper(t, "takeover", path, executionID, leaseDuration, ownerBReady, 0)
	t.Cleanup(func() { killSQLiteProcessHelper(t, ownerB) })
	tokenB := waitForSQLiteProcessToken(t, ownerB, ownerBReady)
	if tokenB <= tokenA {
		t.Fatalf("takeover token = %d, want > stale token %d", tokenB, tokenA)
	}

	runSQLiteProcessHelper(t, "stale", path, executionID, leaseDuration, "", tokenA)

	store := newTestSQLiteCheckpointStore(t, path)
	loaded, err := store.Load(context.Background(), executionID)
	if err != nil {
		t.Fatal(err)
	}
	expected := advanceLeaseCheckpoint(leaseTestCheckpoint(executionID), 1)
	if !reflect.DeepEqual(loaded, expected) {
		t.Fatalf("checkpoint after takeover = %+v, want %+v", loaded, expected)
	}
}

// TestSQLiteCheckpointStoreProcessHelper is executed only as a subprocess by
// TestSQLiteCheckpointStoreMultiProcessLeaseContention. Keeping the helper in
// the test binary exercises the same SQLite implementation across a real OS
// process boundary without adding a separate test command.
func TestSQLiteCheckpointStoreProcessHelper(t *testing.T) {
	if os.Getenv(sqliteProcessHelperEnv) != "1" {
		return
	}

	path := os.Getenv("HARNESS_SQLITE_DB")
	executionID := os.Getenv("HARNESS_SQLITE_EXECUTION_ID")
	mode := os.Getenv("HARNESS_SQLITE_MODE")
	leaseDuration, err := time.ParseDuration(os.Getenv("HARNESS_SQLITE_LEASE_DURATION"))
	if err != nil {
		t.Fatalf("parse lease duration: %v", err)
	}
	store, err := NewSQLiteCheckpointStore(path, leaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	switch mode {
	case "owner":
		token, _, err := store.AcquireExecutionLease(ctx, executionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateFenced(ctx, leaseTestCheckpoint(executionID), token); err != nil {
			t.Fatal(err)
		}
		writeSQLiteProcessToken(t, token)
		blockSQLiteProcessHelper()
	case "busy":
		_, release, err := store.AcquireExecutionLease(ctx, executionID)
		if release != nil {
			release()
		}
		if !errors.Is(err, ErrExecutionBusy) {
			t.Fatalf("AcquireExecutionLease() error = %v, want ErrExecutionBusy", err)
		}
	case "takeover":
		token, _, err := store.AcquireExecutionLease(ctx, executionID)
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, err := store.Load(ctx, executionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveFenced(ctx, advanceLeaseCheckpoint(checkpoint, 1), token); err != nil {
			t.Fatal(err)
		}
		writeSQLiteProcessToken(t, token)
		blockSQLiteProcessHelper()
	case "stale":
		staleToken, err := strconv.ParseUint(os.Getenv("HARNESS_SQLITE_STALE_TOKEN"), 10, 64)
		if err != nil {
			t.Fatalf("parse stale token: %v", err)
		}
		if err := store.RenewExecutionLease(ctx, executionID, staleToken); !errors.Is(err, ErrExecutionFenced) {
			t.Fatalf("RenewExecutionLease() error = %v, want ErrExecutionFenced", err)
		}
		checkpoint, err := store.Load(ctx, executionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveFenced(ctx, advanceLeaseCheckpoint(checkpoint, 2), staleToken); !errors.Is(err, ErrExecutionFenced) {
			t.Fatalf("SaveFenced() error = %v, want ErrExecutionFenced", err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

type sqliteProcessHelper struct {
	cmd    *exec.Cmd
	output bytes.Buffer
	done   chan struct{}
	err    error
}

func startSQLiteProcessHelper(t *testing.T, mode, path, executionID string, leaseDuration time.Duration, readyFile string, staleToken uint64) *sqliteProcessHelper {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := &sqliteProcessHelper{
		cmd: exec.Command(executable, "-test.run=^TestSQLiteCheckpointStoreProcessHelper$", "-test.count=1"),
	}
	helper.cmd.Env = append(os.Environ(),
		sqliteProcessHelperEnv+"=1",
		"HARNESS_SQLITE_MODE="+mode,
		"HARNESS_SQLITE_DB="+path,
		"HARNESS_SQLITE_EXECUTION_ID="+executionID,
		"HARNESS_SQLITE_LEASE_DURATION="+leaseDuration.String(),
		"HARNESS_SQLITE_READY_FILE="+readyFile,
		fmt.Sprintf("HARNESS_SQLITE_STALE_TOKEN=%d", staleToken),
	)
	helper.cmd.Stdout = &helper.output
	helper.cmd.Stderr = &helper.output
	if err := helper.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	helper.done = make(chan struct{})
	go func() {
		helper.err = helper.cmd.Wait()
		close(helper.done)
	}()
	return helper
}

func runSQLiteProcessHelper(t *testing.T, mode, path, executionID string, leaseDuration time.Duration, readyFile string, staleToken uint64) {
	t.Helper()
	helper := startSQLiteProcessHelper(t, mode, path, executionID, leaseDuration, readyFile, staleToken)
	<-helper.done
	if helper.err != nil {
		t.Fatalf("sqlite process helper %q failed: %v\n%s", mode, helper.err, helper.output.String())
	}
}

func waitForSQLiteProcessToken(t *testing.T, helper *sqliteProcessHelper, path string) uint64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			token, parseErr := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if parseErr == nil && token != 0 {
				return token
			}
		}
		select {
		case <-helper.done:
			t.Fatalf("sqlite process helper exited before readiness: %v\n%s", helper.err, helper.output.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	killSQLiteProcessHelper(t, helper)
	t.Fatalf("timed out waiting for sqlite process helper readiness\n%s", helper.output.String())
	return 0
}

func writeSQLiteProcessToken(t *testing.T, token uint64) {
	t.Helper()
	path := os.Getenv("HARNESS_SQLITE_READY_FILE")
	if path == "" {
		t.Fatal("HARNESS_SQLITE_READY_FILE is required")
	}
	if err := os.WriteFile(path, []byte(strconv.FormatUint(token, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func blockSQLiteProcessHelper() {
	for {
		time.Sleep(time.Hour)
	}
}

func killSQLiteProcessHelper(t *testing.T, helper *sqliteProcessHelper) {
	t.Helper()
	if helper == nil || helper.cmd == nil || helper.cmd.Process == nil {
		return
	}
	select {
	case <-helper.done:
		return
	default:
	}
	_ = helper.cmd.Process.Kill()
	<-helper.done
}
