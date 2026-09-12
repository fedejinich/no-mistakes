package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ProjectProfile is an operator-local agent selection for one Git
// repository. The map key is the repository's canonical Git common directory,
// so linked worktrees share a profile while nested submodules get their own
// identity. Project profiles are loaded from global config only.
type ProjectProfile struct {
	Agent       types.AgentName             `yaml:"agent"`
	Agents      []types.AgentName           `yaml:"-"`
	AgentConfig map[string]agentcfg.Profile `yaml:"agent_config"`
}

// projectProfileRaw is the YAML representation of one project profile. It
// uses the same scalar-or-list agent spelling as the global agent field and
// the same validated model/effort entries as agent_config.
type projectProfileRaw struct {
	Agent       agentList                  `yaml:"agent"`
	AgentConfig map[string]agentProfileRaw `yaml:"agent_config"`
}

// parseProjectProfiles validates and canonicalizes project profile keys. A
// missing path is retained in cleaned absolute form so an operator can set a
// profile before cloning a repository; an existing path is realpath-resolved
// so a symlink cannot create a second identity for the same Git directory.
func parseProjectProfiles(raw map[string]projectProfileRaw) (map[string]ProjectProfile, error) {
	profiles := make(map[string]ProjectProfile, len(raw))
	keysByCanonical := make(map[string]string, len(raw))
	for _, key := range sortedProjectProfileKeys(raw) {
		canonical, err := canonicalProjectProfileKey(key)
		if err != nil {
			return nil, fmt.Errorf("invalid project_profiles.%s: %w", key, err)
		}
		if previous, exists := keysByCanonical[canonical]; exists {
			return nil, fmt.Errorf("invalid project_profiles: keys %q and %q resolve to the same Git common directory %q", previous, key, canonical)
		}
		keysByCanonical[canonical] = key

		agents := copyAgents(raw[key].Agent)
		if err := validateProjectProfileAgents(agents, "agent"); err != nil {
			return nil, fmt.Errorf("invalid project_profiles.%s: %w", key, err)
		}
		agentConfig, err := parseAgentConfig(raw[key].AgentConfig)
		if err != nil {
			return nil, fmt.Errorf("invalid project_profiles.%s.agent_config: %w", key, err)
		}
		profiles[canonical] = ProjectProfile{
			Agent:       firstAgent(agents),
			Agents:      agents,
			AgentConfig: agentConfig,
		}
	}
	if len(profiles) == 0 {
		return nil, nil
	}
	return profiles, nil
}

func canonicalProjectProfileKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("Git common directory must not be empty")
	}
	if !filepath.IsAbs(key) {
		return "", fmt.Errorf("Git common directory %q is not an absolute path", key)
	}
	key = filepath.Clean(key)
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	}
	return filepath.Abs(key)
}

func validateProjectProfileAgents(agents []types.AgentName, field string) error {
	for i, name := range agents {
		name = types.AgentName(strings.TrimSpace(string(name)))
		if name == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
		if name != types.AgentAuto && !agentcfg.Known(name) {
			return fmt.Errorf("%s[%d] names an unknown agent %q", field, i, name)
		}
	}
	return nil
}

func projectProfileAgents(profile ProjectProfile) []types.AgentName {
	if len(profile.Agents) > 0 {
		return copyAgents(profile.Agents)
	}
	if name := types.AgentName(strings.TrimSpace(string(profile.Agent))); name != "" {
		return []types.AgentName{name}
	}
	return nil
}

func validateProjectProfile(profile ProjectProfile) error {
	agents := projectProfileAgents(profile)
	if err := validateProjectProfileAgents(agents, "agent"); err != nil {
		return err
	}
	for name, entry := range profile.AgentConfig {
		agentName := types.AgentName(strings.TrimSpace(name))
		if !agentcfg.Known(agentName) {
			return fmt.Errorf("agent_config has an unknown agent %q", name)
		}
		if err := agentcfg.Validate(agentName, entry); err != nil {
			return fmt.Errorf("agent_config.%s: %w", name, err)
		}
	}
	return nil
}

// ForProject returns an isolated global configuration with the matching
// operator-local project profile applied. It never mutates g. If no profile
// is registered for the repository, the returned configuration is a deep
// enough copy for callers to resolve or edit its agent selection safely.
func (g *GlobalConfig) ForProject(path string) (*GlobalConfig, error) {
	if g == nil {
		return nil, fmt.Errorf("project profile resolution requires global config")
	}
	clone := cloneGlobalConfig(g)
	profile, found, _, err := g.projectProfileFor(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return &clone, nil
	}
	if err := validateProjectProfile(profile); err != nil {
		return nil, fmt.Errorf("invalid project profile for %q: %w", path, err)
	}
	applyProjectProfileToGlobal(&clone, profile)
	return &clone, nil
}

