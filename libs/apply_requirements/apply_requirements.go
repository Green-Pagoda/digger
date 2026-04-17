package apply_requirements

import (
	"fmt"
	"github.com/diggerhq/digger/libs/ci"
	digger_github "github.com/diggerhq/digger/libs/ci/github"
	"github.com/diggerhq/digger/libs/digger_config"
	"github.com/diggerhq/digger/libs/scheduler"
	"log/slog"
)

// IsMergeableForApply returns true when the PR is mergeable, OR when the only
// failing check is digger/apply itself — breaking the chicken-and-egg where
// the apply check is configured as a required status check on its own PR.
// Falls back to standard IsMergeable for providers without the
// BlockedMergeInspector capability.
func IsMergeableForApply(svc ci.PullRequestService, prNumber int) (bool, error) {
	inspector, ok := svc.(digger_github.BlockedMergeInspector)
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
		return false, fmt.Errorf("InspectMergeability for PR %d: %w", prNumber, err)
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

	const diggerApplyCheck = "digger/apply"
	foundSelfBlocker := false
	for _, name := range state.FailingChecks {
		if name != diggerApplyCheck {
			return false, nil
		}
		foundSelfBlocker = true
	}
	// Refuse to bypass when no self-blocker was actually present — the block
	// must be caused by something we cannot resolve (signed commits,
	// unresolved conversations, etc.).
	return foundSelfBlocker, nil
}

// IgnoreMergeabilityForProject will strip out the 'mergeability' requirement if
// the project's workflow has specified skip_merge_check: true
func IgnoreMergeabilityForProject(project digger_config.Project, jobs []scheduler.Job) bool {
	job, err := scheduler.JobForProjectName(jobs, project.Name)
	if err != nil {
		slog.Warn("could not find job for mergeability ignore check, returning false",
			"project", project.Name, "error", err)
		return false
	}
	return job.SkipMergeCheck
}
func CheckApplyRequirements(ghService ci.PullRequestService, impactedProjects []digger_config.Project, jobs []scheduler.Job, prNumber int, sourceBranch string, targetBranch string) error {
	// Bypass the chicken-and-egg where the apply check itself blocks the PR.
	// The check-name policy lives in this package; the CI provider only
	// reports raw mergeability state.
	isMergeable, err := IsMergeableForApply(ghService, prNumber)
	if err != nil {
		slog.Error("Error checking if PR is mergeable", "prNumber", prNumber, "error", err)
		return fmt.Errorf("error checking if PR is mergeable: %w", err)
	}
	approvals, err := ghService.GetApprovals(prNumber)
	if err != nil {
		slog.Error("Error getting approvals", "prNumber", prNumber, "error", err)
		return fmt.Errorf("error getting approvals: %w", err)
	}
	isApproved := len(approvals) > 0
	isDiverged, err := ghService.IsDivergedFromBranch(sourceBranch, targetBranch)
	if err != nil {
		slog.Error("Error checking if PR is diverged", "prNumber", prNumber, "error", err)
		return fmt.Errorf("error checking if PR is diverged: %w", err)
	}
	for _, proj := range impactedProjects {
		for _, req := range proj.ApplyRequirements {
			ignoreMergeability := IgnoreMergeabilityForProject(proj, jobs)
			switch req {
			case digger_config.ApplyRequirementsApproved:
				if !isApproved {
					return fmt.Errorf("PR fails apply requirements for project %v, Expected PR to be approved, a minimum of one approval is required before proceeding", proj.Name)
				}
			case digger_config.ApplyRequirementsUndiverged:
				if isDiverged {
					return fmt.Errorf("PR fails apply requirements for project %v, Expected PR to be undiverged from target branch. Merge main into the PR branch or rebase the PR branch on top of main", proj.Name)
				}
			case digger_config.ApplyRequirementsMergeable:
				if !isMergeable && !ignoreMergeability {
					return fmt.Errorf("PR fails apply requirements for project %v, Expected PR to be mergable. Ensure all status checks are successful in order to proceed", proj.Name)
				}
			default:
				slog.Warn("unknown apply requirements found", "project", proj.Name, "requirement", req)
			}
		}
	}
	return nil
}
