package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// projectProfileFreezeStep gives the test a point after the daemon has
// resolved the project profile and before an agent process is started.
type projectProfileFreezeStep struct {
	started chan struct{}
	release chan struct{}
}

func (s *projectProfileFreezeStep) Name() types.StepName { return types.StepReview }

func (s *projectProfileFreezeStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	close(s.started)
	select {
	case <-s.release:
	case <-sctx.Ctx.Done():
		return nil, sctx.Ctx.Err()
	}
	if _, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{
		Purpose: "project-profile",
		Prompt:  "profile fixture",
		CWD:     sctx.WorkDir,
	}); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

func projectProfileCommonDirForDaemon(t *testing.T, workDir string) string {
	t.Helper()
	common := strings.TrimSpace(gitOutput(t, workDir, "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(common) {
		common = filepath.Join(workDir, common)
	}
	resolved, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatalf("resolve git common dir %q: %v", common, err)
	}
	return filepath.Clean(resolved)
}

// TestPushReceivedUsesAndFreezesTheRegisteredCheckoutProjectProfile exercises
// the complete daemon path. The profile is registered for the source checkout,
// while the run executes in a separate gate worktree. Editing global config
// after the run has started must not retarget the already-created executor.
// The fake Pi captures argv, so the assertion proves the selected profile was
// actually passed to the launched harness without using a real agent.
func TestPushReceivedUsesAndFreezesTheRegisteredCheckoutProjectProfile(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "pi-argv.log")
	piBin := writeCapturingPiAgent(t, t.TempDir(), capturePath)
	claudeBin := writeMockClaude(t, t.TempDir())
	step := &projectProfileFreezeStep{started: make(chan struct{}), release: make(chan struct{})}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
	_, headSHA := setupTestGitRepo(t, p, d, "project-profile-run-repo")
	repo, err := d.GetRepo("project-profile-run-repo")
	if err != nil || repo == nil {
		t.Fatalf("GetRepo: repo=%#v err=%v", repo, err)
	}
	commonDir := projectProfileCommonDirForDaemon(t, repo.WorkingPath)

	configYAML := fmt.Sprintf(`agent: claude
agent_path_override:
  claude: %q
  pi: %q
project_profiles:
  %q:
    agent: pi
    agent_config:
      pi:
        model: local-project-model
        effort: high
`, claudeBin, piBin, commonDir)
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("project-profile-run-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}

	select {
	case <-step.started:
	case <-time.After(5 * time.Second):
		t.Fatal("project-profile step did not start")
	}

	// The active run owns the resolved config. A later edit must affect future
	// runs only; it must not change the profile captured by this executor.
	changedYAML := fmt.Sprintf(`agent: claude
agent_path_override:
  claude: %q
  pi: %q
project_profiles:
  %q:
    agent: pi
    agent_config:
      pi:
        model: changed-after-start
        effort: low
`, claudeBin, piBin, commonDir)
	if err := os.WriteFile(p.ConfigFile(), []byte(changedYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	close(step.release)

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		t.Fatalf("run status = %q, want %q (error: %s)", run.Status, types.RunCompleted, runErr)
	}
	argv, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(argv)
	for _, want := range []string{"--model local-project-model", "--thinking high"} {
		if !strings.Contains(got, want) {
			t.Errorf("Pi argv = %q, missing frozen project-profile setting %q", got, want)
		}
	}
	if strings.Contains(got, "changed-after-start") || strings.Contains(got, "--thinking low") {
		t.Fatalf("active run used config edited after start: argv = %q", got)
	}
}

func TestProjectProfileProbeStepHonorsCancellation(t *testing.T) {
	step := &projectProfileFreezeStep{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := step.Execute(&pipeline.StepContext{Ctx: ctx}); err == nil {
		t.Fatal("cancelled project profile probe did not return an error")
	}
}
