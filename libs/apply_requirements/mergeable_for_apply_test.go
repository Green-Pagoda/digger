package apply_requirements

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/diggerhq/digger/libs/ci"
	digger_github "github.com/diggerhq/digger/libs/ci/github"
	gh "github.com/google/go-github/v61/github"
	"github.com/stretchr/testify/assert"
)

// TestIsMergeableForApply exercises the policy wrapper as a pure function of
// the MergeabilityState a BlockedMergeInspector would return. HTTP-level
// behavior (GraphQL errors, truncation detection, review-decision parsing) is
// covered in libs/ci/github where InspectMergeability is tested directly;
// here we only verify that the wrapper turns a given state into the correct
// bypass decision.
func TestIsMergeableForApply(t *testing.T) {
	selfBlocking := []string{"digger/apply"}

	cases := []struct {
		name       string
		state      digger_github.MergeabilityState
		want       bool
		wantErr    bool
		errContain string
		reason     string
	}{
		{
			name:   "mergeable PR short-circuits to true",
			state:  digger_github.MergeabilityState{Mergeable: true},
			want:   true,
			reason: "clean PR should be mergeable without inspecting the other fields",
		},
		{
			name: "blocked only by digger/apply bypasses",
			state: digger_github.MergeabilityState{
				Blocked:       true,
				FailingChecks: []string{"digger/apply"},
			},
			want: true,
			reason: "bypass must fire when digger/apply is the sole failing check — " +
				"this is the chicken-and-egg the function exists to resolve",
		},
		{
			name: "blocked with digger/apply plus another failing check does not bypass",
			state: digger_github.MergeabilityState{
				Blocked:       true,
				FailingChecks: []string{"digger/apply", "ci/build"},
			},
			want:   false,
			reason: "bypass must NOT fire when a non-self-blocking check is also failing",
		},
		{
			name: "blocked by other check (no digger/apply) does not bypass",
			state: digger_github.MergeabilityState{
				Blocked:       true,
				FailingChecks: []string{"ci/lint"},
			},
			want:   false,
			reason: "bypass only fires when the self-blocker is actually present",
		},
		{
			name: "blocked with no failing checks does not bypass",
			state: digger_github.MergeabilityState{
				Blocked:       true,
				FailingChecks: nil,
			},
			want: false,
			reason: "block must be caused by non-check requirements (signed commits, " +
				"unresolved conversations) — cannot be resolved by re-running a check",
		},
		{
			name: "dirty state (not blocked) returns false",
			state: digger_github.MergeabilityState{
				Mergeable: false,
				Blocked:   false,
			},
			want:   false,
			reason: "dirty/behind/unknown states require human intervention",
		},
		{
			name: "reviews blocking overrides check-level bypass",
			state: digger_github.MergeabilityState{
				Blocked:         true,
				ReviewsBlocking: true,
				FailingChecks:   []string{"digger/apply"},
			},
			want:   false,
			reason: "no status-check workflow can unblock a PR that needs reviews",
		},
		{
			name: "truncated state surfaces error",
			state: digger_github.MergeabilityState{
				Blocked:   true,
				Truncated: true,
			},
			want:       false,
			wantErr:    true,
			errContain: "truncated",
			reason: "callers cannot prove 'only blocker is self-blocking' when the " +
				"check list is truncated — must surface as error, not silently fail",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &bypassFakeService{inspectResult: c.state}
			got, err := IsMergeableForApply(svc, 1, selfBlocking)
			if c.wantErr {
				assert.Error(t, err, c.reason)
				if c.errContain != "" {
					assert.Contains(t, err.Error(), c.errContain)
				}
			} else {
				assert.NoError(t, err, c.reason)
			}
			assert.Equal(t, c.want, got, c.reason)
		})
	}
}

// TestIsMergeableForApply_InspectorErrorIsSurfaced verifies that an error
// from InspectMergeability is propagated to the caller rather than swallowed.
func TestIsMergeableForApply_InspectorErrorIsSurfaced(t *testing.T) {
	svc := &bypassFakeService{inspectErr: fmt.Errorf("upstream API boom")}
	got, err := IsMergeableForApply(svc, 1, []string{"digger/apply"})
	assert.Error(t, err)
	assert.ErrorContains(t, err, "upstream API boom",
		"underlying error must reach the caller so operators see the cause")
	assert.False(t, got)
}

// TestIsMergeableForApply_FallsBackForNonGithub verifies that providers
// without the BlockedMergeInspector capability (GitLab, Bitbucket, Azure)
// fall back to plain IsMergeable. The chicken-and-egg this code resolves is
// GitHub-specific, so other providers are unaffected.
func TestIsMergeableForApply_FallsBackForNonGithub(t *testing.T) {
	mock := digger_github.MockCiService{CommentsPerPr: map[int][]*ci.Comment{}}
	result, err := IsMergeableForApply(&mock, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result, "should fall back to IsMergeable for non-GitHub providers")
}

