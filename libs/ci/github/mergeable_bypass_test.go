package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/diggerhq/digger/libs/ci"
	gh "github.com/google/go-github/v61/github"
	"github.com/stretchr/testify/assert"
)

// newTestGithubService creates a GithubService backed by a fake HTTP server.
// The handler receives all GitHub API requests and can return canned responses.
// Token is set to "test-token" so the GraphQL bypass path is exercised.
func newTestGithubService(t *testing.T, handler http.Handler) (GithubService, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return GithubService{
		Client:   client,
		Owner:    "testowner",
		RepoName: "testrepo",
		Token:    "test-token",
	}, server
}

// fakeGitHubAPI builds an http.Handler that serves the minimal subset of
// endpoints the bypass logic calls: the GraphQL endpoint, Issues.Get (used by
// IsPullRequest), and PullRequests.Get.
func fakeGitHubAPI(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun) http.Handler {
	return fakeGitHubAPIFull(t, pr, issue, statuses, checkRuns, "")
}

// fakeGitHubAPIFull is like fakeGitHubAPI but accepts a reviewDecision for the
// GraphQL response (empty string = null, i.e. no review rule configured).
func fakeGitHubAPIFull(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun, reviewDecision string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "POST" && r.URL.Path == "/graphql":
			resp := buildGraphQLResponse(statuses, checkRuns, reviewDecision)
			json.NewEncoder(w).Encode(resp)

		// IsPullRequest calls Issues.Get
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/issues/1":
			json.NewEncoder(w).Encode(issue)

		// IsMergeable and IsMergeableForApply call PullRequests.Get
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			json.NewEncoder(w).Encode(pr)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// buildGraphQLResponse constructs a canned GraphQL response matching the
// bypassQuery shape from the same test data used for REST endpoints.
func buildGraphQLResponse(statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun, reviewDecision string) map[string]any {
	// Build context nodes from statuses and check runs
	var nodes []map[string]string
	for _, s := range statuses {
		// GraphQL returns state in UPPER_CASE
		nodes = append(nodes, map[string]string{
			"context": s.GetContext(),
			"state":   graphqlState(s.GetState()),
		})
	}
	for _, cr := range checkRuns {
		node := map[string]string{
			"name":   cr.GetName(),
			"status": graphqlCheckStatus(cr.GetStatus()),
		}
		if cr.GetConclusion() != "" {
			node["conclusion"] = graphqlConclusion(cr.GetConclusion())
		}
		nodes = append(nodes, node)
	}

	// reviewDecision is null (omitted) when empty
	var rd any
	if reviewDecision != "" {
		rd = reviewDecision
	}

	return map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviewDecision": rd,
					"commits": map[string]any{
						"nodes": []map[string]any{
							{
								"commit": map[string]any{
									"statusCheckRollup": map[string]any{
										"contexts": map[string]any{
											"totalCount": len(nodes),
											"nodes":      nodes,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// graphqlState converts REST status state to GraphQL StatusState enum.
func graphqlState(restState string) string {
	switch restState {
	case "success":
		return "SUCCESS"
	case "pending":
		return "PENDING"
	case "failure":
		return "FAILURE"
	case "error":
		return "ERROR"
	default:
		return fmt.Sprintf("UNKNOWN_%s", restState)
	}
}

// graphqlCheckStatus converts REST check run status to GraphQL CheckStatusState.
func graphqlCheckStatus(restStatus string) string {
	switch restStatus {
	case "completed":
		return "COMPLETED"
	case "in_progress":
		return "IN_PROGRESS"
	case "queued":
		return "QUEUED"
	default:
		return fmt.Sprintf("UNKNOWN_%s", restStatus)
	}
}

// graphqlConclusion converts REST conclusion to GraphQL CheckConclusionState.
func graphqlConclusion(restConclusion string) string {
	switch restConclusion {
	case "success":
		return "SUCCESS"
	case "failure":
		return "FAILURE"
	case "neutral":
		return "NEUTRAL"
	case "skipped":
		return "SKIPPED"
	case "cancelled":
		return "CANCELLED"
	default:
		return fmt.Sprintf("UNKNOWN_%s", restConclusion)
	}
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

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
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

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result, "should bypass when digger/apply is the only blocker")
}

// TestBypass_BlockedOnlyByDiggerApplyCheckRun_ReturnsTrue mirrors
// TestBypass_BlockedOnlyByDiggerApply_ReturnsTrue but posts digger/apply as
// a CheckRun rather than a commit status. GitHub's newer checks API is
// increasingly the preferred reporting mechanism, and this case hits a
// different branch of checkContext.IsPassing (Conclusion vs State), so
// coverage for the feature's primary check name in CheckRun form prevents
// a silent behavior gap if the backend ever switches form.
func TestBypass_BlockedOnlyByDiggerApplyCheckRun_ReturnsTrue(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/plan"), State: gh.String("success")},
	}
	checkRuns := []*gh.CheckRun{
		{Name: gh.String("digger/apply"), Status: gh.String("in_progress")},
		{Name: gh.String("ci/build"), Status: gh.String("completed"), Conclusion: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), statuses, checkRuns))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result, "should bypass when digger/apply CheckRun is the only blocker")
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

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
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

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.False(t, result, "should not bypass when a check run is failing")
}

func TestBypass_DirtyState_ReturnsFalse(t *testing.T) {
	pr := makePR("dirty", false)
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), nil, nil))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
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

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
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

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
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

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.False(t, result,
		"should not bypass when there are no checks at all — the block "+
			"is caused by non-check requirements")
}

// fakeGitHubAPIWithTotals is like fakeGitHubAPI but overrides the GraphQL
// totalCount, letting tests simulate a truncated (paginated) response where
// TotalCount > len(Contexts).
func fakeGitHubAPIWithTotals(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun, graphQLTotal int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "POST" && r.URL.Path == "/graphql":
			resp := buildGraphQLResponse(statuses, checkRuns, "")
			// Reach into the canned response to override totalCount, simulating
			// a page where GitHub reported more contexts than it returned.
			data := resp["data"].(map[string]any)
			repo := data["repository"].(map[string]any)
			prData := repo["pullRequest"].(map[string]any)
			commits := prData["commits"].(map[string]any)
			nodes := commits["nodes"].([]map[string]any)
			commit := nodes[0]["commit"].(map[string]any)
			rollup := commit["statusCheckRollup"].(map[string]any)
			contexts := rollup["contexts"].(map[string]any)
			contexts["totalCount"] = graphQLTotal
			json.NewEncoder(w).Encode(resp)

		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/issues/1":
			json.NewEncoder(w).Encode(issue)

		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			json.NewEncoder(w).Encode(pr)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestBypass_TruncatedStatuses_ReturnsError verifies that the bypass refuses
// to fire when the combined status response is truncated (more statuses exist
// than were returned) and surfaces the truncation as an actionable error.
// This prevents silently missing a failing non-digger check that fell beyond
// the first page, and tells operators the real cause rather than the generic
// "ensure all checks pass" message.
func TestBypass_TruncatedStatuses_ReturnsError(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	// Report totalCount=5 but only return 1 context — simulates pagination truncation
	svc, server := newTestGithubService(t,
		fakeGitHubAPIWithTotals(t, pr, makeIssue(), statuses, nil, 5))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err,
		"truncation should surface as an error so the real cause reaches the operator")
	assert.Contains(t, err.Error(), "truncated",
		"error message should name truncation as the cause")
	assert.False(t, result,
		"should refuse to bypass when commit status results are truncated")
}

// TestBypass_TruncatedCheckRuns_ReturnsError verifies that the bypass refuses
// to fire when the check runs response is truncated and surfaces the
// truncation as an actionable error.
func TestBypass_TruncatedCheckRuns_ReturnsError(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	checkRuns := []*gh.CheckRun{
		{Name: gh.String("ci/build"), Status: gh.String("completed"), Conclusion: gh.String("success")},
	}
	// Return 2 context nodes but report totalCount=151 — simulates truncation
	// when both statuses and check runs spill past a single page.
	svc, server := newTestGithubService(t,
		fakeGitHubAPIWithTotals(t, pr, makeIssue(), statuses, checkRuns, 151))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err,
		"truncation should surface as an error so the real cause reaches the operator")
	assert.Contains(t, err.Error(), "truncated",
		"error message should name truncation as the cause")
	assert.False(t, result,
		"should refuse to bypass when check run results are truncated")
}

