package githubactionscontroller

const (
	// RunnerContainerName is a container name which runs GitHub Actions runner.
	RunnerContainerName = "runner"

	// RunnerNameEnvName is a env field key for RUNNER_NAME.
	RunnerNameEnvName = "RUNNER_NAME"

	// RunnerOrgEnvName is a env field key for RUNNER_ORG.
	RunnerOrgEnvName = "RUNNER_ORG"

	// RunnerRepoEnvName is a env field key for RUNNER_REPO.
	RunnerRepoEnvName = "RUNNER_REPO"

	// RunnerTokenEnvName is a env field key for RUNNER_TOKEN.
	RunnerTokenEnvName = "RUNNER_TOKEN"

	// RunnerWebhookLabelKey is a label key to tell this pod should be mutated.
	RunnerWebhookLabelKey = "actions.cybozu.com/webhook"
)
