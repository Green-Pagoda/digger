package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/diggerhq/digger/libs/ci"
	gh "github.com/google/go-github/v61/github"
	"github.com/stretchr/testify/assert"
)

// newTestGithubService creates a GithubService backed by a fake HTTP server.
// The handler receives all GitHub API requests and can return canned responses.
func newTestGithubService(t *testing.T, handler http.Handler) (GithubService, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return GithubService{
		Client:   client,
		Owner:    "testowner",
		RepoName: "testrepo",
	}, server
}

// fakeGitHubAPI builds an http.Handler that routes requests to the correct
// canned response based on the URL path suffix.
func fakeGitHubAPI(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		// IsPullRequest calls Issues.Get
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/issues/1":
			json.NewEncoder(w).Encode(issue)

		// IsMergeable and IsMergeableForApply call PullRequests.Get
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			json.NewEncoder(w).Encode(pr)

		// GetCombinedStatus
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/commits/abc123/status":
			combined := &gh.CombinedStatus{
				TotalCount: gh.Int(len(statuses)),
				Statuses:   statuses,
			}
			json.NewEncoder(w).Encode(combined)

		// ListCheckRunsForRef
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/commits/abc123/check-runs":
			result := &gh.ListCheckRunsResults{
				Total:     gh.Int(len(checkRuns)),
				CheckRuns: checkRuns,
			}
			json.NewEncoder(w).Encode(result)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// makePR builds a minimal PullRequest with the given mergeable state and flag.
func makePR(mergeableState string, mergeable bool) *gh.PullRequest {
	return &gh.PullRequest{
		Mergeable:      gh.Bool(mergeable),
		MergeableState: gh.String(mergeableState),
		Head:           &gh.PullRequestBranch{SHA: gh.String("abc123")},
	}
}

// makeIssue builds a minimal Issue that looks like a PR (has PullRequestLinks).
func makeIssue() *gh.Issue {
	return &gh.Issue{
		PullRequestLinks: &gh.PullRequestLinks{URL: gh.String("https://example.com")},
	}
}

func TestBypass_CleanPR_ReturnsTrue(t *testing.T) {
	pr := makePR("clean", true)
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), nil, nil))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.True(t, result, "clean PR should be mergeable")
}

func TestBypass_BlockedOnlyByDiggerApply_ReturnsTrue(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
		{Context: gh.String("digger/plan"), State: gh.String("success")},
	}
	checkRuns := []*gh.CheckRun{
		{Name: gh.String("ci/build"), Status: gh.String("completed"), Conclusion: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), statuses, checkRuns))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.True(t, result, "should bypass when digger/apply is the only blocker")
}

func TestBypass_BlockedByOtherStatus_ReturnsFalse(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
		{Context: gh.String("ci/lint"), State: gh.String("failure")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), statuses, nil))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result, "should not bypass when another status is failing")
}

func TestBypass_BlockedByOtherCheckRun_ReturnsFalse(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	checkRuns := []*gh.CheckRun{
		{Name: gh.String("ci/build"), Status: gh.String("completed"), Conclusion: gh.String("failure")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), statuses, checkRuns))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result, "should not bypass when a check run is failing")
}

func TestBypass_DirtyState_ReturnsFalse(t *testing.T) {
	pr := makePR("dirty", false)
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), nil, nil))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result, "dirty state (merge conflicts) should not be bypassed")
}

// TestBypass_BlockedByNonCheckReason_NoDiggerApply_ReturnsFalse covers the
// case where a PR is "blocked" due to a non-status-check reason (e.g. missing
// required reviews, unresolved conversations, unsigned commits) and no
// digger/apply check is present at all. All other checks are passing.
// The bypass must NOT fire — digger/apply isn't causing the block.
func TestBypass_BlockedByNonCheckReason_NoDiggerApply_ReturnsFalse(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("ci/build"), State: gh.String("success")},
		{Context: gh.String("ci/lint"), State: gh.String("success")},
	}
	checkRuns := []*gh.CheckRun{
		{Name: gh.String("ci/test"), Status: gh.String("completed"), Conclusion: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), statuses, checkRuns))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result,
		"should not bypass when blocked for non-check reasons (e.g. missing reviews) "+
			"and digger/apply is not even present")
}

