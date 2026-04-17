package github

import (
	"fmt"
	"testing"

	"github.com/diggerhq/digger/libs/ci"
	gh "github.com/google/go-github/v61/github"
	"github.com/stretchr/testify/assert"
)

// bypassFakeInspector is a minimal BlockedMergeInspector with test-controllable
// return values. It embeds MockCiService only to satisfy the broader
// ci.PullRequestService surface — the bypass logic only exercises
// InspectMergeability and IsMergeable.
type bypassFakeInspector struct {
	MockCiService
	inspectResult MergeabilityState
	inspectErr    error
}

func (f *bypassFakeInspector) InspectMergeability(prNumber int) (MergeabilityState, error) {
	return f.inspectResult, f.inspectErr
}

// TestIsMergeableForApply exercises the policy wrapper as a pure function of
// the MergeabilityState a BlockedMergeInspector would return. HTTP-level
// behavior (GraphQL errors, truncation detection, review-decision parsing) is
// covered in mergeable_bypass_test.go where InspectMergeability is tested
// directly; here we only verify that the wrapper turns a given state into the
// correct bypass decision.
func TestIsMergeableForApply(t *testing.T) {
	cases := []struct {
		name       string
		state      MergeabilityState
		want       bool
		wantErr    bool
		errContain string
		reason     string
	}{
		{
			name:   "mergeable PR short-circuits to true",
			state:  MergeabilityState{Mergeable: true},
			want:   true,
			reason: "clean PR should be mergeable without inspecting the other fields",
		},
		{
			name: "blocked only by digger/apply bypasses",
			state: MergeabilityState{
				Blocked:       true,
				FailingChecks: []string{"digger/apply"},
			},
			want: true,
			reason: "bypass must fire when digger/apply is the sole failing check — " +
				"this is the chicken-and-egg the function exists to resolve",
		},
		{
			name: "blocked with digger/apply plus another failing check does not bypass",
			state: MergeabilityState{
				Blocked:       true,
				FailingChecks: []string{"digger/apply", "ci/build"},
			},
			want:   false,
			reason: "bypass must NOT fire when a non-self-blocking check is also failing",
		},
		{
			name: "blocked by other check (no digger/apply) does not bypass",
			state: MergeabilityState{
				Blocked:       true,
				FailingChecks: []string{"ci/lint"},
			},
			want:   false,
			reason: "bypass only fires when the self-blocker is actually present",
		},
		{
			name: "blocked with no failing checks does not bypass",
			state: MergeabilityState{
				Blocked:       true,
				FailingChecks: nil,
			},
			want: false,
			reason: "block must be caused by non-check requirements (signed commits, " +
				"unresolved conversations) — cannot be resolved by re-running a check",
		},
		{
			name: "dirty state (not blocked) returns false",
			state: MergeabilityState{
				Mergeable: false,
				Blocked:   false,
			},
			want:   false,
			reason: "dirty/behind/unknown states require human intervention",
		},
		{
			name: "reviews blocking overrides check-level bypass",
			state: MergeabilityState{
				Blocked:         true,
				ReviewsBlocking: true,
				FailingChecks:   []string{"digger/apply"},
			},
			want:   false,
			reason: "no status-check workflow can unblock a PR that needs reviews",
		},
		{
			name: "truncated state surfaces error",
			state: MergeabilityState{
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
			svc := &bypassFakeInspector{inspectResult: c.state}
			got, err := IsMergeableForApply(svc, 1)
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
	svc := &bypassFakeInspector{inspectErr: fmt.Errorf("upstream API boom")}
	got, err := IsMergeableForApply(svc, 1)
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
	mock := MockCiService{CommentsPerPr: map[int][]*ci.Comment{}}
	result, err := IsMergeableForApply(&mock, 1)
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

	result, err := IsMergeableForApply(iface, 1)
	assert.NoError(t, err)
	assert.True(t, result,
		"digger/apply bypass must run when GithubService is stored in "+
			"ci.PullRequestService as a value, not only as a pointer")
}
