package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	base, err := client.BaseURL.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	client.BaseURL = base
	return GithubService{
		Client:   client,
		Owner:    "testowner",
		RepoName: "testrepo",
		Token:    "test-token",
	}, server
}

// writeJSON encodes v as JSON onto w and fails the test on encode errors.
// Swallowing the error would mask fixture bugs as confusing empty-body or
// partial-response failures at the assertion layer.
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("failed to encode response: %v", err)
	}
}

// fakeGitHubAPI builds an http.Handler that serves the minimal subset of
// endpoints InspectMergeability calls: the GraphQL endpoint, Issues.Get (used
// by IsPullRequest), and PullRequests.Get.
func fakeGitHubAPI(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun) http.Handler {
	return fakeGitHubAPIFull(t, pr, issue, statuses, checkRuns, "", -1)
}

// fakeGitHubAPIFull is like fakeGitHubAPI but accepts a reviewDecision for
// the GraphQL response (empty string = null, i.e. no review rule configured)
// and a graphQLTotal override on the contexts connection (pass -1 to auto-use
// the node count; larger values simulate a truncated/paginated response).
func fakeGitHubAPIFull(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun, reviewDecision string, graphQLTotal int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "POST" && r.URL.Path == "/graphql":
			resp := buildGraphQLResponse(statuses, checkRuns, reviewDecision, graphQLTotal)
			writeJSON(t, w, resp)

		// IsPullRequest calls Issues.Get
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/issues/1":
			writeJSON(t, w, issue)

		// InspectMergeability calls PullRequests.Get
		case r.Method == "GET" && r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			writeJSON(t, w, pr)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// newGraphQLOverrideHandler serves the given graphqlHandler for POST /graphql
// and the standard Issues.Get / PullRequests.Get fixtures for other paths.
// Tests that need to exercise GraphQL-transport edge cases (malformed JSON,
// error arrays, non-200 responses, timeouts) use this to vary only the
// GraphQL behavior without restating the routing boilerplate.
func newGraphQLOverrideHandler(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, graphqlHandler http.HandlerFunc) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			graphqlHandler(w, r)
		case r.URL.Path == "/repos/testowner/testrepo/issues/1":
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, issue)
		case r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, pr)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// buildGraphQLResponse constructs a canned GraphQL response matching the