// TestBypass_BlockedByNonCheckReason_DiggerApplyAlreadyPassed_ReturnsFalse
// covers the case where digger/apply has already succeeded but the PR is still
// "blocked" — meaning something else (reviews, signatures, etc.) is blocking.
func TestBypass_BlockedByNonCheckReason_DiggerApplyAlreadyPassed_ReturnsFalse(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("success")},
		{Context: gh.String("digger/plan"), State: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), statuses, nil))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result,
		"should not bypass when digger/apply already passed — the block "+
			"must be caused by something else (reviews, signatures, etc.)")
}

// TestBypass_BlockedWithNoChecksAtAll_ReturnsFalse covers the case where a PR
// is "blocked" but there are zero status checks and zero check runs. The block
// must be entirely due to non-check branch protection (reviews, signatures,
// linear history, etc.).
func TestBypass_BlockedWithNoChecksAtAll_ReturnsFalse(t *testing.T) {
	pr := makePR("blocked", false)
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), nil, nil))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result,
		"should not bypass when there are no checks at all — the block "+
			"is caused by non-check requirements")
}

// fakeGitHubAPIWithTotals is like fakeGitHubAPI but allows overriding the
// reported total counts for statuses and check runs, simulating truncated
// (paginated) responses when total > len(items).
func fakeGitHubAPIWithTotals(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, statusTotal int, checkRuns []*gh.CheckRun, checkRunTotal int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/issues/1":
			json.NewEncoder(w).Encode(issue)

		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			json.NewEncoder(w).Encode(pr)

		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/commits/abc123/status":
			combined := &gh.CombinedStatus{
				TotalCount: gh.Int(statusTotal),
				Statuses:   statuses,
			}
			json.NewEncoder(w).Encode(combined)

		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/commits/abc123/check-runs":
			result := &gh.ListCheckRunsResults{
				Total:     gh.Int(checkRunTotal),
				CheckRuns: checkRuns,
			}
			json.NewEncoder(w).Encode(result)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestBypass_TruncatedStatuses_ReturnsFalse verifies that the bypass refuses
// to fire when the combined status response is truncated (more statuses exist
// than were returned). This prevents silently missing a failing non-digger
// check that fell beyond the first page.
func TestBypass_TruncatedStatuses_ReturnsFalse(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	// Report total=5 but only return 1 status — simulates pagination truncation
	svc, server := newTestGithubService(t,
		fakeGitHubAPIWithTotals(t, pr, makeIssue(), statuses, 5, nil, 0))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result,
		"should refuse to bypass when commit status results are truncated")
}

// TestBypass_TruncatedCheckRuns_ReturnsFalse verifies that the bypass refuses
// to fire when the check runs response is truncated.
func TestBypass_TruncatedCheckRuns_ReturnsFalse(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	checkRuns := []*gh.CheckRun{
		{Name: gh.String("ci/build"), Status: gh.String("completed"), Conclusion: gh.String("success")},
	}
	// Report total=150 but only return 1 check run
	svc, server := newTestGithubService(t,
		fakeGitHubAPIWithTotals(t, pr, makeIssue(), statuses, 1, checkRuns, 150))
	defer server.Close()

	result, err := svc.IsMergeableForApply(1)
	assert.NoError(t, err)
	assert.False(t, result,
		"should refuse to bypass when check run results are truncated")
}

func TestIsMergeableForApply_FallsBackForNonGithub(t *testing.T) {
	mock := MockCiService{CommentsPerPr: map[int][]*ci.Comment{}}
	result, err := ci.IsMergeableForApply(&mock, 1)
	assert.NoError(t, err)
	assert.True(t, result, "should fall back to IsMergeable for non-GitHub providers")
}

// TestIsMergeableForApply_BypassRunsForValueTypedGithubService verifies that
// the ci.ApplyMergeChecker capability interface is satisfied when a
// GithubService value (not pointer) is boxed into ci.PullRequestService.
// This mirrors the spec-driven CLI path (libs/spec/providers.go) where
// GithubServiceProviderBasic.NewService returns a GithubService by value.
func TestIsMergeableForApply_BypassRunsForValueTypedGithubService(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
		{Context: gh.String("digger/plan"), State: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), statuses, nil))
	defer server.Close()

	// Box the GithubService VALUE (not &svc) into the interface.
	var iface ci.PullRequestService = svc

	result, err := ci.IsMergeableForApply(iface, 1)
	assert.NoError(t, err)
	assert.True(t, result,
		"digger/apply bypass must run when GithubService is stored in "+
			"ci.PullRequestService as a value, not only as a pointer")
}
