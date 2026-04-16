package ci

import (
	"fmt"
	"log/slog"
	"strconv"
)

type PullRequestService interface {
	GetChangedFiles(prNumber int) ([]string, error)
	PublishComment(prNumber int, comment string) (*Comment, error)
	ListIssues() ([]*Issue, error)
	PublishIssue(title string, body string, labels *[]string) (int64, error)
	UpdateIssue(ID int64, title string, body string) (int64, error)
	EditComment(prNumber int, id string, comment string) error
	DeleteComment(id string) error
	CreateCommentReaction(id string, reaction string) error
	GetComments(prNumber int) ([]Comment, error)
	GetApprovals(prNumber int) ([]string, error)
	// SetStatus set status of specified pull/merge request, status could be: "pending", "failure", "success"
	SetStatus(prNumber int, status string, statusContext string) error
	GetCombinedPullRequestStatus(prNumber int) (string, error)
	MergePullRequest(prNumber int, mergeStrategy string) error
	// IsMergeable is still open and ready to be merged
	IsMergeable(prNumber int) (bool, error)
	// IsMerged merged and closed
	IsMerged(prNumber int) (bool, error)
	// IsClosed closed without merging
	IsClosed(prNumber int) (bool, error)
	IsDivergedFromBranch(sourceBranch string, targetBranch string) (bool, error)
	GetBranchName(prNumber int) (string, string, string, string, error)
	SetOutput(prNumber int, key string, value string) error
}

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

// IsMergeableForApply returns true when the PR is mergeable, OR when the only
// failing checks are listed in selfBlockingChecks (typically the apply check
// itself, breaking the chicken-and-egg where the apply check blocks its own
// PR). Falls back to standard IsMergeable for providers without the
// BlockedMergeInspector capability.
//
// Workflow policy ("which checks count as self-blocking") is the caller's
// concern; the CI provider only enumerates the raw mergeability state. This
// keeps platform-specific code in the provider package and workflow-specific
// code in the consuming package.
func IsMergeableForApply(svc PullRequestService, prNumber int, selfBlockingChecks []string) (bool, error) {
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
		return false, err
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

	selfBlocking := make(map[string]bool, len(selfBlockingChecks))
	for _, name := range selfBlockingChecks {
		selfBlocking[name] = true
	}
	foundSelfBlocker := false
	for _, name := range state.FailingChecks {
		if !selfBlocking[name] {
			return false, nil
		}
		foundSelfBlocker = true
	}
	// Refuse to bypass when no self-blocker was actually present — the block
	// must be caused by something we cannot resolve (signed commits,
	// unresolved conversations, etc.).
	return foundSelfBlocker, nil
}

type OrgService interface {
	GetUserTeams(organisation string, user string) ([]string, error)
}

type Issue struct {
	ID    int64
	Title string
	Body  string
}

type Comment struct {
	Id           string
	DiscussionId string // gitlab only
	Body         *string
	Url          string
}

func (c Comment) GetIdAsInt() (int, error) {
	id32, err := strconv.Atoi(c.Id)
	return id32, err
}

func (c Comment) GetIdAsInt64() (int, error) {
	id32, err := strconv.Atoi(c.Id)
	return id32, err
}

type PullRequestComment interface {
	GetUrl() (string, error)
}