// bypassQuery shape from the same test data used for REST endpoints. Pass
// totalCount=-1 to auto-use len(nodes) (no truncation); a value greater
// than len(nodes) simulates a truncated (paginated) response.
func buildGraphQLResponse(statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun, reviewDecision string, totalCount int) map[string]any {
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

	reportedTotal := totalCount
	if reportedTotal < 0 {
		reportedTotal = len(nodes)
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
											"totalCount": reportedTotal,
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

// These tests exercise InspectMergeability directly — the GitHub-specific
// capability that reports raw mergeability state. Policy tests for the
// IsMergeable wrapper (which decides whether to accept a blocked-only-by-
// digger/apply state based on FailingChecks, ReviewsBlocking, Truncated,
// etc.) live in mergeable_test.go alongside the function they test.

// TestInspectMergeability_Truncation_ReportsTruncated verifies that when the
// GraphQL response indicates the check rollup was truncated, the returned
// MergeabilityState carries Truncated=true and Blocked=true — giving the
// caller enough information to refuse bypass without conflating truncation
// with "no failures".
func TestInspectMergeability_Truncation_ReportsTruncated(t *testing.T) {
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	cases := []struct {
		name       string
		statuses   []*gh.RepoStatus
		checkRuns  []*gh.CheckRun
		totalCount int
	}{
		{
			name:       "statuses only",
			statuses:   statuses,
			checkRuns:  nil,
			totalCount: 5,
		},
		{
			name:     "statuses and check runs",
			statuses: statuses,
			checkRuns: []*gh.CheckRun{
				{Name: gh.String("ci/build"), Status: gh.String("completed"), Conclusion: gh.String("success")},
			},
			totalCount: 151,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pr := makePR("blocked", false)
			svc, server := newTestGithubService(t,
				fakeGitHubAPIFull(t, pr, makeIssue(), c.statuses, c.checkRuns, "", c.totalCount))
			defer server.Close()

			state, err := svc.InspectMergeability(1)
			assert.NoError(t, err)
			assert.True(t, state.Truncated,
				"truncation must be signalled to the caller so it can refuse bypass")
			assert.True(t, state.Blocked,
				"Truncated is only meaningful when the PR is blocked")
			assert.False(t, state.Mergeable)
		})
	}
}

// TestInspectMergeability_TruncatedAndReviewsRequired verifies that when the
// check rollup is truncated AND the PR also has a non-APPROVED ReviewDecision,
// both flags are surfaced on the returned state. Regression guard: the
// truncation branch previously hardcoded ReviewsBlocking=false, which was a
// correctness lie about the state — even though the call chain happened to
// refuse bypass for other reasons.
func TestInspectMergeability_TruncatedAndReviewsRequired(t *testing.T) {
	pr := makePR("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPIFull(t, pr, makeIssue(), statuses, nil, "REVIEW_REQUIRED", 42))
	defer server.Close()

	state, err := svc.InspectMergeability(1)
	assert.NoError(t, err)
	assert.True(t, state.Blocked, "PR is blocked")
	assert.True(t, state.Truncated, "upstream response was truncated")
	assert.True(t, state.ReviewsBlocking,
		"REVIEW_REQUIRED must surface as ReviewsBlocking=true even when truncated")
	assert.False(t, state.Mergeable)
	assert.Nil(t, state.FailingChecks,
		"FailingChecks must be nil on the truncated branch (list is incomplete)")
}

// TestInspectMergeability_ReviewDecisionAllowlist verifies that the
// ReviewsBlocking flag is only clear when the review decision is APPROVED or
// empty (no policy). Every other value — including unknown future GitHub enum
// additions — must surface as blocking so the policy wrapper fails closed.
func TestInspectMergeability_ReviewDecisionAllowlist(t *testing.T) {
	cases := []struct {
		decision            string
		wantReviewsBlocking bool
		reason              string
	}{
		{"REVIEW_REQUIRED", true, "reviews required — block is not from checks"},
		{"CHANGES_REQUESTED", true, "changes requested"},
		{"SOME_FUTURE_STATE", true, "unknown reviewDecision must fail closed (allowlist)"},
		{"APPROVED", false, "reviews approved — ReviewsBlocking should be false"},
	}
	for _, c := range cases {
		t.Run(c.decision, func(t *testing.T) {
			pr := makePR("blocked", false)
			statuses := []*gh.RepoStatus{
				{Context: gh.String("digger/apply"), State: gh.String("pending")},
				{Context: gh.String("ci/build"), State: gh.String("success")},
			}
			svc, server := newTestGithubService(t,
				fakeGitHubAPIFull(t, pr, makeIssue(), statuses, nil, c.decision, -1))
			defer server.Close()

			state, err := svc.InspectMergeability(1)
			assert.NoError(t, err)
			assert.Equal(t, c.wantReviewsBlocking, state.ReviewsBlocking, c.reason)
		})
	}
}

// TestInspectMergeability_EmptyToken_ReturnsError verifies that
// InspectMergeability fails fast with a descriptive error when Token is not
// populated. This surfaces provisioning bugs in call sites that construct
// GithubService without wiring the token, rather than silently degrading to a
// path that cannot see reviewDecision.
func TestInspectMergeability_EmptyToken_ReturnsError(t *testing.T) {
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

	state, err := svc.InspectMergeability(1)
	assert.Error(t, err, "expected fail-fast error when Token is empty")
	assert.Equal(t, MergeabilityState{}, state)
	assert.Contains(t, err.Error(), "Token",
		"error should name the missing field to aid diagnosis")
}

// TestInspectMergeability_GraphQLMalformedJSON_ReturnsError verifies that an
// upstream returning bytes that are not valid JSON (e.g. a gateway HTML page
// that slipped past the status check, or a corrupted proxy response) surfaces
// an error instead of silently treating the response as empty.
func TestInspectMergeability_GraphQLMalformedJSON_ReturnsError(t *testing.T) {
	pr := makePR("blocked", false)
	handler := newGraphQLOverrideHandler(t, pr, makeIssue(),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{not-valid-json"))
		})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	state, err := svc.InspectMergeability(1)
	assert.Error(t, err, "expected unmarshal error from malformed JSON")
	assert.Equal(t, MergeabilityState{}, state)
	assert.Contains(t, err.Error(), "unmarshal",
		"error should identify the parse failure")
}

// TestInspectMergeability_NotAPullRequest_ReturnsMergeable verifies the
// IsPullRequest=false path: an issue (not a PR) is "mergeable" for workflow
// purposes, since there is nothing to block. Locks in the capability's
// contract for issues.
func TestInspectMergeability_NotAPullRequest_ReturnsMergeable(t *testing.T) {
	// An Issue without PullRequestLinks is the "not a PR" case.
	bareIssue := &gh.Issue{}
	pr := makePR("blocked", false) // PR fixture used only if path falls through
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/testowner/testrepo/issues/1":
			writeJSON(t, w, bareIssue)
		case r.URL.Path == "/repos/testowner/testrepo/pulls/1":
			t.Errorf("PullRequests.Get should not be called for an issue")
			writeJSON(t, w, pr)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	state, err := svc.InspectMergeability(1)
	assert.NoError(t, err)
	assert.True(t, state.Mergeable, "issues should be treated as mergeable (closable)")
}

// TestInspectMergeability_GraphQLMultipleErrors_AllSurfaced verifies that
// when the GraphQL response carries multiple errors (one per failed field
// path, mixed auth/rate-limit/deprecation signals), every message is included
// in the wrapped error rather than only the first.
func TestInspectMergeability_GraphQLMultipleErrors_AllSurfaced(t *testing.T) {
	pr := makePR("blocked", false)
	handler := newGraphQLOverrideHandler(t, pr, makeIssue(),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{
				"data": nil,
				"errors": []map[string]any{
					{"message": "rate limit exceeded"},
					{"message": "field statusCheckRollup is deprecated"},
					{"message": "permission denied on reviewDecision"},
				},
			})
		})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	state, err := svc.InspectMergeability(1)
	assert.Error(t, err)
	assert.Equal(t, MergeabilityState{}, state)
	assert.Contains(t, err.Error(), "rate limit exceeded")
	assert.Contains(t, err.Error(), "deprecated")
	assert.Contains(t, err.Error(), "permission denied",
		"all GraphQL errors must be surfaced, not just the first")
}

