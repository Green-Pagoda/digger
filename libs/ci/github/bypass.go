package github

// MergeabilityState describes a PR's mergeability with enough detail for
// workflows to decide whether they can resolve the block themselves. Returned
// by BlockedMergeInspector.InspectMergeability.
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

// newBlockedState constructs a MergeabilityState for the blocked path,
// enforcing the Blocked=true invariant callers of the blocked case must
// always maintain. FailingChecks is defensively copied so the returned
// state cannot be mutated through the caller's slice.
//
// The mergeable fast path is a plain struct literal (MergeabilityState{
// Mergeable: true}) — it carries no correlated fields and a constructor
// would only add noise. Zero-value error returns (MergeabilityState{})
// similarly have no invariants to enforce.
func newBlockedState(reviewsBlocking bool, failing []string, truncated bool) MergeabilityState {
	var checks []string
	if len(failing) > 0 {
		checks = append(checks, failing...)
	}
	return MergeabilityState{
		Blocked:         true,
		ReviewsBlocking: reviewsBlocking,
		FailingChecks:   checks,
		Truncated:       truncated,
	}
}
