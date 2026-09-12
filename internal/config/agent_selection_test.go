package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentSelectionSnapshotRoundTripsOnlySelection(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentClaude,
		Agents:            []types.AgentName{types.AgentClaude, types.AgentCodex},
		AgentConfig:       map[string]agentcfg.Profile{"claude": {Model: "sonnet", Effort: agentcfg.EffortHigh}},
		ProjectProfileKey: "/work/project/.git",
		ACPXPath:          "/operator/acpx",
		AgentArgsOverride: map[string][]string{"claude": {"--permission-mode", "acceptEdits"}},
		ReplayGlobalYAML:  []byte("secret global input"),
		TrustedConfigSHA:  "trusted-sha",
	}
	payload, err := cfg.MarshalAgentSelection()
	if err != nil {
		t.Fatalf("MarshalAgentSelection: %v", err)
	}
	for _, forbidden := range []string{"acpx_path", "agent_args_override", "secret global input", "trusted-sha"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("snapshot contains forbidden value %q: %s", forbidden, payload)
		}
	}

	got := &Config{
		Agent:             types.AgentAuto,
		Agents:            []types.AgentName{types.AgentAuto},
		AgentConfig:       map[string]agentcfg.Profile{"codex": {Effort: agentcfg.EffortLow}},
		ProjectProfileKey: "/old/project/.git",
		ACPXPath:          "/keep/acpx",
		AgentArgsOverride: map[string][]string{"codex": {"--model", "operator-model"}},
		ReplayGlobalYAML:  []byte("keep this input"),
		TrustedConfigSHA:  "keep-this-sha",
	}
	before := *got
	before.Agents = copyAgents(got.Agents)
	before.AgentConfig = cloneAgentProfiles(got.AgentConfig)
	if err := got.ApplyAgentSelection(payload); err != nil {
		t.Fatalf("ApplyAgentSelection: %v", err)
	}
	if got.Agent != cfg.Agent || !reflect.DeepEqual(got.Agents, cfg.Agents) || !reflect.DeepEqual(got.AgentConfig, cfg.AgentConfig) || got.ProjectProfileKey != cfg.ProjectProfileKey {
		t.Fatalf("selection = Agent %q, Agents %v, AgentConfig %#v, ProjectProfileKey %q; want cfg selection", got.Agent, got.Agents, got.AgentConfig, got.ProjectProfileKey)
	}
	if got.ACPXPath != before.ACPXPath || !reflect.DeepEqual(got.AgentArgsOverride, before.AgentArgsOverride) || !reflect.DeepEqual(got.ReplayGlobalYAML, before.ReplayGlobalYAML) || got.TrustedConfigSHA != before.TrustedConfigSHA {
		t.Fatalf("snapshot changed non-selection config: got ACPXPath=%q args=%v yaml=%q sha=%q", got.ACPXPath, got.AgentArgsOverride, got.ReplayGlobalYAML, got.TrustedConfigSHA)
	}
}

func TestAgentSelectionSnapshotAllowsAutoForDemoAndEmptyMarshalIsLegacy(t *testing.T) {
	auto := &Config{Agent: types.AgentAuto, Agents: []types.AgentName{types.AgentAuto}}
	payload, err := auto.MarshalAgentSelection()
	if err != nil {
		t.Fatalf("MarshalAgentSelection(auto): %v", err)
	}
	if payload == "" {
		t.Fatal("auto selection was treated as an empty legacy snapshot")
	}
	var got Config
	if err := got.ApplyAgentSelection(payload); err != nil {
		t.Fatalf("ApplyAgentSelection(auto): %v", err)
	}
	if got.Agent != types.AgentAuto || !reflect.DeepEqual(got.Agents, []types.AgentName{types.AgentAuto}) {
		t.Fatalf("auto selection = Agent %q, Agents %v", got.Agent, got.Agents)
	}

	empty := &Config{}
	payload, err = empty.MarshalAgentSelection()
	if err != nil {
		t.Fatalf("MarshalAgentSelection(empty): %v", err)
	}
	if payload != "" {
		t.Fatalf("MarshalAgentSelection(empty) = %q, want legacy empty payload", payload)
	}
}

