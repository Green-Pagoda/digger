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

		// IsMergeable and isMergeableOrOnlyBlockedByDiggerApply call PullRequests.Get
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			json.NewEncoder(w).Encode(pr)

		// GetCombinedStatus
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/commits/abc123/status":
			combined := &gh.CombinedStatus{Statuses: statuses}
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

	result, err := svc.isMergeableOrOnlyBlockedByDiggerApply(1)
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

	result, err := svc.isMergeableOrOnlyBlockedByDiggerApply(1)
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

	result, err := svc.isMergeableOrOnlyBlockedByDiggerApply(1)
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

	result, err := svc.isMergeableOrOnlyBlockedByDiggerApply(1)
	assert.NoError(t, err)
	assert.False(t, result, "should not bypass when a check run is failing")
}

func TestBypass_DirtyState_ReturnsFalse(t *testing.T) {
	pr := makePR("dirty", false)
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), nil, nil))
	defer server.Close()

	result, err := svc.isMergeableOrOnlyBlockedByDiggerApply(1)
	assert.NoError(t, err)
	assert.False(t, result, "dirty state (merge conflicts) should not be bypassed")
}

func TestIsMergeableForApply_FallsBackForNonGithub(t *testing.T) {
	mock := MockCiService{CommentsPerPr: map[int][]*ci.Comment{}}
	result, err := IsMergeableForApply(&mock, 1)
	assert.NoError(t, err)
	assert.True(t, result, "should fall back to IsMergeable for non-GitHub providers")
}
