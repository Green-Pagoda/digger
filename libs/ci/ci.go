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

// ApplyMergeChecker is an optional capability for providers that support
// enhanced mergeability checks during apply. GitHub implements this to break
// the chicken-and-egg cycle where digger/apply blocks itself as a required
// status check. See: https://github.com/diggerhq/digger/issues/1180
type ApplyMergeChecker interface {
	IsMergeableForApply(prNumber int) (bool, error)
}

// IsMergeableForApply checks mergeability using provider-specific bypass
// logic if available, falling back to standard IsMergeable otherwise.
func IsMergeableForApply(svc PullRequestService, prNumber int) (bool, error) {
	if checker, ok := svc.(ApplyMergeChecker); ok {
		return checker.IsMergeableForApply(prNumber)
	}
	return svc.IsMergeable(prNumber)
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
