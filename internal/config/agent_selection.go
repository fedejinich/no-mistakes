package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const agentSelectionSnapshotVersion = 1

// agentSelectionSnapshot is the durable, run-scoped part of Config. Keep this
// type separate from Config so a run cannot accidentally persist machine-local
// paths, raw arguments, source YAML, credentials, or another global setting.
type agentSelectionSnapshot struct {
	Agent             types.AgentName
	Agents            []types.AgentName
	AgentConfig       map[string]agentcfg.Profile
	ProjectProfileKey string
}

// agentSelectionSnapshotJSON is the stable JSON representation of an
// agentSelectionSnapshot. Profile has no JSON tags because it is an in-memory
// normalization type, so its fields are spelled explicitly here.
type agentSelectionSnapshotJSON struct {
	Version           int                              `json:"version"`
	Agent             types.AgentName                  `json:"agent"`
	Agents            []types.AgentName                `json:"agents"`
	AgentConfig       map[string]agentSelectionProfile `json:"agent_config"`
	ProjectProfileKey string                           `json:"project_profile_key,omitempty"`
}

// agentSelectionSnapshotWire uses pointers for required fields so omission
// and JSON null cannot silently turn into a zero-value selection. The profile
// type has its own strict decoder for the same reason.
type agentSelectionSnapshotWire struct {
	Version           *int                              `json:"version"`
	Agent             *types.AgentName                  `json:"agent"`
	Agents            *[]types.AgentName                `json:"agents"`
	AgentConfig       *map[string]agentSelectionProfile `json:"agent_config"`
	ProjectProfileKey json.RawMessage                   `json:"project_profile_key"`
}

type agentSelectionProfile struct {
	Model  string          `json:"model,omitempty"`
	Effort agentcfg.Effort `json:"effort,omitempty"`
}

// UnmarshalJSON rejects null scalar profile fields, which encoding/json would
// otherwise accept as empty strings, while DisallowUnknownFields rejects a
// profile shape this version does not understand.
func (p *agentSelectionProfile) UnmarshalJSON(data []byte) error {
	type plain agentSelectionProfile
	var decoded plain
	if err := decodeAgentSelectionJSON(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := decodeAgentSelectionJSON(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"model", "effort"} {
		if value, ok := fields[name]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%s must not be null", name)
		}
	}
	*p = agentSelectionProfile(decoded)
	return nil
}

// MarshalAgentSelection freezes the effective agent selection for a run. An
// entirely empty Config returns an empty payload so callers preserve legacy
// behavior for runs created before selection snapshots existed.
func (c *Config) MarshalAgentSelection() (string, error) {
	if c == nil {
		return "", fmt.Errorf("marshal agent selection requires config")
	}

	snapshot, err := canonicalAgentSelection(agentSelectionSnapshot{
		Agent:             c.Agent,
		Agents:            c.Agents,
		AgentConfig:       c.AgentConfig,
		ProjectProfileKey: c.ProjectProfileKey,
	})
	if err != nil {
		return "", fmt.Errorf("validate agent selection: %w", err)
	}
	if snapshot.Agent == "" && len(snapshot.Agents) == 0 && len(snapshot.AgentConfig) == 0 && snapshot.ProjectProfileKey == "" {
		return "", nil
	}
	if snapshot.Agent == "" {
		return "", fmt.Errorf("validate agent selection: selection must name an agent")
	}

	profiles := make(map[string]agentSelectionProfile, len(snapshot.AgentConfig))
	for name, profile := range snapshot.AgentConfig {
		profiles[name] = agentSelectionProfile{Model: profile.Model, Effort: profile.Effort}
	}
	payload, err := json.Marshal(agentSelectionSnapshotJSON{
		Version:           agentSelectionSnapshotVersion,
		Agent:             snapshot.Agent,
		Agents:            snapshot.Agents,
		AgentConfig:       profiles,
		ProjectProfileKey: snapshot.ProjectProfileKey,
	})
	if err != nil {
		return "", fmt.Errorf("encode agent selection: %w", err)
	}
	return string(payload), nil
}

// ApplyAgentSelection restores the run-scoped selection onto an effective
// Config. An empty payload is the legacy form for a run created before this
// snapshot existed and is therefore a no-op. Non-empty payloads are decoded
// and validated before the receiver is mutated.
func (c *Config) ApplyAgentSelection(payload string) error {
	if c == nil {
		return fmt.Errorf("apply agent selection requires config")
	}
	if strings.TrimSpace(payload) == "" {
		return nil
	}

	snapshot, err := parseAgentSelectionSnapshot([]byte(payload))
	if err != nil {
		return err
	}

	c.Agent = snapshot.Agent
	c.Agents = copyAgents(snapshot.Agents)
	c.AgentConfig = cloneAgentProfiles(snapshot.AgentConfig)
	c.ProjectProfileKey = snapshot.ProjectProfileKey
	return nil
}