// TestBypass_BlockedByReviewRequirement_BailsEarly verifies that the GraphQL
// path returns false immediately when reviewDecision is REVIEW_REQUIRED,
// without needing to inspect individual checks.
func TestBypass_BlockedByReviewRequirement_BailsEarly(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
		{Context: gh.String("ci/build"), State: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPIFull(t, pr, makeIssue(), statuses, nil, "REVIEW_REQUIRED"))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.False(t, result,
		"should bail early when reviews are required — block is not from checks")
}

// TestBypass_BlockedByChangesRequested_BailsEarly verifies early bail when a
// reviewer has requested changes.
func TestBypass_BlockedByChangesRequested_BailsEarly(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPIFull(t, pr, makeIssue(), statuses, nil, "CHANGES_REQUESTED"))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.False(t, result,
		"should bail early when changes are requested")
}

// TestBypass_BlockedByUnknownReviewDecision_BailsEarly verifies the
// allowlist semantics: any reviewDecision value other than APPROVED or
// empty is treated as blocking, so a future GitHub enum addition (e.g.
// DISMISSED_STALE) fails closed rather than silently bypassing.
func TestBypass_BlockedByUnknownReviewDecision_BailsEarly(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPIFull(t, pr, makeIssue(), statuses, nil, "SOME_FUTURE_STATE"))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.False(t, result,
		"should treat unknown reviewDecision as blocking (fail-closed allowlist)")
}

