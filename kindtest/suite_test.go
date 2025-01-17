package kindtest

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v41/github"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	controllerNS = "meows"
	orgName      = "neco-test"
	repoName     = "meows-ci"
)

var (
	testID                  = "kindtest-" + time.Now().UTC().Format("2006-01-02-150405") // Generate unique ID
	testBranch              = "test-branch-" + testID
	repoRunner1NS           = testID + "-test-repo-runner1"
	repoRunner2NS           = testID + "-test-repo-runner2"
	orgRunner1NS            = testID + "-test-org-runner1"
	repoRunnerPool1Name     = "repo-runnerpool1"
	repoRunnerPool2Name     = "repo-runnerpool2"
	orgRunnerPool1Name      = "org-runnerpool1"
	repoRunnerPool1Replicas = 3
	repoRunnerPool2Replicas = 1
	orgRunnerPool1Replicas  = 1
	githubClient            *github.Client
)

// Env variables.
var (
	binDir                  = os.Getenv("BIN_DIR")
	testRepoWorkDir         = os.Getenv("TEST_REPO_WORK_DIR")
	githubAppID             = os.Getenv("GITHUB_APP_ID")
	githubAppInstallationID = os.Getenv("GITHUB_APP_INSTALLATION_ID")
	githubAppPrivateKeyPath = os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	slackChannel            = os.Getenv("SLACK_CHANNEL")
	slackAppToken           = os.Getenv("SLACK_APP_TOKEN")
	slackBotToken           = os.Getenv("SLACK_BOT_TOKEN")
)

func TestOnKind(t *testing.T) {
	if os.Getenv("KINDTEST") == "" {
		t.Skip("Skip running kindtest/")
	}
	RegisterFailHandler(Fail)
	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(10 * time.Second)
	RunSpecs(t, "KindTest Suite")
}

var _ = BeforeSuite(func() {
	fmt.Println("testID: " + testID)

	By("checking env variables")
	Expect(binDir).ShouldNot(BeEmpty())
	fmt.Println("This test uses the binaries under " + binDir)

	Expect(githubAppID).ShouldNot(BeEmpty())
	Expect(githubAppInstallationID).ShouldNot(BeEmpty())
	Expect(githubAppPrivateKeyPath).ShouldNot(BeEmpty())
	Expect(slackChannel).ShouldNot(BeEmpty())
	Expect(slackAppToken).ShouldNot(BeEmpty())
	Expect(slackBotToken).ShouldNot(BeEmpty())

	By("initializing github client")
	appID, err := strconv.ParseInt(githubAppID, 10, 64)
	Expect(err).ShouldNot(HaveOccurred())
	Expect(appID).ShouldNot(BeZero())

	appInstallID, err := strconv.ParseInt(githubAppInstallationID, 10, 64)
	Expect(err).ShouldNot(HaveOccurred())
	Expect(appInstallID).ShouldNot(BeZero())

	rt, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, appID, appInstallID, githubAppPrivateKeyPath)
	Expect(err).ShouldNot(HaveOccurred())
	githubClient = github.NewClient(&http.Client{Transport: rt})

	By("creating test branch in CI test repository")
	cloneURL := fmt.Sprintf("git@github.com:%s/%s", orgName, repoName)
	fmt.Println(cloneURL)
	gitSafe("clone", "-v", cloneURL, ".")
	gitSafe("checkout", "-b", testBranch)
	pushWorkflowFile("blank.yaml", "", "")
})

var _ = Describe("meows", func() {
	Context("bootstrap", testBootstrap)
	Context("runner", testRunner)
})
