package grok

// Console model metadata. Everything the Console payload builder knows about a
// model lives in the two tables below; ConsoleModels (public name -> upstream
// name, consumed by internal/api) and the per-name effort lookup are derived
// from them, so adding a model is a one-line change in one place.

// consoleModelSpec maps a public model name to its console.x.ai model.
type consoleModelSpec struct {
	Public   string // name API clients use
	Upstream string // "model" field sent to console.x.ai
	// Effort is the reasoning effort the public name pins ("" = taken from the
	// request's reasoning_effort).
	Effort string
}

// consoleUpstreamTraits are the payload features of an upstream model.
type consoleUpstreamTraits struct {
	Reasoning       bool // accepts a "reasoning" block
	SearchTools     bool // supports web_search / x_search tools
	MaxOutputTokens int  // 0 = defaultConsoleMaxOutputTokens
}

const defaultConsoleMaxOutputTokens = 1_000_000

var consoleModelSpecs = []consoleModelSpec{
	{"grok-imagine-image-console", "grok-imagine-image", ""},
	{"grok-imagine-image-quality-console", "grok-imagine-image-quality", ""},
	{"grok-imagine-video-console", "grok-imagine-video", ""},

	{"grok-4.5-console", "grok-4.5", ""},
	{"grok-4.5-low", "grok-4.5", "low"},
	{"grok-4.5-medium", "grok-4.5", "medium"},
	{"grok-4.5-high", "grok-4.5", "high"},
	{"grok-4.5-xhigh", "grok-4.5", "xhigh"},

	{"grok-4.3-console", "grok-4.3", ""},
	{"grok-4.3-low", "grok-4.3", "low"},
	{"grok-4.3-medium", "grok-4.3", "medium"},
	{"grok-4.3-high", "grok-4.3", "high"},

	{"grok-4.20-0309-reasoning-console", "grok-4.20-0309-reasoning", ""},
	{"grok-4.20-0309-console", "grok-4.20-0309", ""},
	{"grok-4.20-0309-non-reasoning-console", "grok-4.20-0309-non-reasoning", ""},

	{"grok-4.20-multi-agent-console", "grok-4.20-multi-agent-0309", ""},
	{"grok-4.20-multi-agent-low", "grok-4.20-multi-agent-0309", "low"},
	{"grok-4.20-multi-agent-medium", "grok-4.20-multi-agent-0309", "medium"},
	{"grok-4.20-multi-agent-high", "grok-4.20-multi-agent-0309", "high"},
	{"grok-4.20-multi-agent-xhigh", "grok-4.20-multi-agent-0309", "xhigh"},

	{"grok-build-console", "grok-build-0.1", ""},
}

var consoleUpstreams = map[string]consoleUpstreamTraits{
	"grok-4.3":                     {Reasoning: true, SearchTools: true},
	"grok-4.5":                     {Reasoning: true, SearchTools: true, MaxOutputTokens: 2_000_000},
	"grok-4.20-multi-agent-0309":   {Reasoning: true, SearchTools: true, MaxOutputTokens: 2_000_000},
	"grok-4.20-0309":               {SearchTools: true},
	"grok-4.20-0309-reasoning":     {SearchTools: true},
	"grok-4.20-0309-non-reasoning": {SearchTools: true},
	"grok-build-0.1":               {SearchTools: true, MaxOutputTokens: 256_000},
}

// ConsoleModels maps the public model name -> the console.x.ai model field.
// It is derived from consoleModelSpecs and must be treated as read-only.
var ConsoleModels = func() map[string]string {
	m := make(map[string]string, len(consoleModelSpecs))
	for _, s := range consoleModelSpecs {
		m[s.Public] = s.Upstream
	}
	return m
}()

// consoleFixedEffort maps a public name that pins its reasoning effort.
var consoleFixedEffort = func() map[string]string {
	m := map[string]string{}
	for _, s := range consoleModelSpecs {
		if s.Effort != "" {
			m[s.Public] = s.Effort
		}
	}
	return m
}()

// reasoningEffortMap maps OpenAI reasoning_effort -> console API effort.
var reasoningEffortMap = map[string]string{
	"none":    "none",
	"minimal": "low",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "xhigh",
}

// ConsoleUpstreamModel returns the console.x.ai model name for a public model
// name. Names that are not in the table are returned unchanged (they are
// treated as upstream names already).
func ConsoleUpstreamModel(model string) string { return consoleUpstreamName(model) }

// consoleUpstreamName resolves a public (or already upstream) model name.
func consoleUpstreamName(model string) string {
	if up := ConsoleModels[model]; up != "" {
		return up
	}
	return model
}

// consoleEffort picks the reasoning effort: the model's pinned effort, else the
// request's reasoning_effort mapped to the console vocabulary, else medium.
func consoleEffort(model, requested string) string {
	if e := consoleFixedEffort[model]; e != "" {
		return e
	}
	if requested == "" {
		return "medium"
	}
	if e := reasoningEffortMap[requested]; e != "" {
		return e
	}
	return "medium"
}