// TestBypass_ApprovedReviewWithDiggerApplyBlocking_ReturnsTrue verifies the
// full happy path: reviews are approved, digger/apply is the only blocker.
func TestBypass_ApprovedReviewWithDiggerApplyBlocking_ReturnsTrue(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
		{Context: gh.String("digger/plan"), State: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPIFull(t, pr, makeIssue(), statuses, nil, "APPROVED"))
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result,
		"should bypass when reviews are approved and digger/apply is the only blocker")
}

// TestBypass_EmptyToken_ReturnsError verifies that the bypass fails fast with
// a descriptive error when Token is not populated. This surfaces provisioning
// bugs in call sites that construct GithubService without wiring the token,
// rather than silently degrading to a path that cannot see reviewDecision.
func TestBypass_EmptyToken_ReturnsError(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	// Construct a GithubService without a Token. The blocked-state gate must
	// be reached before the Token check, so we still need the fake API to
	// answer IsPullRequest and PullRequests.Get.
	server := httptest.NewServer(fakeGitHubAPI(t, pr, makeIssue(), statuses, nil))
	defer server.Close()
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	svc := GithubService{Client: client, Owner: "testowner", RepoName: "testrepo"}

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err, "expected fail-fast error when Token is empty")
	assert.False(t, result)
	assert.Contains(t, err.Error(), "Token",
		"error should name the missing field to aid diagnosis")
}

func TestIsMergeableForApply_FallsBackForNonGithub(t *testing.T) {
	mock := MockCiService{CommentsPerPr: map[int][]*ci.Comment{}}
	result, err := ci.IsMergeableForApply(&mock, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result, "should fall back to IsMergeable for non-GitHub providers")
}

// TestBypass_GraphQLMalformedJSON_ReturnsError verifies that an upstream
// returning bytes that are not valid JSON (e.g. a gateway HTML page that
// slipped past the status check, or a corrupted proxy response) surfaces an
// error instead of silently treating the response as empty.
func TestBypass_GraphQLMalformedJSON_ReturnsError(t *testing.T) {
	pr := makePR("blocked", false)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{not-valid-json"))
		case r.URL.Path == "/repos/testowner/testrepo/issues/1":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(makeIssue())
		case r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(pr)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err, "expected unmarshal error from malformed JSON")
	assert.False(t, result)
	assert.Contains(t, err.Error(), "unmarshal",
		"error should identify the parse failure")
}