// TestIsMergeableForApply_BypassRunsForValueTypedGithubService verifies that
// the BlockedMergeInspector capability interface is satisfied when a
// GithubService VALUE (not pointer) is boxed into ci.PullRequestService.
// This mirrors the spec-driven CLI path (libs/spec/providers.go) where
// GithubServiceProviderBasic.NewService returns a GithubService by value.
// A prior regression assumed pointer boxing and silently skipped the bypass
// on the CLI path — this test locks in dispatch for both forms.
//
// Uses a real GithubService backed by an HTTP fake (rather than the in-package
// bypassFakeService) because the point of the test is interface-dispatch
// through the actual GithubService type. Helpers below are copied from
// libs/ci/github/mergeable_bypass_test.go; update both places if the wire
// format changes.
func TestIsMergeableForApply_BypassRunsForValueTypedGithubService(t *testing.T) {
	pr := makePRForTest("blocked", false)
	statuses := []*gh.RepoStatus{
		{Context: gh.String("digger/apply"), State: gh.String("pending")},
		{Context: gh.String("digger/plan"), State: gh.String("success")},
	}
	svc, server := newTestGithubService(t,
		fakeGitHubAPI(t, pr, makeIssueForTest(), statuses, nil))
	defer server.Close()

	// Box the GithubService VALUE (not &svc) into the interface.
	var iface ci.PullRequestService = svc

	result, err := IsMergeableForApply(iface, 1, []string{"digger/apply"})
	assert.NoError(t, err)
	assert.True(t, result,
		"digger/apply bypass must run when GithubService is stored in "+
			"ci.PullRequestService as a value, not only as a pointer")
}

// ---------------------------------------------------------------------------
// HTTP fake helpers. Copied from libs/ci/github/mergeable_bypass_test.go for
// the one test that needs a real GithubService boxed through the interface.
// If the GitHub API wire format these fakes emulate changes, update both
// files. The duplication is deliberate — exporting test-only helpers from
// libs/ci/github would pollute its public API for a single caller here.
// ---------------------------------------------------------------------------

// newTestGithubService creates a GithubService backed by a fake HTTP server.
func newTestGithubService(t *testing.T, handler http.Handler) (digger_github.GithubService, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return digger_github.GithubService{
		Client:   client,
		Owner:    "testowner",
		RepoName: "testrepo",
		Token:    "test-token",
	}, server
}

// fakeGitHubAPI serves the minimal subset of GitHub endpoints
// InspectMergeability calls: GraphQL, Issues.Get, PullRequests.Get.
func fakeGitHubAPI(t *testing.T, pr *gh.PullRequest, issue *gh.Issue, statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/graphql":
			json.NewEncoder(w).Encode(buildGraphQLResponse(statuses, checkRuns))
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

// buildGraphQLResponse returns a canned no-truncation, no-review-policy
// response. The single value-typed dispatch test only cares that
// InspectMergeability parses a valid response; the richer fixture variants
// live in libs/ci/github alongside the tests that need them.
func buildGraphQLResponse(statuses []*gh.RepoStatus, checkRuns []*gh.CheckRun) map[string]any {
	var nodes []map[string]string
	for _, s := range statuses {
		nodes = append(nodes, map[string]string{
			"context": s.GetContext(),
			"state":   graphqlStateForTest(s.GetState()),
		})
	}
	for _, cr := range checkRuns {
		node := map[string]string{
			"name":   cr.GetName(),
			"status": "COMPLETED",
		}
		if cr.GetConclusion() != "" {
			node["conclusion"] = graphqlConclusionForTest(cr.GetConclusion())
		}
		nodes = append(nodes, node)
	}
	return map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviewDecision": nil,
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

func graphqlStateForTest(restState string) string {
	switch restState {
	case "success":
		return "SUCCESS"
	case "pending":
		return "PENDING"
	case "failure":
		return "FAILURE"
	default:
		return "UNKNOWN"
	}
}

func graphqlConclusionForTest(restConclusion string) string {
	switch restConclusion {
	case "success":
		return "SUCCESS"
	case "failure":
		return "FAILURE"
	default:
		return "UNKNOWN"
	}
}

func makePRForTest(mergeableState string, mergeable bool) *gh.PullRequest {
	return &gh.PullRequest{
		Mergeable:      gh.Bool(mergeable),
		MergeableState: gh.String(mergeableState),
		Head:           &gh.PullRequestBranch{SHA: gh.String("abc123")},
	}
}

func makeIssueForTest() *gh.Issue {
	return &gh.Issue{
		PullRequestLinks: &gh.PullRequestLinks{URL: gh.String("https://example.com")},
	}
}