// EffectiveForProject merges the trusted repository configuration and then
// applies the operator-local project profile. Applying the local profile last
// lets the operator's explicit local agent selection take precedence over the
// trusted repository's agent choice while retaining the repository's other
// resolved settings.
func EffectiveForProject(global *GlobalConfig, repo *RepoConfig, path string) (*Config, error) {
	if global == nil {
		return nil, fmt.Errorf("project profile resolution requires global config")
	}
	if repo == nil {
		repo = &RepoConfig{}
	}
	cfg := Merge(global, repo)
	if len(global.ProjectProfiles) == 0 {
		return cfg, nil
	}
	profile, found, key, err := global.projectProfileFor(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return cfg, nil
	}
	if err := validateProjectProfile(profile); err != nil {
		return nil, fmt.Errorf("invalid project profile for %q: %w", path, err)
	}
	applyProjectProfileToConfig(cfg, profile)
	cfg.ProjectProfileKey = key
	return cfg, nil
}

func (g *GlobalConfig) projectProfileFor(path string) (ProjectProfile, bool, string, error) {
	if len(g.ProjectProfiles) == 0 {
		return ProjectProfile{}, false, "", nil
	}
	commonDir, err := git.FindGitCommonDir(path)
	if err != nil {
		return ProjectProfile{}, false, "", fmt.Errorf("resolve project profile for %q: %w", path, err)
	}
	profile, found := g.ProjectProfiles[commonDir]
	if !found {
		return ProjectProfile{}, false, commonDir, nil
	}
	return cloneProjectProfile(profile), true, commonDir, nil
}

func applyProjectProfileToGlobal(global *GlobalConfig, profile ProjectProfile) {
	agents := projectProfileAgents(profile)
	if len(agents) > 0 {
		global.Agents = agents
		global.Agent = firstAgent(agents)
	}
	if len(profile.AgentConfig) > 0 {
		global.AgentConfig = mergeAgentProfiles(global.AgentConfig, profile.AgentConfig)
	}
}

func applyProjectProfileToConfig(cfg *Config, profile ProjectProfile) {
	agents := projectProfileAgents(profile)
	if len(agents) > 0 {
		cfg.Agents = agents
		cfg.Agent = firstAgent(agents)
	}
	if len(profile.AgentConfig) > 0 {
		cfg.AgentConfig = mergeAgentProfiles(cfg.AgentConfig, profile.AgentConfig)
	}
}

func mergeAgentProfiles(base, overlay map[string]agentcfg.Profile) map[string]agentcfg.Profile {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	merged := make(map[string]agentcfg.Profile, len(base)+len(overlay))
	for name, profile := range base {
		merged[name] = profile
	}
	for name, profile := range overlay {
		current := merged[name]
		if strings.TrimSpace(profile.Model) != "" {
			current.Model = strings.TrimSpace(profile.Model)
		}
		if profile.Effort != "" {
			current.Effort = profile.Effort
		}
		if current.IsZero() {
			delete(merged, name)
			continue
		}
		merged[name] = current
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func cloneProjectProfile(profile ProjectProfile) ProjectProfile {
	return ProjectProfile{
		Agent:       profile.Agent,
		Agents:      copyAgents(profile.Agents),
		AgentConfig: cloneAgentProfiles(profile.AgentConfig),
	}
}

func cloneAgentProfiles(profiles map[string]agentcfg.Profile) map[string]agentcfg.Profile {
	if len(profiles) == 0 {
		return nil
	}
	clone := make(map[string]agentcfg.Profile, len(profiles))
	for name, profile := range profiles {
		clone[name] = profile
	}
	return clone
}

func cloneGlobalConfig(global *GlobalConfig) GlobalConfig {
	clone := *global
	clone.SourceYAML = append([]byte(nil), global.SourceYAML...)
	clone.Agents = copyAgents(global.Agents)
	clone.ACPRegistryOverrides = cloneStringMap(global.ACPRegistryOverrides)
	clone.AgentPathOverride = cloneStringMap(global.AgentPathOverride)
	clone.AgentArgsOverride = cloneStringSliceMap(global.AgentArgsOverride)
	clone.AgentConfig = cloneAgentProfiles(global.AgentConfig)
	if len(global.ProjectProfiles) > 0 {
		clone.ProjectProfiles = make(map[string]ProjectProfile, len(global.ProjectProfiles))
		for key, profile := range global.ProjectProfiles {
			clone.ProjectProfiles[key] = cloneProjectProfile(profile)
		}
	} else {
		clone.ProjectProfiles = nil
	}
	clone.ReviewAgents = cloneReviewAgents(global.ReviewAgents)
	clone.WorktreeRoots = cloneStringMap(global.WorktreeRoots)
	clone.ForgeProfiles = cloneForgeProfiles(global.ForgeProfiles)
	clone.Intent.DisabledReaders = append([]string(nil), global.Intent.DisabledReaders...)
	return clone
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneStringSliceMap(values map[string][]string) map[string][]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string][]string, len(values))
	for key, value := range values {
		clone[key] = append([]string(nil), value...)
	}
	return clone
}

func cloneReviewAgents(values map[string]ReviewAgent) map[string]ReviewAgent {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]ReviewAgent, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneForgeProfiles(values ForgeProfiles) ForgeProfiles {
	if len(values) == 0 {
		return nil
	}
	clone := make(ForgeProfiles, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

// sortedProjectProfileKeys keeps profile validation deterministic even when
// callers construct the raw map directly.
func sortedProjectProfileKeys(values map[string]projectProfileRaw) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