// TestInspectMergeability_GraphQLNon200_TruncatesErrorBody verifies that a
// large upstream error page (as GHE edge proxies can return) is capped in the
// wrapped error rather than propagated verbatim through logs and Sentry.
func TestInspectMergeability_GraphQLNon200_TruncatesErrorBody(t *testing.T) {
	pr := makePR("blocked", false)
	hugeBody := make([]byte, 4096)
	for i := range hugeBody {
		hugeBody[i] = 'X'
	}
	handler := newGraphQLOverrideHandler(t, pr, makeIssue(),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			w.Write(hugeBody)
		})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	state, err := svc.InspectMergeability(1)
	assert.Error(t, err)
	assert.Equal(t, MergeabilityState{}, state)
	assert.Contains(t, err.Error(), "502", "error should name the status code")
	assert.Contains(t, err.Error(), "[truncated]",
		"error should signal that the body was truncated")
	// Cap check: 512 body bytes + ~80 bytes of wrapping prefix. Well under 4096.
	assert.Less(t, len(err.Error()), 1024,
		"truncated error message should not carry the full 4KB body")
}

// TestInspectMergeability_GraphQLTimeout_ReturnsError verifies that a hung
// GitHub endpoint is bounded by the injected HTTPClient's timeout rather than
// wedging the caller forever.
func TestInspectMergeability_GraphQLTimeout_ReturnsError(t *testing.T) {
	pr := makePR("blocked", false)
	handler := newGraphQLOverrideHandler(t, pr, makeIssue(),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			time.Sleep(500 * time.Millisecond)
			// Response after sleep is irrelevant — client will have cancelled.
			w.WriteHeader(http.StatusOK)
		})
	svc, server := newTestGithubService(t, handler)
	defer server.Close()

	// Inject a short-timeout client on the service itself — no shared global
	// to race on if this test is ever run in parallel with others.
	svc.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}

	state, err := svc.InspectMergeability(1)
	assert.Error(t, err, "expected timeout error from slow GraphQL endpoint")
	assert.Equal(t, MergeabilityState{}, state)
}

