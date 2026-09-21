package extensions

const (
	MainAgent   = "agenui_agent"
	StyleAgent  = "agenui_style"
	BinderAgent = "agenui_binder"
)

func IsMainAgent(agentID string) bool {
	return agentID == MainAgent
}

func IsStyleGenerationAgent(agentID string) bool {
	return agentID == StyleAgent
}
