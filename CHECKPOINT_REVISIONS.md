# Checkpoint revisions

Checkpoint revisions add an ordering signal to durable execution snapshots without
adding process-local state to the Runtime.

Every checkpoint written by `Run` or `Resume` now carries a non-zero `Revision`.
The value is derived from durable execution progress, so a restarted process can
recompute the same revision from the checkpoint itself.

The logical revision advances at each persisted callback boundary:

- execution creation
- every reserved model attempt
- every reserved model retry
- entering `running_tool`
- accepting or reconciling a tool result and returning to `running_model`
- entering a terminal state

This keeps revision tracking independent from wall-clock time, process identity,
and an in-memory counter.

## Legacy compatibility

`Revision == 0` remains valid for checkpoints written before revision tracking was
introduced. A newly written Runtime checkpoint always has a non-zero revision.
A resumed legacy checkpoint therefore upgrades naturally on its next successful
write.

## FileStore stale-write protection

`FileStore.Save` loads the current snapshot before publication. Once the stored
record has a non-zero revision, a save with revision zero or with an equal/older
revision fails with `ErrCheckpointConflict` and leaves the current record intact.

The existing execution lock is still the primary ownership mechanism. `Run` and
`Resume` hold `ExecutionLocker` ownership for the whole execution. Revisions are
a second invariant that detects stale handoff or direct `FileStore` writes that
would otherwise move an execution backward.

## Boundary

This does not provide distributed compare-and-swap semantics. `FileStore` is still
a local Linux store using `flock`, atomic file publication, and fsync. A future
network/distributed store must provide its own atomic ownership or conditional
write primitive if it wants to use revisions as a fencing condition.
