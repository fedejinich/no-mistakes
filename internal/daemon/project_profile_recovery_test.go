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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func waitForProjectProfileParkedRun(t *testing.T, d interface {
	GetRun(string) (*db.Run, error)
}, runID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.AwaitingAgentSince != nil {
			return run
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s did not park awaiting the agent", runID)
	return nil
}

// TestRecoveredRunUsesThePersistedProjectAgentSelection verifies that a
// parked run keeps the agent list and profile it selected at start. Recovery
// deliberately reads a changed global config; the recovered agent must still
// launch with the original local project profile.
func TestRecoveredRunUsesThePersistedProjectAgentSelection(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "pi-recovery-argv.log")
	piBin := writeCapturingPiAgent(t, t.TempDir(), capturePath)
	claudeBin := writeMockClaude(t, t.TempDir())
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "project-profile-recovery-repo")
	repo, err := d.GetRepo("project-profile-recovery-repo")
	if err != nil || repo == nil {
		t.Fatalf("GetRepo: repo=%#v err=%v", repo, err)
	}
	commonDir := projectProfileCommonDirForDaemon(t, repo.WorkingPath)

	initialYAML := fmt.Sprintf(`agent: claude
agent_path_override:
  claude: %q
  pi: %q
project_profiles:
  %q:
    agent: pi
    agent_config:
      pi:
        model: recovery-original-model
        effort: high
`, claudeBin, piBin, commonDir)
	if err := os.WriteFile(p.ConfigFile(), []byte(initialYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("project-profile-recovery-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}

	parked := waitForProjectProfileParkedRun(t, d, result.RunID)
	payload, err := d.GetRunAgentSelection(result.RunID)
	if err != nil {
		t.Fatalf("GetRunAgentSelection: %v", err)
	}
	if strings.TrimSpace(payload) == "" {
		t.Fatal("parked run did not persist an agent selection snapshot")
	}
	if !strings.Contains(payload, "recovery-original-model") || !strings.Contains(payload, "high") {
		t.Fatalf("persisted selection = %q, want original project profile", payload)
	}

	changedYAML := fmt.Sprintf(`agent: claude
agent_path_override:
  claude: %q
  pi: %q
project_profiles:
  %q:
    agent: pi
    agent_config:
      pi:
        model: recovery-changed-model
        effort: low
`, claudeBin, piBin, commonDir)
	removedYAML := fmt.Sprintf("agent: claude\nagent_path_override:\n  claude: %q\n  pi: %q\n", claudeBin, piBin)
	for _, localConfig := range []string{changedYAML, removedYAML} {
		if err := os.WriteFile(capturePath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.ConfigFile(), []byte(localConfig), 0o644); err != nil {
			t.Fatal(err)
		}

		// Build only the recovery plan. The original daemon remains alive with the
		// parked run, while this manager proves recovery can reconstruct it from the
		// same temporary NM_HOME and database without launching any real agent.
		recoveryManager := NewRunManager(d, p, func() []pipeline.Step {
			return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
		})
		plan, err := recoveryManager.prepareRecoveredRun(context.Background(), parked)
		if err != nil {
			t.Fatalf("prepareRecoveredRun: %v", err)
		}
		if plan == nil || plan.agent == nil {
			t.Fatal("prepareRecoveredRun returned no agent")
		}
		defer plan.agent.Close()

		if _, err := plan.agent.Run(context.Background(), agent.RunOpts{
			Purpose: "recovery-project-profile",
			Prompt:  "recovery fixture",
			CWD:     plan.workDir,
		}); err != nil {
			t.Fatalf("recovered agent run: %v", err)
		}
		argv, err := os.ReadFile(capturePath)
		if err != nil {
			t.Fatal(err)
		}
		got := string(argv)
		for _, want := range []string{"--model recovery-original-model", "--thinking high"} {
			if !strings.Contains(got, want) {
				t.Errorf("recovered Pi argv = %q, missing persisted selection %q", got, want)
			}
		}
		if strings.Contains(got, "recovery-changed-model") || strings.Contains(got, "--thinking low") {
			t.Fatalf("recovered run used changed global project profile: argv = %q", got)
		}
	}
}
