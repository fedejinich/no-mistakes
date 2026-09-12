package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// initProjectProfileRepo creates a real Git checkout and returns its path and
// canonical Git common directory. Project profiles are keyed by the latter,
// so these tests exercise the same identity the daemon uses for a registered
// checkout rather than relying on a temporary directory name.
func initProjectProfileRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	runProjectProfileGit(t, root, "init", "--initial-branch=main")
	runProjectProfileGit(t, root, "config", "user.name", "No Mistakes Test")
	runProjectProfileGit(t, root, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProjectProfileGit(t, root, "add", "README.md")
	runProjectProfileGit(t, root, "commit", "-m", "initial")
	return root, projectProfileGitCommonDir(t, root)
}

func runProjectProfileGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func projectProfileGitCommonDir(t *testing.T, dir string) string {
	t.Helper()
	common := runProjectProfileGit(t, dir, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	resolved, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatalf("resolve git common dir %q: %v", common, err)
	}
	return filepath.Clean(resolved)
}

func projectProfileYAML(commonDir string) []byte {
	return []byte(fmt.Sprintf(`agent: codex
agent_config:
  codex:
    model: global-codex
    effort: low
  cursor:
    model: global-cursor
project_profiles:
  %q:
    agent: claude
    agent_config:
      claude:
        model: local-claude
        effort: high
      codex:
        model: local-codex
        effort: medium
`, commonDir))
}

func TestProjectProfileMatchesTheRegisteredGitWorktree(t *testing.T) {
	root, commonDir := initProjectProfileRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runProjectProfileGit(t, root, "worktree", "add", "--detach", linked)

	global, err := LoadGlobalFromBytes(projectProfileYAML(commonDir))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if got := projectProfileGitCommonDir(t, linked); got != commonDir {
		t.Fatalf("linked worktree common dir = %q, want %q", got, commonDir)
	}

	for _, path := range []string{root, linked} {
		got, err := global.ForProject(path)
		if err != nil {
			t.Fatalf("ForProject(%q): %v", path, err)
		}
		if got.Agent != types.AgentClaude {
			t.Errorf("ForProject(%q).Agent = %q, want %q", path, got.Agent, types.AgentClaude)
		}
		if got.Agents == nil || len(got.Agents) != 1 || got.Agents[0] != types.AgentClaude {
			t.Errorf("ForProject(%q).Agents = %v, want [%q]", path, got.Agents, types.AgentClaude)
		}
		if profile := got.AgentConfig[string(types.AgentClaude)]; profile != (agentcfg.Profile{Model: "local-claude", Effort: agentcfg.EffortHigh}) {
			t.Errorf("ForProject(%q).AgentConfig[claude] = %#v", path, profile)
		}
	}
}

func TestProjectProfileDoesNotInheritAcrossParentAndSubmodule(t *testing.T) {
	parent, parentCommon := initProjectProfileRepo(t)
	child, childSourceCommon := initProjectProfileRepo(t)
	runProjectProfileGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", child, "modules/child")
	runProjectProfileGit(t, parent, "commit", "-am", "add child submodule")
	childCheckout := filepath.Join(parent, "modules", "child")
	childCommon := projectProfileGitCommonDir(t, childCheckout)
	if childCommon == parentCommon {
		t.Fatalf("submodule common dir unexpectedly equals parent common dir %q", parentCommon)
	}
	if childCommon == childSourceCommon {
		t.Fatalf("submodule checkout reused source common dir %q", childSourceCommon)
	}

	global := DefaultGlobalConfig()
	global.Agent = types.AgentCodex
	global.Agents = []types.AgentName{types.AgentCodex}
	global.ProjectProfiles = map[string]ProjectProfile{
		parentCommon: {Agent: types.AgentClaude},
		childCommon:  {Agent: types.AgentPi},
	}

	parentResolved, err := global.ForProject(parent)
	if err != nil {
		t.Fatalf("ForProject(parent): %v", err)
	}
	if parentResolved.Agent != types.AgentClaude {
		t.Fatalf("parent Agent = %q, want %q", parentResolved.Agent, types.AgentClaude)
	}
	childResolved, err := global.ForProject(childCheckout)
	if err != nil {
		t.Fatalf("ForProject(submodule): %v", err)
	}
	if childResolved.Agent != types.AgentPi {
		t.Fatalf("submodule Agent = %q, want %q", childResolved.Agent, types.AgentPi)
	}

	// A nested repository with no registration must fall back to the global
	// selection. The parent profile is never inherited by ancestry.
	delete(global.ProjectProfiles, childCommon)
	childFallback, err := global.ForProject(childCheckout)
	if err != nil {
		t.Fatalf("ForProject(unregistered submodule): %v", err)
	}
	if childFallback.Agent != types.AgentCodex {
		t.Fatalf("unregistered submodule Agent = %q, want global %q", childFallback.Agent, types.AgentCodex)
	}
}