func TestApplyAgentSelectionEmptyPayloadIsNoOp(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentClaude,
		Agents:            []types.AgentName{types.AgentClaude},
		AgentConfig:       map[string]agentcfg.Profile{"claude": {Effort: agentcfg.EffortHigh}},
		ProjectProfileKey: "/work/project/.git",
	}
	before := *cfg
	before.Agents = copyAgents(cfg.Agents)
	before.AgentConfig = cloneAgentProfiles(cfg.AgentConfig)
	for _, payload := range []string{"", " \t\n"} {
		if err := cfg.ApplyAgentSelection(payload); err != nil {
			t.Fatalf("ApplyAgentSelection(%q): %v", payload, err)
		}
	}
	if cfg.Agent != before.Agent || !reflect.DeepEqual(cfg.Agents, before.Agents) || !reflect.DeepEqual(cfg.AgentConfig, before.AgentConfig) || cfg.ProjectProfileKey != before.ProjectProfileKey {
		t.Fatalf("empty snapshot changed selection: got %#v, want %#v", cfg, before)
	}
}

func TestApplyAgentSelectionRejectsMalformedPayloadWithoutMutation(t *testing.T) {
	bad := []struct {
		name    string
		payload string
	}{
		{name: "invalid version", payload: `{"version":2,"agent":"claude","agents":["claude"],"agent_config":{}}`},
		{name: "unknown agent", payload: `{"version":1,"agent":"gemini","agents":["gemini"],"agent_config":{}}`},
		{name: "invalid effort", payload: `{"version":1,"agent":"claude","agents":["claude"],"agent_config":{"claude":{"effort":"turbo"}}}`},
		{name: "unsupported profile", payload: `{"version":1,"agent":"rovodev","agents":["rovodev"],"agent_config":{"rovodev":{"model":"secret-model"}}}`},
		{name: "wrong agents type", payload: `{"version":1,"agent":"claude","agents":"claude","agent_config":{}}`},
		{name: "wrong profile type", payload: `{"version":1,"agent":"claude","agents":["claude"],"agent_config":{"claude":"high"}}`},
		{name: "null profile model", payload: `{"version":1,"agent":"claude","agents":["claude"],"agent_config":{"claude":{"model":null,"effort":"high"}}}`},
		{name: "unknown field", payload: `{"version":1,"agent":"claude","agents":["claude"],"agent_config":{},"raw_args":["--model"]}`},
		{name: "missing selection", payload: `{"version":1,"agent":"","agents":[],"agent_config":{}}`},
		{name: "trailing value", payload: `{"version":1,"agent":"claude","agents":["claude"],"agent_config":{}} {}`},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Agent:             types.AgentPi,
				Agents:            []types.AgentName{types.AgentPi},
				AgentConfig:       map[string]agentcfg.Profile{"pi": {Effort: agentcfg.EffortMax}},
				ProjectProfileKey: "/keep/project/.git",
			}
			before := *cfg
			before.Agents = copyAgents(cfg.Agents)
			before.AgentConfig = cloneAgentProfiles(cfg.AgentConfig)
			if err := cfg.ApplyAgentSelection(tt.payload); err == nil {
				t.Fatal("ApplyAgentSelection accepted malformed payload")
			}
			if cfg.Agent != before.Agent || !reflect.DeepEqual(cfg.Agents, before.Agents) || !reflect.DeepEqual(cfg.AgentConfig, before.AgentConfig) || cfg.ProjectProfileKey != before.ProjectProfileKey {
				t.Fatalf("malformed payload mutated selection: got %#v, want %#v", cfg, before)
			}
		})
	}
}

func TestAgentSelectionSnapshotOptionalProjectProfileKey(t *testing.T) {
	payload := `{"version":1,"agent":"codex","agents":["codex"],"agent_config":{}}`
	cfg := &Config{ProjectProfileKey: "/stale/project/.git"}
	if err := cfg.ApplyAgentSelection(payload); err != nil {
		t.Fatalf("ApplyAgentSelection: %v", err)
	}
	if cfg.ProjectProfileKey != "" {
		t.Fatalf("missing project_profile_key = %q, want empty", cfg.ProjectProfileKey)
	}
}
