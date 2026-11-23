package render

import (
	"github.com/xxddccaa/akey/internal/spec"
)

// piLevels are pi's thinking levels, lowest first.
var piLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

// renderPi writes models.json (custom providers, contains keys) and
// settings.json (defaults + tools.pi.extra) for the pi coding agent.
//
// models.json: providers.<pid> = {name, baseUrl, api, apiKey, [authHeader],
// models: [{id, name, reasoning, input, contextWindow, maxTokens,
// thinkingLevelMap}]}. api is openai-responses / openai-completions /
// anthropic-messages. Same "<id>-anthropic" split as opencode. Anthropic
// baseUrl has no "/v1" (pi uses the Anthropic SDK convention); bearer auth
// sets authHeader so pi also sends "Authorization: Bearer <key>".
//
// Merge order: model overrides.pi into its models[] entry, provider
// overrides.pi into providers.<pid>; tools.pi.extra goes into settings.json.
func renderPi(m *spec.Master, t *spec.Tool, def *spec.Model) ([]File, error) {
	models := m.ModelsFor(spec.ToolPi)
	if err := checkSplitCollisions(m, models); err != nil {
		return nil, err
	}
	providers := map[string]any{}
	owner := map[string]*spec.Provider{}
	seen := map[string]bool{}
	for _, md := range models {
		p := m.Providers[md.Provider]
		pid := splitProviderID(m, md)
		blk, ok := providers[pid].(map[string]any)
		if !ok {
			blk = piProvider(p, md)
			providers[pid] = blk
			owner[pid] = p
		}
		if seen[pid+"\x00"+md.Model] {
			continue // pi upserts by id; first entry wins
		}
		seen[pid+"\x00"+md.Model] = true
		blk["models"] = append(blk["models"].([]any), deepMerge(piModel(md), md.Overrides[spec.ToolPi]))
	}
	for pid, blk := range providers {
		deepMerge(blk.(map[string]any), owner[pid].Overrides[spec.ToolPi])
	}
	mb, err := encodeJSON(map[string]any{"providers": providers}, false)
	if err != nil {
		return nil, err
	}

	settings := map[string]any{
		"defaultProvider": splitProviderID(m, def),
		"defaultModel":    def.Model,
	}
	if lvl := piLevel(m.Effort(spec.ToolPi)); lvl != "" {
		settings["defaultThinkingLevel"] = lvl
	}
	deepMerge(settings, t.Extra)
	sb, err := encodeJSON(settings, false)
	if err != nil {
		return nil, err
	}
	return []File{
		{RelPath: "models.json", Content: mb},
		{RelPath: "settings.json", Content: sb},
	}, nil
}

func piProvider(p *spec.Provider, md *spec.Model) map[string]any {
	blk := map[string]any{"name": p.Name, "models": []any{}}
	switch {
	case md.API == spec.APIAnthropic:
		blk["api"] = "anthropic-messages"
		blk["baseUrl"] = anthropicRoot(p.Anthropic.BaseURL)
		if p.OpenAI != nil {
			blk["name"] = p.Name + " (Anthropic)"
		}
		if p.Anthropic.Auth == spec.AuthBearer {
			blk["authHeader"] = true
		}
	case p.OpenAI.Wire == spec.WireChat:
		blk["api"] = "openai-completions"
		blk["baseUrl"] = p.OpenAI.BaseURL
	default:
		blk["api"] = "openai-responses"
		blk["baseUrl"] = p.OpenAI.BaseURL
	}
	if p.Key != "" {
		blk["apiKey"] = p.Key
	}
	return blk
}

func piModel(md *spec.Model) map[string]any {
	e := map[string]any{
		"id":        md.Model,
		"name":      md.Name,
		"reasoning": len(md.ReasoningEfforts) > 0,
	}
	if in := filterInput(md.Input, "text", "image"); len(in) > 0 {
		e["input"] = in
	}
	if md.ContextWindow > 0 {
		e["contextWindow"] = md.ContextWindow
	}
	if md.MaxOutput > 0 {
		e["maxTokens"] = md.MaxOutput
	}
	if tm := piThinkingMap(md.ReasoningEfforts); tm != nil {
		e["thinkingLevelMap"] = tm
	}
	return e
}

// piThinkingMap maps pi's thinking levels onto a model's reasoning_efforts.
// Every level gets an explicit value: the provider effort string to send, or
// null when the model does not support it (pi then hides/clamps that level).
//
//	off      -> "none" if the model lists none, else null (cannot disable)
//	minimal, low, medium, high -> same name if listed, else null
//	xhigh    -> "xhigh" if listed, else "max" if listed, else null
//
// Efforts beyond pi's scale ("max" when "xhigh" is also listed, "ultra") are
// unreachable from pi. Returns nil when the model has no efforts.
func piThinkingMap(efforts []string) map[string]any {
	if len(efforts) == 0 {
		return nil
	}
	tm := map[string]any{}
	for _, lvl := range piLevels {
		tm[lvl] = nil
	}
	if hasString(efforts, "none") {
		tm["off"] = "none"
	}
	for _, lvl := range []string{"minimal", "low", "medium", "high"} {
		if hasString(efforts, lvl) {
			tm[lvl] = lvl
		}
	}
	switch {
	case hasString(efforts, "xhigh"):
		tm["xhigh"] = "xhigh"
	case hasString(efforts, "max"):
		tm["xhigh"] = "max"
	}
	return tm
}

// piLevel maps a master effort to pi's defaultThinkingLevel: none -> off,
// minimal..xhigh unchanged, anything above xhigh (max, ultra) -> xhigh.
func piLevel(effort string) string {
	switch effort {
	case "":
		return ""
	case "none":
		return "off"
	case "minimal", "low", "medium", "high", "xhigh":
		return effort
	case "max", "ultra":
		return "xhigh"
	}
	return ""
}
