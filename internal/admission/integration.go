package admission

import (
	"context"
)

// Common Request Boundary Integration
//
// As specified in PLAN_ROUTING_ORCHESTRATION.md (§2), all requests across root turns,
// delegated children, resume, compaction, /ask, standalone council/scout paths, and
// council stages must follow a strict three-tier acquisition order:
//
//   1. Execution Limit:
//      Acquire live-agent slot in tree (e.g., Runtime.acquireLive).
//
//   2. Provider Stream Limit:
//      Acquire concurrency slot for provider (e.g., sem := Runtime.Limit(provider); sem <- struct{}{}).
//
//   3. Premium Quota Slot:
//      Acquire admission lease (admission.Acquire).
//
//   4. HTTP Request / Model Stream Execution:
//      Issue provider stream call and consume events.
//
//   5. Request Lifetime & Caller Cleanup:
//      Complete all response reading, error handling, and resource closure.
//
//   6. Release Premium Quota Slot:
//      Call handle.Close().
//
//   7. Release Provider Stream Limit:
//      Release semaphore (<-sem).
//
//   8. Release Execution Limit:
//      Release live-agent slot (releaseLive).
//
// Crucial Invariant:
//   A premium quota slot must NEVER be held while waiting for a provider stream slot,
//   running tools (e.g. bash commands), or waiting on subagent tasks.
//   Releasing admission immediately upon completion of model stream cleanup prevents
//   quota starvation across independent processes.

// StreamFunc represents the execution of a model stream up to response completion.
type StreamFunc func(ctx context.Context) error

// ExecuteAtBoundary coordinates admission acquisition and guaranteed release
// at the common request boundary. It assumes provider and execution limits
// have already been acquired by the caller.
func ExecuteAtBoundary(ctx context.Context, m *Manager, req Request, streamFn StreamFunc) error {
	handle, err := m.Acquire(ctx, req)
	if err != nil {
		return err
	}
	defer func() {
		_ = handle.Close()
	}()

	return streamFn(ctx)
}