// TestInspectMergeability_FailingChecks_IdentifiesDiggerApply covers the
// end-to-end happy path that #1180 exists to unlock: a blocked PR where
// digger/apply appears in the rollup as a failing check gets surfaced in
// FailingChecks with that exact name, so the policy wrapper can match it
// against the self-blocking allowlist.
//
// Each subcase covers a different shape GitHub's statusCheckRollup can
// deliver digger/apply in — classic commit status (StatusContext), GitHub
// Apps-style CheckRun while a prior run is still in flight, and CheckRun
// after a completed FAILURE. The CheckRun paths are the primary real-world
// delivery in the chicken-and-egg scenario and were previously only
// covered indirectly through the policy-layer fake.
func TestInspectMergeability_FailingChecks_IdentifiesDiggerApply(t *testing.T) {
	cases := []struct {
		name      string
		statuses  []*gh.RepoStatus
		checkRuns []*gh.CheckRun
	}{
		{
			name: "digger/apply as StatusContext (PENDING)",
			statuses: []*gh.RepoStatus{
				{Context: gh.String("digger/apply"), State: gh.String("pending")},
			},
		},
		{
			name: "digger/apply as CheckRun (in-flight, no conclusion)",
			checkRuns: []*gh.CheckRun{
				{Name: gh.String("digger/apply"), Status: gh.String("in_progress")},
			},
		},
		{
			name: "digger/apply as CheckRun (completed FAILURE)",
			checkRuns: []*gh.CheckRun{
				{Name: gh.String("digger/apply"), Status: gh.String("completed"), Conclusion: gh.String("failure")},
			},
		},
		{
			name: "digger/apply as CheckRun alongside a passing StatusContext",
			statuses: []*gh.RepoStatus{
				{Context: gh.String("ci/build"), State: gh.String("success")},
			},
			checkRuns: []*gh.CheckRun{
				{Name: gh.String("digger/apply"), Status: gh.String("in_progress")},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pr := makePR("blocked", false)
			svc, server := newTestGithubService(t,
				fakeGitHubAPI(t, pr, makeIssue(), c.statuses, c.checkRuns))
			defer server.Close()

			state, err := svc.InspectMergeability(1)
			assert.NoError(t, err)
			assert.True(t, state.Blocked)
			assert.False(t, state.Truncated)
			assert.False(t, state.ReviewsBlocking)
			assert.Equal(t, []string{"digger/apply"}, state.FailingChecks,
				"digger/apply must land in FailingChecks under its canonical name so the policy wrapper can match it against the self-blocking allowlist")
		})
	}
}

// TestInspectMergeability_FailingChecks_MixedBlockers verifies that when a
// non-self-blocking check is failing alongside digger/apply, both names
// surface in FailingChecks. The policy wrapper's job is then to refuse
// bypass because FailingChecks is not a subset of the self-blocking list —
// but that decision needs both names to be visible to make it.
func TestInspectMergeability_FailingChecks_MixedBlockers(t *testing.T) {
	pr := makePR("blocked", false)
	checkRuns := []*gh.CheckRun{
		{Name: gh.String("digger/apply"), Status: gh.String("in_progress")},
		{Name: gh.String("ci/build"), Status: gh.String("completed"), Conclusion: gh.String("failure")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssue(), nil, checkRuns))
	defer server.Close()

	state, err := svc.InspectMergeability(1)
	assert.NoError(t, err)
	assert.True(t, state.Blocked)
	assert.ElementsMatch(t, []string{"digger/apply", "ci/build"}, state.FailingChecks,
		"both the self-blocking check and the real blocker must surface so the policy wrapper can refuse bypass")
}