// TestBypass_NotAPullRequest_ReturnsMergeable verifies the IsPullRequest=false
// path: an issue (not a PR) is "mergeable" for workflow purposes, since there
// is nothing to block. Locks in InspectMergeability's contract for issues.
func TestBypass_NotAPullRequest_ReturnsMergeable(t *testing.T) {
	// An Issue without PullRequestLinks is the "not a PR" case.
	bareIssue := &gh.Issue{}
	pr := makePR("blocked", false) // PR fixture used only if path falls through
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/testowner/testrepo/issues/1":
			json.NewEncoder(w).Encode(bareIssue)
		case r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			t.Errorf("PullRequests.Get should not be called for an issue")
			json.NewEncoder(w).Encode(pr)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result, "issues should be treated as mergeable (closable)")
}

// TestBypass_GraphQLMultipleErrors_AllSurfaced verifies that when the GraphQL
// response carries multiple errors (one per failed field path, mixed
// auth/rate-limit/deprecation signals), every message is included in the
// wrapped error rather than only the first.
func TestBypass_GraphQLMultipleErrors_AllSurfaced(t *testing.T) {
	pr := makePR("blocked", false)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			json.NewEncoder(w).Encode(map[string]any{
				"data": nil,
				"errors": []map[string]any{
					{"message": "rate limit exceeded"},
					{"message": "field statusCheckRollup is deprecated"},
					{"message": "permission denied on reviewDecision"},
				},
			})
		case r.URL.Path == "/repos/testowner/testrepo/issues/1":
			json.NewEncoder(w).Encode(makeIssue())
		case r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			json.NewEncoder(w).Encode(pr)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err)
	assert.False(t, result)
	assert.Contains(t, err.Error(), "rate limit exceeded")
	assert.Contains(t, err.Error(), "deprecated")
	assert.Contains(t, err.Error(), "permission denied",
		"all GraphQL errors must be surfaced, not just the first")
}

// TestBypass_GraphQLNon200_TruncatesErrorBody verifies that a large upstream
// error page (as GHE edge proxies can return) is capped in the wrapped error
// rather than propagated verbatim through logs and Sentry.
func TestBypass_GraphQLNon200_TruncatesErrorBody(t *testing.T) {
	pr := makePR("blocked", false)
	hugeBody := make([]byte, 4096)
	for i := range hugeBody {
		hugeBody[i] = 'X'
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			w.WriteHeader(http.StatusBadGateway)
			w.Write(hugeBody)
		case r.URL.Path == "/repos/testowner/testrepo/issues/1":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(makeIssue())
		case r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(pr)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err)
	assert.False(t, result)
	assert.Contains(t, err.Error(), "502", "error should name the status code")
	assert.Contains(t, err.Error(), "[truncated]",
		"error should signal that the body was truncated")
	// Cap check: 512 body bytes + ~80 bytes of wrapping prefix. Well under 4096.
	assert.Less(t, len(err.Error()), 1024,
		"truncated error message should not carry the full 4KB body")
}

// TestBypass_GraphQLTimeout_ReturnsError verifies that a hung GitHub endpoint
// is bounded by the injected HTTPClient's timeout rather than wedging apply
// forever. The feature exists to unblock apply; a missing timeout would be
// strictly worse than the chicken-and-egg it is fixing.
func TestBypass_GraphQLTimeout_ReturnsError(t *testing.T) {
	pr := makePR("blocked", false)
	// Handler that blocks past the test's timeout on the GraphQL endpoint but
	// answers other endpoints normally so we reach the timeout-prone code path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			time.Sleep(500 * time.Millisecond)
			// Response after sleep is irrelevant — client will have cancelled.
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/repos/testowner/testrepo/issues/1":
			json.NewEncoder(w).Encode(makeIssue())
		case r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			json.NewEncoder(w).Encode(pr)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	// Inject a short-timeout client on the service itself — no shared global
	// to race on if this test is ever run in parallel with others.
	svc.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}

	result, err := ci.IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err, "expected timeout error from slow GraphQL endpoint")
	assert.False(t, result)
}

// TestIsMergeableForApply_BypassRunsForValueTypedGithubService verifies that
// the ci.BlockedMergeInspector capability interface is satisfied when a
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

	result, err := ci.IsMergeableForApply(iface, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result,
		"digger/apply bypass must run when GithubService is stored in "+
			"ci.PullRequestService as a value, not only as a pointer")
}
