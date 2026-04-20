package apply_requirements

import (
	"fmt"
	"log/slog"

	"github.com/diggerhq/digger/libs/ci"
	dgh "github.com/diggerhq/digger/libs/ci/github"
	"github.com/diggerhq/digger/libs/digger_config"
	"github.com/diggerhq/digger/libs/scheduler"
)

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
	// The bypass policy (self-blocking check name, truncation handling,
	// review-decision guard) lives with the provider in libs/ci/github;
	// here we just consume the boolean verdict.
	isMergeable, err := dgh.IsMergeable(ghService, prNumber)
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
