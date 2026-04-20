package github

import (
	"fmt"
	"log/slog"

	"github.com/diggerhq/digger/libs/ci"
)

// MergeabilityState describes a PR's mergeability with enough detail for
// workflows to decide whether they can resolve the block themselves. Returned
// by BlockedMergeInspector.InspectMergeability.
//
// Field validity is conditional: Mergeable=true short-circuits the state —
// the other fields carry no meaning. ReviewsBlocking, FailingChecks, and
// Truncated are only meaningful when Blocked=true; when Blocked=false (and
// Mergeable=false), the PR is in a non-resolvable state (dirty, behind,
// unknown) and those fields are zero values.
type MergeabilityState struct {
	// Mergeable is true when the PR is already mergeable by the platform's
	// normal rules. Workflows can short-circuit on this without inspecting
	// the other fields.
	Mergeable bool

	// Blocked is true when the PR's mergeable state is specifically
	// "blocked" (branch protection failing) — the only state a workflow can
	// hope to resolve. False with Mergeable=false means another non-resolvable
	// state (dirty, behind, unknown).
	Blocked bool

	// ReviewsBlocking is true when the block is caused by missing required
	// reviews or requested changes. Only meaningful when Blocked is true.
	// When true, no status-check workflow can unblock the PR.
	ReviewsBlocking bool

	// FailingChecks lists the names of non-passing required status checks
	// and check runs. Only populated when Blocked is true. Empty means the
	// inspector saw no enumerable check failures, so the block must come
	// from another non-check requirement.
	FailingChecks []string

	// Truncated is true when the inspector could not enumerate the full
	// set of checks (e.g. the upstream API paginated and more pages exist).
	// Callers that rely on FailingChecks being exhaustive must treat a
	// truncated state as "cannot determine" rather than "no failures".
	Truncated bool
}

// BlockedMergeInspector is an optional capability for providers that can
// enumerate why a PR is blocked. The digger/apply chicken-and-egg bypass
// uses the result to decide whether the block is a check the workflow can
// resolve itself.
type BlockedMergeInspector interface {
	InspectMergeability(prNumber int) (MergeabilityState, error)
}

// diggerApplyCheck is the status-check name the bypass treats as
// self-blocking. It must match the name Digger reports via SetStatus /
// CreateCheckRun on apply jobs — a rename in either place silently breaks
// the bypass.
const diggerApplyCheck = "digger/apply"

// IsMergeable reports whether a PR is mergeable, correctly interpreting the
// self-referential digger/apply check. The platform marks a PR "blocked"
// when digger/apply (a required status check) hasn't passed — but
// digger/apply only turns green AFTER apply runs, so its red state is
// expected, not a real blocker. When it's the sole failing check, this
// function reports true; the platform's own IsMergeable cannot, because
// it doesn't know digger/apply is self-referential.
//
// Falls back to the platform's plain IsMergeable for providers without the
// BlockedMergeInspector capability.
func IsMergeable(svc ci.PullRequestService, prNumber int) (bool, error) {
	inspector, ok := svc.(BlockedMergeInspector)
	if !ok {
		// Non-GitHub providers (GitLab, Bitbucket, Azure) do not implement
		// the inspector capability — the chicken-and-egg this code solves
		// is specific to GitHub's "blocked" branch-protection state. Debug
		// level (not Warn) because this path fires on every apply for
		// those providers and is expected behavior.
		slog.Debug("BlockedMergeInspector not implemented; using plain IsMergeable (digger/apply bypass unsupported for this provider)",
			"providerType", fmt.Sprintf("%T", svc), "prNumber", prNumber)
		return svc.IsMergeable(prNumber)
	}
	state, err := inspector.InspectMergeability(prNumber)
	if err != nil {
		return false, fmt.Errorf("InspectMergeability for PR %d: %w", prNumber, err)
	}
	if state.Mergeable {
		return true, nil
	}
	if !state.Blocked || state.ReviewsBlocking {
		return false, nil
	}
	if state.Truncated {
		// We cannot prove the only blocker is a self-blocking check when we
		// cannot see the full list. Surface the real cause to the caller
		// rather than falsely reporting "not mergeable, ensure checks pass".
		return false, fmt.Errorf("cannot determine mergeability: status check list was truncated by upstream API")
	}

	for _, name := range state.FailingChecks {
		if name != diggerApplyCheck {
			return false, nil
		}
	}
	// Refuse to bypass when no self-blocker was actually present — the block
	// must be caused by something we cannot resolve (signed commits,
	// unresolved conversations, etc.).
	return len(state.FailingChecks) > 0, nil
}
