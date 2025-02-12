package bugzilla

import (
	"fmt"
	"strings"

	releasecontroller "github.com/openshift/release-controller/pkg/release-controller"
	"k8s.io/klog"
	"k8s.io/test-infra/prow/bugzilla"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
)

// Verifier takes a list of bugzilla bugs and uses the Bugzilla client to
// retrieve the associated GitHub PR via the bugzilla bug's external bug links.
// It then uses the github client to read the comments of the associated PR to
// determine whether the bug's QA Contact reviewed the GitHub PR. If yes, the bug
// gets marked as VERIFIED in Bugzilla.
type Verifier struct {
	// bzClient is used to retrieve external bug links and mark QA reviewed bugs as VERIFIED
	bzClient bugzilla.Client
	// ghClient is used to retrieve comments on a bug's PR
	ghClient github.Client
	// pluginConfig is used to check whether a repository allows approving reviews as LGTM
	pluginConfig *plugins.Configuration
}

// NewVerifier returns a Verifier configured with the provided github and bugzilla clients and the provided pluginConfig
func NewVerifier(bzClient bugzilla.Client, ghClient github.Client, pluginConfig *plugins.Configuration) *Verifier {
	return &Verifier{
		bzClient:     bzClient,
		ghClient:     ghClient,
		pluginConfig: pluginConfig,
	}
}

// pr contains the org, repo, and pr number for a pr
type pr struct {
	org   string
	repo  string
	prNum int
}

// VerifyBugs takes a list of bugzilla bug IDs and for each bug changes the bug status to VERIFIED if bug was reviewed and
// lgtm'd by the bug's QA Contect
func (c *Verifier) VerifyBugs(bugs []int, tagName string) []error {
	tagSemVer, err := releasecontroller.SemverParseTolerant(tagName)
	if err != nil {
		return []error{fmt.Errorf("failed to parse tag `%s` semver: %w", tagName, err)}
	}
	tagRelease := releasecontroller.SemverToMajorMinor(tagSemVer)
	bzPRs, errs := getPRs(bugs, c.bzClient)
	for bugID, extPRs := range bzPRs {
		bug, err := c.bzClient.GetBug(bugID)
		if err != nil {
			errs = append(errs, fmt.Errorf("Unable to get bugzilla number %d: %v", bugID, err))
			continue
		}
		// bugzilla usually denotes unset target releases with `---`
		if len(bug.TargetRelease) == 0 || bug.TargetRelease[0] == "---" {
			klog.Warningf("Bug %d does not have a target release", bug.ID)
			continue
		}
		// the format for target release is always `int.int.{0,z}`
		bugSplitVer := strings.Split(bug.TargetRelease[0], ".")
		if len(bugSplitVer) < 2 {
			errs = append(errs, fmt.Errorf("Bug %d: length of target release `%s` after split by `.` is less than 2", bug.ID, bug.TargetRelease[0]))
			continue
		}
		bugRelease := fmt.Sprintf("%s.%s", bugSplitVer[0], bugSplitVer[1])
		if bugRelease != tagRelease {
			// bugfix included in different release than target; ignore
			klog.Infof("Bug %d is in different release (%s) than tag %s", bug.ID, bugRelease, tagName)
			continue
		}
		var success bool
		message := fmt.Sprintf("Bugfix included in accepted release %s", tagName)
		var unlabeledPRs []pr
		var bugErrs []error
		if bug.Status != "ON_QA" {
			// In case bug has already been moved to VERIFIED, completely ignore
			if bug.Status == "VERIFIED" {
				klog.V(4).Infof("Bug %d already in VERIFIED status", bug.ID)
				continue
			} else {
				bugErrs = append(bugErrs, fmt.Errorf("Bug is not in ON_QA status"))
			}
		} else {
			for _, extPR := range extPRs {
				labels, err := c.ghClient.GetIssueLabels(extPR.org, extPR.repo, extPR.prNum)
				if err != nil {
					newErr := fmt.Errorf("Unable to get labels for github pull %s/%s#%d: %v", extPR.org, extPR.repo, extPR.prNum, err)
					errs = append(errs, newErr)
					bugErrs = append(bugErrs, newErr)
				}
				var hasLabel bool
				for _, label := range labels {
					if label.Name == "qe-approved" {
						hasLabel = true
						break
					}
				}
				if !hasLabel {
					unlabeledPRs = append(unlabeledPRs, extPR)
				}
			}
		}
		if len(unlabeledPRs) > 0 || len(bugErrs) > 0 {
			message = fmt.Sprintf("%s\nBug will not be automatically moved to VERIFIED for the following reasons:", message)
			for _, extPR := range unlabeledPRs {
				message = fmt.Sprintf("%s\n- PR %s/%s#%d not approved by QA contact", message, extPR.org, extPR.repo, extPR.prNum)
			}
			for _, err := range bugErrs {
				message = fmt.Sprintf("%s\n- %s", message, err)
			}
			message = fmt.Sprintf("%s\n\nThis bug must now be manually moved to VERIFIED", message)
			// Sometimes the QAContactDetail is nil; if not nil, include name of QA contact in message
			if bug.QAContactDetail != nil {
				message = fmt.Sprintf("%s by %s", message, bug.QAContactDetail.Name)
			}
		} else {
			success = true
		}
		if success {
			message = fmt.Sprintf("%s\nAll linked GitHub PRs have been approved by a QA contact; updating bug status to VERIFIED", message)
		}
		if message != "" {
			comments, err := c.bzClient.GetComments(bugID)
			if err != nil {
				errs = append(errs, fmt.Errorf("Failed to get comments on bug %d: %v", bug.ID, err))
				continue
			}
			var alreadyCommented bool
			for _, comment := range comments {
				if comment.Text == message && (comment.Creator == "openshift-bugzilla-robot" || comment.Creator == "openshift-bugzilla-robot@redhat.com") {
					alreadyCommented = true
					break
				}
			}
			if !alreadyCommented {
				if _, err := c.bzClient.CreateComment(&bugzilla.CommentCreate{ID: bugID, Comment: message, IsPrivate: true}); err != nil {
					errs = append(errs, fmt.Errorf("Failed to comment on bug %d: %v", bug.ID, err))
				}
			}
		}
		if success {
			klog.V(4).Infof("Updating bug %d (current status %s) to VERIFIED status", bug.ID, bug.Status)
			if err := c.bzClient.UpdateBug(bug.ID, bugzilla.BugUpdate{Status: "VERIFIED"}); err != nil {
				errs = append(errs, fmt.Errorf("Failed to update status for bug %d: %v", bug.ID, err))
			}
		} else {
			klog.V(4).Infof("Bug %d (current status %s) not approved by QA contact", bug.ID, bug.Status)
		}
	}
	return errs
}