func canonicalAgentSelection(snapshot agentSelectionSnapshot) (agentSelectionSnapshot, error) {
	agent := types.AgentName(strings.TrimSpace(string(snapshot.Agent)))
	agents := copyAgents(snapshot.Agents)
	for i, name := range agents {
		name = types.AgentName(strings.TrimSpace(string(name)))
		if name == "" {
			return agentSelectionSnapshot{}, fmt.Errorf("agents[%d] must not be empty", i)
		}
		if err := validateSelectionAgent(name); err != nil {
			return agentSelectionSnapshot{}, fmt.Errorf("agents[%d]: %w", i, err)
		}
		agents[i] = name
	}

	if agent != "" {
		if err := validateSelectionAgent(agent); err != nil {
			return agentSelectionSnapshot{}, fmt.Errorf("agent: %w", err)
		}
		if len(agents) == 0 {
			agents = []types.AgentName{agent}
		} else if agents[0] != agent {
			return agentSelectionSnapshot{}, fmt.Errorf("agent %q does not match the first agents entry %q", agent, agents[0])
		}
	} else if len(agents) > 0 {
		agent = agents[0]
	}

	profiles, err := canonicalAgentProfiles(snapshot.AgentConfig)
	if err != nil {
		return agentSelectionSnapshot{}, err
	}

	return agentSelectionSnapshot{
		Agent:             agent,
		Agents:            agents,
		AgentConfig:       profiles,
		ProjectProfileKey: strings.TrimSpace(snapshot.ProjectProfileKey),
	}, nil
}

func validateSelectionAgent(name types.AgentName) error {
	if name == types.AgentAuto {
		return nil
	}
	if !agentcfg.Known(name) {
		return fmt.Errorf("unknown agent %q", name)
	}
	return nil
}

func canonicalAgentProfiles(profiles map[string]agentcfg.Profile) (map[string]agentcfg.Profile, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(profiles))
	for name := range profiles {
		keys = append(keys, name)
	}
	sort.Strings(keys)

	canonical := make(map[string]agentcfg.Profile, len(profiles))
	for _, name := range keys {
		if strings.TrimSpace(name) != name || name == "" {
			return nil, fmt.Errorf("agent_config has an invalid agent name %q", name)
		}
		if !agentcfg.Known(types.AgentName(name)) {
			return nil, fmt.Errorf("agent_config has an unknown agent %q", name)
		}
		profile := profiles[name]
		profile.Model = strings.TrimSpace(profile.Model)
		if profile.Effort != "" {
			effort, err := agentcfg.ParseEffort(string(profile.Effort))
			if err != nil {
				return nil, fmt.Errorf("agent_config.%s: %w", name, err)
			}
			profile.Effort = effort
		}
		if profile.IsZero() {
			return nil, fmt.Errorf("agent_config.%s must set model or effort", name)
		}
		if err := agentcfg.Validate(types.AgentName(name), profile); err != nil {
			return nil, fmt.Errorf("agent_config.%s: %w", name, err)
		}
		canonical[name] = profile
	}
	return canonical, nil
}

func parseAgentSelectionSnapshot(data []byte) (agentSelectionSnapshot, error) {
	var wire agentSelectionSnapshotWire
	if err := decodeAgentSelectionJSON(data, &wire); err != nil {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: %w", err)
	}
	if wire.Version == nil {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: missing version")
	}
	if *wire.Version != agentSelectionSnapshotVersion {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: unsupported version %d", *wire.Version)
	}
	if wire.Agent == nil {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: agent must be a non-null string")
	}
	if wire.Agents == nil {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: agents must be a non-null array")
	}
	if wire.AgentConfig == nil {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: agent_config must be a non-null object")
	}

	projectProfileKey := ""
	if len(wire.ProjectProfileKey) > 0 {
		if bytes.Equal(bytes.TrimSpace(wire.ProjectProfileKey), []byte("null")) {
			return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: project_profile_key must not be null")
		}
		if err := decodeAgentSelectionJSON(wire.ProjectProfileKey, &projectProfileKey); err != nil {
			return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: invalid project_profile_key: %w", err)
		}
	}

	snapshot, err := canonicalAgentSelection(agentSelectionSnapshot{
		Agent:             *wire.Agent,
		Agents:            *wire.Agents,
		AgentConfig:       profileValues(*wire.AgentConfig),
		ProjectProfileKey: projectProfileKey,
	})
	if err != nil {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: %w", err)
	}
	if snapshot.Agent == "" {
		return agentSelectionSnapshot{}, fmt.Errorf("decode agent selection: selection must name an agent")
	}
	return snapshot, nil
}

func profileValues(profiles map[string]agentSelectionProfile) map[string]agentcfg.Profile {
	values := make(map[string]agentcfg.Profile, len(profiles))
	for name, profile := range profiles {
		values[name] = agentcfg.Profile{Model: profile.Model, Effort: profile.Effort}
	}
	return values
}

func decodeAgentSelectionJSON(data []byte, dst any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty JSON value")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}
