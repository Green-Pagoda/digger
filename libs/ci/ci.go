package ci

import "strconv"

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
	// inspector saw no enumerable check failures (so the block must come
	// from another non-check requirement, or the inspector could not see
	// the full set due to truncation).
	FailingChecks []string
}

// BlockedMergeInspector is an optional capability for providers that can
// enumerate why a PR is blocked. Workflows (e.g. the digger/apply
// chicken-and-egg bypass) use the result to decide whether the block is
// something they can resolve themselves.
// See: https://github.com/diggerhq/digger/issues/1180
type BlockedMergeInspector interface {
	InspectMergeability(prNumber int) (*MergeabilityState, error)
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
	// unresolved conversations, truncated check enumeration, etc.).
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