// getPRs identifies bugzilla bugs and the associated github PRs fixed in a release from
// a given buglist generated by `oc adm release info --bugs=git-cache-path --ouptut=name from-tag to-tag`
func getPRs(input []int, bzClient bugzilla.Client) (map[int][]pr, []error) {
	bzPRs := make(map[int][]pr)
	var errs []error
	for _, bzID := range input {
		extBugs, err := bzClient.GetExternalBugPRsOnBug(bzID)
		if err != nil {
			// there are a couple of bugs with weird permissions issues that can cause this to fail; simply log instead of generating error
			if bugzilla.IsAccessDenied(err) {
				klog.V(4).Infof("Access denied getting external bugs for bugzilla bug %d: %v", bzID, err)
			} else {
				errs = append(errs, fmt.Errorf("Failed to get external bugs for bugzilla bug %d: %v", bzID, err))
			}
			continue
		}
		foundPR := false
		for _, extBug := range extBugs {
			if extBug.Type.URL == "https://github.com/" {
				if existingPRs, ok := bzPRs[bzID]; ok {
					bzPRs[bzID] = append(existingPRs, pr{org: extBug.Org, repo: extBug.Repo, prNum: extBug.Num})
				} else {
					bzPRs[bzID] = []pr{{org: extBug.Org, repo: extBug.Repo, prNum: extBug.Num}}
				}
				foundPR = true
			}
		}
		if !foundPR {
			// sometimes people ignore the bot and manually change the bugzilla tags, resulting in a bug not being linked; ignore these
			klog.V(5).Infof("Failed to identify associated GitHub PR for bugzilla bug %d", bzID)
		}
	}
	return bzPRs, errs
}