func TestForProjectUsesGlobalFallbackAndDoesNotMutateGlobalConfig(t *testing.T) {
	root, commonDir := initProjectProfileRepo(t)
	other, _ := initProjectProfileRepo(t)
	global, err := LoadGlobalFromBytes(projectProfileYAML(commonDir))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	original := *global
	originalAgents := append([]types.AgentName(nil), global.Agents...)
	originalAgentConfig := map[string]agentcfg.Profile{}
	for name, profile := range global.AgentConfig {
		originalAgentConfig[name] = profile
	}

	got, err := global.ForProject(root)
	if err != nil {
		t.Fatalf("ForProject(registered): %v", err)
	}
	got.AgentConfig["codex"] = agentcfg.Profile{Model: "mutated"}
	got.ProjectProfiles[commonDir] = ProjectProfile{
		AgentConfig: map[string]agentcfg.Profile{"claude": {Model: "mutated-local"}},
	}
	if global.Agent != original.Agent || !reflect.DeepEqual(global.Agents, originalAgents) || !reflect.DeepEqual(global.AgentConfig, originalAgentConfig) {
		t.Fatalf("ForProject mutated global config: got Agent=%q Agents=%v AgentConfig=%v", global.Agent, global.Agents, global.AgentConfig)
	}
	if profile := global.ProjectProfiles[commonDir].AgentConfig["claude"]; profile.Model != "local-claude" {
		t.Fatalf("ForProject shared ProjectProfiles map: %#v", global.ProjectProfiles)
	}

	fallback, err := global.ForProject(other)
	if err != nil {
		t.Fatalf("ForProject(unregistered): %v", err)
	}
	if fallback.Agent != types.AgentCodex {
		t.Fatalf("fallback Agent = %q, want global %q", fallback.Agent, types.AgentCodex)
	}
	if got := fallback.AgentConfig["codex"]; got != (agentcfg.Profile{Model: "global-codex", Effort: agentcfg.EffortLow}) {
		t.Fatalf("fallback AgentConfig[codex] = %#v", got)
	}
}

