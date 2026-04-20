package apply_requirements

import (
	"fmt"
	"testing"

	dgh "github.com/diggerhq/digger/libs/ci/github"
	"github.com/diggerhq/digger/libs/digger_config"
	"github.com/diggerhq/digger/libs/scheduler"
	"github.com/stretchr/testify/assert"
)

// bypassFakeService embeds the github package's MockCiService to satisfy the
// ci.PullRequestService interface, then overrides the bypass-relevant methods
// (InspectMergeability, GetApprovals, IsDivergedFromBranch) with
// test-controllable fakes. Embedding MockCiService (rather than restating the
// ~20-method interface) keeps this file focused on the handful of methods
// CheckApplyRequirements actually exercises.
//
// By implementing InspectMergeability, this type satisfies the
// dgh.BlockedMergeInspector capability — which is how the bypass
// logic decides whether to consult us instead of falling back to IsMergeable.
type bypassFakeService struct {
	dgh.MockCiService

	// inspectResult / inspectErr are what InspectMergeability returns.
	inspectResult dgh.MergeabilityState
	inspectErr    error

	// approvals is what GetApprovals returns.
	approvals []string

	// diverged is what IsDivergedFromBranch returns.
	diverged bool
}

func (f *bypassFakeService) InspectMergeability(prNumber int) (dgh.MergeabilityState, error) {
	return f.inspectResult, f.inspectErr
}

func (f *bypassFakeService) GetApprovals(prNumber int) ([]string, error) {
	return f.approvals, nil
}

func (f *bypassFakeService) IsDivergedFromBranch(sourceBranch, targetBranch string) (bool, error) {
	return f.diverged, nil
}

// mergeableProject is a minimal project fixture that requires mergeability.
func mergeableProject() digger_config.Project {
	return digger_config.Project{
		Name:              "proj",
		ApplyRequirements: []string{digger_config.ApplyRequirementsMergeable},
	}
}

func TestCheckApplyRequirements_MergeableShortCircuitsToApproval(t *testing.T) {
	svc := &bypassFakeService{
		inspectResult: dgh.MergeabilityState{Mergeable: true},
		approvals:     []string{"reviewer"},
	}
	err := CheckApplyRequirements(svc, []digger_config.Project{mergeableProject()}, nil, 1, "feat", "main")
	assert.NoError(t, err,
		"a mergeable PR should satisfy the mergeability requirement")
}

func TestCheckApplyRequirements_BypassFiresWhenOnlyBlockerIsDiggerApply(t *testing.T) {
	svc := &bypassFakeService{
		inspectResult: dgh.MergeabilityState{
			Blocked:       true,
			FailingChecks: []string{"digger/apply"},
		},
		approvals: []string{"reviewer"},
	}
	err := CheckApplyRequirements(svc, []digger_config.Project{mergeableProject()}, nil, 1, "feat", "main")
	assert.NoError(t, err,
		"bypass should fire when digger/apply is the sole failing check, "+
			"resolving the chicken-and-egg this package exists to solve")
}

func TestCheckApplyRequirements_BlockedByOtherCheckFailsMergeability(t *testing.T) {
	svc := &bypassFakeService{
		inspectResult: dgh.MergeabilityState{
			Blocked:       true,
			FailingChecks: []string{"digger/apply", "ci/build"},
		},
		approvals: []string{"reviewer"},
	}
	err := CheckApplyRequirements(svc, []digger_config.Project{mergeableProject()}, nil, 1, "feat", "main")
	assert.Error(t, err,
		"bypass must not fire when a non-self-blocking check is also failing")
	assert.Contains(t, err.Error(), "PR to be mergable",
		"error should explain the mergeability requirement failure")
}

func TestCheckApplyRequirements_SkipMergeCheckBypassesMergeabilityRequirement(t *testing.T) {
	// Inspector reports a genuine block with non-digger/apply failures;
	// SkipMergeCheck on the job should let the apply proceed anyway.
	svc := &bypassFakeService{
		inspectResult: dgh.MergeabilityState{
			Blocked:       true,
			FailingChecks: []string{"ci/build"},
		},
		approvals: []string{"reviewer"},
	}
	jobs := []scheduler.Job{{ProjectName: "proj", SkipMergeCheck: true}}
	err := CheckApplyRequirements(svc, []digger_config.Project{mergeableProject()}, jobs, 1, "feat", "main")
	assert.NoError(t, err,
		"skip_merge_check on the project's job should bypass the mergeability requirement")
}

func TestCheckApplyRequirements_InspectMergeabilityErrorIsSurfaced(t *testing.T) {
	svc := &bypassFakeService{
		inspectErr: fmt.Errorf("upstream API boom"),
		approvals:  []string{"reviewer"},
	}
	err := CheckApplyRequirements(svc, []digger_config.Project{mergeableProject()}, nil, 1, "feat", "main")
	assert.Error(t, err)
	assert.ErrorContains(t, err, "upstream API boom",
		"the underlying error must be wrapped, not swallowed, so operators can see the cause")
}

func TestIgnoreMergeabilityForProject(t *testing.T) {
	proj := digger_config.Project{Name: "proj"}
	t.Run("skip_merge_check true", func(t *testing.T) {
		jobs := []scheduler.Job{{ProjectName: "proj", SkipMergeCheck: true}}
		assert.True(t, IgnoreMergeabilityForProject(proj, jobs))
	})
	t.Run("skip_merge_check false", func(t *testing.T) {
		jobs := []scheduler.Job{{ProjectName: "proj", SkipMergeCheck: false}}
		assert.False(t, IgnoreMergeabilityForProject(proj, jobs))
	})
	t.Run("no job for project falls back to false", func(t *testing.T) {
		jobs := []scheduler.Job{{ProjectName: "other", SkipMergeCheck: true}}
		assert.False(t, IgnoreMergeabilityForProject(proj, jobs),
			"when no job matches the project, the check must NOT be skipped "+
				"(fail-closed so a misconfigured workflow doesn't silently bypass review gates)")
	})
}
