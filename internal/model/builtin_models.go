package model

// allModels is the built-in model table, in registration (listing) order. It
// is copied into the runtime registry at init; runtime changes never mutate
// this slice.
var allModels = []Spec{
	// --- grok.com chat ---
	{ModelName: "grok-4.6", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.6"},
	{ModelName: "grok-4.20-0309-non-reasoning", ModeId: ModeFast, Tier: TierBasic, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning"},
	{ModelName: "grok-4.20-0309", ModeId: ModeAuto, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309"},
	{ModelName: "grok-4.20-0309-reasoning", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning"},
	{ModelName: "grok-4.20-0309-non-reasoning-super", ModeId: ModeFast, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Super"},
	{ModelName: "grok-4.20-0309-super", ModeId: ModeAuto, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Super"},
	{ModelName: "grok-4.20-0309-reasoning-super", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Super"},
	{ModelName: "grok-4.20-0309-non-reasoning-heavy", ModeId: ModeFast, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Heavy"},
	{ModelName: "grok-4.20-0309-heavy", ModeId: ModeAuto, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Heavy"},
	{ModelName: "grok-4.20-0309-reasoning-heavy", ModeId: ModeExpert, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Heavy"},
	{ModelName: "grok-4.20-multi-agent-0309", ModeId: ModeHeavy, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent 0309"},
	{ModelName: "grok-4.20-fast", ModeId: ModeFast, Tier: TierBasic, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Fast", PreferBest: true},
	{ModelName: "grok-4.3-fast", ModeId: ModeFast, Tier: TierBasic, Capability: CapChat, Enabled: true, PublicName: "Grok 4.3 Fast", PreferBest: true},
	{ModelName: "grok-4.20-auto", ModeId: ModeAuto, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Auto", PreferBest: true},
	{ModelName: "grok-4.20-expert", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Expert", PreferBest: true},
	{ModelName: "grok-4.20-heavy", ModeId: ModeHeavy, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Heavy", PreferBest: true},
	{ModelName: "grok-4.3-beta", ModeId: ModeGrok43, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.3 Beta"},
	// --- media (grok.com) ---
	{ModelName: "grok-imagine-image-lite", ModeId: ModeFast, Tier: TierBasic, Capability: CapImage, Enabled: true, PublicName: "Grok Imagine Image Lite"},
	{ModelName: "grok-imagine-image", ModeId: ModeAuto, Tier: TierSuper, Capability: CapImage, Enabled: true, PublicName: "Grok Imagine Image"},
	{ModelName: "grok-imagine-image-pro", ModeId: ModeAuto, Tier: TierSuper, Capability: CapImage, Enabled: true, PublicName: "Grok Imagine Image Pro"},
	{ModelName: "grok-imagine-image-edit", ModeId: ModeAuto, Tier: TierSuper, Capability: CapImageEdit, Enabled: true, PublicName: "Grok Imagine Image Edit"},
	{ModelName: "grok-imagine-video", ModeId: ModeAuto, Tier: TierSuper, Capability: CapVideo, Enabled: true, PublicName: "Grok Imagine Video"},
	// --- console.x.ai media (basic SSO, DPoP) ---
	{ModelName: "grok-imagine-image-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapImage | CapImageEdit, Enabled: true, PublicName: "Grok Imagine Image (Console)"},
	{ModelName: "grok-imagine-image-quality-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapImage | CapImageEdit, Enabled: true, PublicName: "Grok Imagine Image Quality (Console)"},
	{ModelName: "grok-imagine-video-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapVideo, Enabled: true, PublicName: "Grok Imagine Video (Console)"},
	// --- console.x.ai chat (basic, free) ---
	{ModelName: "grok-4.5-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 (Console)"},
	{ModelName: "grok-4.5-low", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 Low Thinking"},
	{ModelName: "grok-4.5-medium", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 Medium Thinking"},
	{ModelName: "grok-4.5-high", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 High Thinking"},
	{ModelName: "grok-4.5-xhigh", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 XHigh Thinking"},
	{ModelName: "grok-4.3-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 (Console)"},
	{ModelName: "grok-4.3-low", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 Low Thinking"},
	{ModelName: "grok-4.3-medium", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 Medium Thinking"},
	{ModelName: "grok-4.3-high", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 High Thinking"},
	{ModelName: "grok-4.20-0309-reasoning-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning (Console)"},
	{ModelName: "grok-4.20-0309-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 0309 (Console)"},
	{ModelName: "grok-4.20-multi-agent-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent (Console)"},
	{ModelName: "grok-4.20-multi-agent-low", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent Low"},
	{ModelName: "grok-4.20-multi-agent-medium", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent Medium"},
	{ModelName: "grok-4.20-multi-agent-high", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent High"},
	{ModelName: "grok-4.20-multi-agent-xhigh", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent XHigh"},
	{ModelName: "grok-4.20-0309-non-reasoning-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning (Console)"},
	{ModelName: "grok-build-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok Build 0.1 (Console)"},
}