func TestForProjectMergesProfileEntriesByFieldAndPreservesAgentOrder(t *testing.T) {
	root, commonDir := initProjectProfileRepo(t)
	global := DefaultGlobalConfig()
	global.Agent = types.AgentClaude
	global.Agents = []types.AgentName{types.AgentClaude}
	global.AgentConfig = map[string]agentcfg.Profile{
		"pi": {Model: "global-pi", Effort: agentcfg.EffortLow},
	}
	global.ProjectProfiles = map[string]ProjectProfile{
		commonDir: {
			Agents: []types.AgentName{types.AgentPi, types.AgentClaude},
			AgentConfig: map[string]agentcfg.Profile{
				"pi": {Effort: agentcfg.EffortHigh},
			},
		},
	}

	got, err := global.ForProject(root)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if got.Agent != types.AgentPi {
		t.Fatalf("Agent = %q, want first local agent %q", got.Agent, types.AgentPi)
	}
	if want := []types.AgentName{types.AgentPi, types.AgentClaude}; !reflect.DeepEqual(got.Agents, want) {
		t.Fatalf("Agents = %v, want %v", got.Agents, want)
	}
	if profile := got.AgentConfig["pi"]; profile != (agentcfg.Profile{Model: "global-pi", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("partial local pi profile = %#v, want global model plus local effort", profile)
	}
}

func TestEffectiveForProjectLocalProfilePrecedesTrustedRepoAgent(t *testing.T) {
	root, commonDir := initProjectProfileRepo(t)
	global := DefaultGlobalConfig()
	global.Agent = types.AgentPi
	global.Agents = []types.AgentName{types.AgentPi}
	global.AgentConfig = map[string]agentcfg.Profile{
		"pi":     {Effort: agentcfg.EffortLow},
		"claude": {Model: "global-claude", Effort: agentcfg.EffortMedium},
	}
	global.ProjectProfiles = map[string]ProjectProfile{
		commonDir: {
			Agent: types.AgentClaude,
			AgentConfig: map[string]agentcfg.Profile{
				"claude": {Model: "local-claude", Effort: agentcfg.EffortHigh},
			},
		},
	}
	repo := &RepoConfig{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}

	got, err := EffectiveForProject(global, repo, root)
	if err != nil {
		t.Fatalf("EffectiveForProject: %v", err)
	}
	if got.Agent != types.AgentClaude {
		t.Fatalf("effective Agent = %q, want local %q over repo %q", got.Agent, types.AgentClaude, types.AgentCodex)
	}
	if len(got.Agents) != 1 || got.Agents[0] != types.AgentClaude {
		t.Fatalf("effective Agents = %v, want [%q]", got.Agents, types.AgentClaude)
	}
	if profile := got.AgentProfile(); profile != (agentcfg.Profile{Model: "local-claude", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("effective claude profile = %#v", profile)
	}
	if profile := got.AgentProfileFor(types.AgentPi); profile != (agentcfg.Profile{Effort: agentcfg.EffortLow}) {
		t.Fatalf("global pi profile was not retained: %#v", profile)
	}
}

func TestEffectiveForProjectWithoutLocalProfileRetainsTrustedRepoSelection(t *testing.T) {
	root, _ := initProjectProfileRepo(t)
	global := DefaultGlobalConfig()
	global.Agent = types.AgentPi
	global.Agents = []types.AgentName{types.AgentPi}
	global.AgentConfig = map[string]agentcfg.Profile{"pi": {Effort: agentcfg.EffortLow}}
	repo := &RepoConfig{Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude}}

	got, err := EffectiveForProject(global, repo, root)
	if err != nil {
		t.Fatalf("EffectiveForProject: %v", err)
	}
	if got.Agent != types.AgentClaude {
		t.Fatalf("effective Agent = %q, want trusted repo %q when no local profile exists", got.Agent, types.AgentClaude)
	}
	if profile := got.AgentProfileFor(types.AgentPi); profile != (agentcfg.Profile{Effort: agentcfg.EffortLow}) {
		t.Fatalf("global profile changed without local profile: %#v", profile)
	}
}

func TestRepoConfigCannotSetProjectProfileOrAgentConfig(t *testing.T) {
	root, commonDir := initProjectProfileRepo(t)
	global := DefaultGlobalConfig()
	global.Agent = types.AgentPi
	global.Agents = []types.AgentName{types.AgentPi}
	global.ProjectProfiles = map[string]ProjectProfile{
		commonDir: {
			Agent: types.AgentClaude,
			AgentConfig: map[string]agentcfg.Profile{
				"claude": {Model: "operator-local-model", Effort: agentcfg.EffortHigh},
			},
		},
	}
	// These fields are versioned repository input. They must not become a
	// second local-selection channel, even when a contributor writes them on a
	// pushed branch.
	repo, err := LoadRepoFromBytes([]byte(`agent: codex
agent_config:
  claude:
    model: attacker-model
    effort: minimal
project_profiles:
  /tmp/attacker-repository:
    agent: pi
`))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	got, err := EffectiveForProject(global, repo, root)
	if err != nil {
		t.Fatalf("EffectiveForProject: %v", err)
	}
	if got.Agent != types.AgentClaude {
		t.Fatalf("repo config selected Agent %q, want local %q", got.Agent, types.AgentClaude)
	}
	if profile := got.AgentProfile(); profile != (agentcfg.Profile{Model: "operator-local-model", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("repo config replaced local profile: %#v", profile)
	}
}

func TestLoadGlobalProjectProfilesFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "unknown agent",
			yaml: `project_profiles:
  /tmp/repository/.git:
    agent_config:
      gemini:
        model: attacker-model
`,
		},
		{
			name: "unknown effort",
			yaml: `project_profiles:
  /tmp/repository/.git:
    agent_config:
      claude:
        effort: turbo
`,
		},
		{
			name: "unmappable model",
			yaml: `project_profiles:
  /tmp/repository/.git:
    agent_config:
      rovodev:
        model: attacker-model
`,
		},
		{
			name: "relative project key",
			yaml: `project_profiles:
  repository/.git:
    agent: claude
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadGlobalFromBytes([]byte(tt.yaml)); err == nil {
				t.Fatalf("LoadGlobalFromBytes accepted invalid project profile")
			}
		})
	}
}

func TestForProjectRejectsInvalidInMemoryProfile(t *testing.T) {
	root, commonDir := initProjectProfileRepo(t)
	global := DefaultGlobalConfig()
	global.ProjectProfiles = map[string]ProjectProfile{
		commonDir: {
			Agent: types.AgentClaude,
			AgentConfig: map[string]agentcfg.Profile{
				"rovodev": {Model: "must-fail-closed"},
			},
		},
	}
	if _, err := global.ForProject(root); err == nil {
		t.Fatal("ForProject accepted an unmappable in-memory project profile")
	}
}
