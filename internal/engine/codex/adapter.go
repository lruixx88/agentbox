package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tmalldedede/agentbox/internal/container"
	"github.com/tmalldedede/agentbox/internal/engine"
)

const (
	// AgentName Agent 名称
	AgentName = "codex"

	// DefaultImage 默认镜像 (v2: codex 0.87+, resume 支持 --json)
	DefaultImage = "agentbox/agent:v2"
)

// Adapter Codex 适配器
type Adapter struct {
	image string
}

// New 创建 Codex 适配器
func New() *Adapter {
	return &Adapter{
		image: DefaultImage,
	}
}

// NewWithImage 使用自定义镜像创建适配器
func NewWithImage(image string) *Adapter {
	return &Adapter{
		image: image,
	}
}

// Name 返回 Agent 名称
func (a *Adapter) Name() string {
	return AgentName
}

// DisplayName 返回显示名称
func (a *Adapter) DisplayName() string {
	return "OpenAI Codex"
}

// Description 返回描述
func (a *Adapter) Description() string {
	return "OpenAI Codex CLI - lightweight coding agent"
}

// Image 返回 Docker 镜像
func (a *Adapter) Image() string {
	return a.image
}

// PrepareContainer 准备容器配置
func (a *Adapter) PrepareContainer(session *engine.SessionInfo) *container.CreateConfig {
	// 构建环境变量
	env := make(map[string]string)
	for k, v := range session.Env {
		env[k] = v
	}

	return &container.CreateConfig{
		Name:  fmt.Sprintf("agentbox-%s-%s", AgentName, session.ID),
		Image: a.image,
		Cmd:   []string{"sleep", "infinity"}, // 保持容器运行
		Env:   env,
		Mounts: []container.Mount{
			{
				Source:   session.Workspace,
				Target:   "/workspace",
				ReadOnly: false,
			},
		},
		Resources: container.ResourceConfig{
			CPULimit:    2.0,
			MemoryLimit: 4 * 1024 * 1024 * 1024, // 4GB
		},
		NetworkMode: "bridge", // Codex 需要网络访问 API
		Privileged:  false,    // 由 Runtime 配置决定是否开启特权模式
		Labels: map[string]string{
			"agentbox.managed":    "true",
			"agentbox.agent":      AgentName,
			"agentbox.session.id": session.ID,
		},
	}
}

// PrepareContainerWithConfig 使用 AgentConfig 准备容器配置
func (a *Adapter) PrepareContainerWithConfig(session *engine.SessionInfo, cfg *engine.AgentConfig) *container.CreateConfig {
	config := a.PrepareContainer(session)

	// 应用资源限制
	if cfg.Resources.CPUs > 0 {
		config.Resources.CPULimit = cfg.Resources.CPUs
	}
	if cfg.Resources.MemoryMB > 0 {
		config.Resources.MemoryLimit = int64(cfg.Resources.MemoryMB) * 1024 * 1024
	}

	// 添加标签
	config.Labels["agentbox.agent.id"] = cfg.ID
	config.Labels["agentbox.agent.name"] = cfg.Name

	return config
}

// GenerateConfigTOML 生成 Codex 的 config.toml 内容
// 参考 Codex 源码: codex-rs/core/src/config.rs
// apiKey 参数用于非 OpenAI 提供商，通过 http_headers 直接嵌入 Authorization
func (a *Adapter) GenerateConfigTOML(cfg *engine.AgentConfig, apiKey string) string {
	var sb strings.Builder

	// ===== 基础配置 =====
	// model_provider - 模型提供商
	// Zhipu 兼容：保持自定义 base_url 与 header 认证，强制走 chat wire_api
	provider := cfg.Model.Provider
	baseURL := cfg.Model.BaseURL
	isZhipu := provider == "zhipu" || provider == "zm" ||
		(strings.Contains(baseURL, "open.bigmodel.cn") && strings.Contains(baseURL, "/api/anthropic"))
	if provider != "" {
		sb.WriteString(fmt.Sprintf("model_provider = \"%s\"\n", provider))
	}

	// model - 默认模型
	if cfg.Model.Name != "" {
		sb.WriteString(fmt.Sprintf("model = \"%s\"\n", cfg.Model.Name))
	}

	// model_reasoning_effort - 推理强度 (low/medium/high)
	if cfg.Model.ReasoningEffort != "" {
		sb.WriteString(fmt.Sprintf("model_reasoning_effort = \"%s\"\n", cfg.Model.ReasoningEffort))
	}

	// disable_response_storage - 禁用响应存储 (隐私)
	sb.WriteString("disable_response_storage = true\n")

	// sandbox_mode - 沙箱模式
	// Docker 容器环境不支持 landlock，必须使用 danger-full-access 禁用沙箱
	sb.WriteString("sandbox_mode = \"danger-full-access\"\n")

	sb.WriteString("\n")

	// ===== Provider 配置 =====
	// 即使 Provider 或 BaseURL 为空，也尝试从环境变量推断
	if provider == "" && baseURL != "" {
		// 简单的推断逻辑
		if strings.Contains(baseURL, "open.bigmodel.cn") {
			provider = "zhipu"
		} else if strings.Contains(baseURL, "openai.com") || strings.Contains(baseURL, "api.openai.com") {
			provider = "openai"
		} else if strings.Contains(baseURL, "anthropic.com") || strings.Contains(baseURL, "api.anthropic.com") {
			provider = "anthropic"
		}
	}

	// Codex 使用 OpenAI 兼容 API，需要将 zhipu 的 Anthropic 端点转换为 OpenAI 兼容端点
	// 检查 provider 是否为 zhipu，或者 BaseURL 包含智谱AI的特征
	isZhipu = isZhipu || provider == "zhipu" || (strings.Contains(baseURL, "open.bigmodel.cn") && strings.Contains(baseURL, "/api/anthropic"))
	if isZhipu && strings.Contains(baseURL, "/api/anthropic") {
		// 将 /api/anthropic 转换为 /api/paas/v4 (OpenAI 兼容端点，用于 Codex)
		// 注意：如果需要编码计划权限，可以使用 /api/coding/paas/v4
		baseURL = strings.Replace(baseURL, "/api/anthropic", "/api/paas/v4", 1)
	}

	if provider != "" && baseURL != "" {
		providerName := strings.ToLower(provider)

		sb.WriteString(fmt.Sprintf("[model_providers.%s]\n", providerName))
		sb.WriteString(fmt.Sprintf("name = \"%s\"\n", provider))
		sb.WriteString(fmt.Sprintf("base_url = \"%s\"\n", baseURL))

		// wire_api - API 协议类型 (chat/responses)
		wireAPI := cfg.Model.WireAPI
		if wireAPI == "" {
			if providerName == "openai" {
				if isZhipu {
					wireAPI = "chat"
				} else {
					wireAPI = "responses"
				}
			} else {
				wireAPI = "chat"
			}
		}
		sb.WriteString(fmt.Sprintf("wire_api = \"%s\"\n", wireAPI))

		// requires_openai_auth 和认证方式
		if providerName == "openai" {
			// OpenAI 官方：使用 auth.json 认证
			sb.WriteString("requires_openai_auth = true\n")
		} else {
			// 第三方提供商：使用 http_headers 直接嵌入 Authorization 头
			// 这是 Codex 0.89+ 推荐的方式，比 env_key 更可靠
			sb.WriteString("requires_openai_auth = false\n")
			if apiKey != "" {
				sb.WriteString(fmt.Sprintf("http_headers = {\"Authorization\" = \"Bearer %s\"}\n", apiKey))
			}
		}

		// 重试配置
		sb.WriteString("request_max_retries = 4\n")
		sb.WriteString("stream_max_retries = 10\n")
		sb.WriteString("stream_idle_timeout_ms = 300000\n")
	}

	return sb.String()
}

// GenerateAuthJSON 生成 Codex 的 auth.json 内容
// 参考 Codex 源码: codex-rs/core/src/config.rs
func (a *Adapter) GenerateAuthJSON(apiKey string) string {
	// auth.json 格式: {"OPENAI_API_KEY": "sk-xxx"}
	return fmt.Sprintf(`{"OPENAI_API_KEY": "%s"}`, apiKey)
}

// GetConfigFiles 返回需要挂载到容器的配置文件
// 返回: map[容器内路径]文件内容
// 注意: 使用 ~ 前缀表示需要在运行时展开为用户 home 目录
func (a *Adapter) GetConfigFiles(cfg *engine.AgentConfig, apiKey string) map[string]string {
	files := make(map[string]string)

	// ~/.codex/config.toml
	// 即使 Provider 或 BaseURL 为空，也生成基础配置文件
	// Codex 可以通过环境变量工作，但配置文件可以提供更好的兼容性
	configTOML := a.GenerateConfigTOML(cfg, apiKey)
	// 总是生成配置文件（至少包含基础配置）
	if configTOML == "" {
		// 如果 GenerateConfigTOML 返回空，至少生成基础配置
		configTOML = "disable_response_storage = true\nsandbox_mode = \"danger-full-access\"\n"
	}
	files["~/.codex/config.toml"] = configTOML

	// ~/.codex/auth.json
	// OpenAI 兼容通道需要 auth.json（Zhipu 也复用该路径）
	providerName := strings.ToLower(cfg.Model.Provider)
	isZhipu := providerName == "zhipu" || providerName == "zm" || strings.Contains(cfg.Model.BaseURL, "open.bigmodel.cn")
	if apiKey != "" && (providerName == "openai" || isZhipu) {
		files["~/.codex/auth.json"] = a.GenerateAuthJSON(apiKey)
	}

	return files
}

// PrepareExec 准备执行命令
func (a *Adapter) PrepareExec(req *engine.ExecOptions) []string {
	if req.ThreadID != "" {
		// 多轮对话：resume 子命令不接受额外参数
		return []string{"codex", "exec", "resume", req.ThreadID, "-c", "skip_git_repo_check=true", "--", req.Prompt}
	}

	// 首轮：完整参数（flags 在前，prompt 在最后）
	args := []string{
		"codex", "exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"--json",
	}
	args = append(args, req.Prompt)
	return args
}

// PrepareExecWithConfig 使用 AgentConfig 准备执行命令
func (a *Adapter) PrepareExecWithConfig(req *engine.ExecOptions, cfg *engine.AgentConfig) []string {
	if req.ThreadID != "" {
		// 多轮对话：resume 子命令不接受额外参数
		return []string{"codex", "exec", "resume", req.ThreadID, "-c", "skip_git_repo_check=true", "--", req.Prompt}
	}

	// 首轮：完整参数（flags 在前，prompt 在最后）
	args := []string{
		"codex", "exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"--json",
	}

	// ===== 模型配置 =====
	if cfg.Model.Name != "" {
		args = append(args, "--model", cfg.Model.Name)
	}

	// ===== 推理强度 (Codex 特有) =====
	if cfg.Model.ReasoningEffort != "" {
		args = append(args, "--reasoning-effort", cfg.Model.ReasoningEffort)
	}

	// ===== 目录访问 =====
	for _, dir := range cfg.Permissions.AdditionalDirs {
		args = append(args, "--add-dir", dir)
	}

	// ===== 指令配置 =====
	if cfg.BaseInstructions != "" {
		args = append(args, "--base-instructions", cfg.BaseInstructions)
	}
	if cfg.DeveloperInstructions != "" {
		args = append(args, "--developer-instructions", cfg.DeveloperInstructions)
	}

	// ===== 功能开关 =====
	if cfg.Features.WebSearch {
		args = append(args, "--search")
	}

	// ===== 资源限制 =====
	if cfg.Resources.MaxTokens > 0 {
		args = append(args, "--max-tokens", fmt.Sprintf("%d", cfg.Resources.MaxTokens))
	}
	if cfg.Resources.Timeout > 0 {
		args = append(args, "--timeout", cfg.Resources.Timeout.String())
	}

	// ===== 配置覆盖 =====
	for key, value := range cfg.ConfigOverrides {
		args = append(args, "--config", fmt.Sprintf("%s=%s", key, value))
	}

	// ===== 输出配置 =====
	if cfg.OutputSchema != "" {
		args = append(args, "--output-schema", cfg.OutputSchema)
	}

	// prompt 必须放在最后，避免 flags 被解析为 prompt
	args = append(args, req.Prompt)
	return args
}

// RequiredEnvVars 返回必需的环境变量
func (a *Adapter) RequiredEnvVars() []string {
	return []string{"OPENAI_API_KEY"}
}

// ValidateConfig 验证 AgentConfig 是否与此适配器兼容
func (a *Adapter) ValidateConfig(cfg *engine.AgentConfig) error {
	if cfg.Adapter != engine.AdapterCodex {
		return fmt.Errorf("adapter %q is not compatible with Codex adapter", cfg.Adapter)
	}

	// 验证沙箱模式
	validSandboxModes := map[string]bool{
		"":                                 true,
		engine.SandboxModeReadOnly:         true,
		engine.SandboxModeWorkspaceWrite:   true,
		engine.SandboxModeDangerFullAccess: true,
	}
	if !validSandboxModes[cfg.Permissions.SandboxMode] {
		return fmt.Errorf("invalid sandbox mode %q for Codex", cfg.Permissions.SandboxMode)
	}

	// 验证审批策略
	validApprovalPolicies := map[string]bool{
		"":                             true,
		engine.ApprovalPolicyUntrusted: true,
		engine.ApprovalPolicyOnFailure: true,
		engine.ApprovalPolicyOnRequest: true,
		engine.ApprovalPolicyNever:     true,
	}
	if !validApprovalPolicies[cfg.Permissions.ApprovalPolicy] {
		return fmt.Errorf("invalid approval policy %q for Codex", cfg.Permissions.ApprovalPolicy)
	}

	// 验证推理强度
	validReasoningEfforts := map[string]bool{
		"":       true,
		"low":    true,
		"medium": true,
		"high":   true,
	}
	if !validReasoningEfforts[cfg.Model.ReasoningEffort] {
		return fmt.Errorf("invalid reasoning effort %q for Codex", cfg.Model.ReasoningEffort)
	}

	// Claude Code 专有字段不应该在 Codex 配置中设置
	if cfg.Permissions.Mode != "" {
		return fmt.Errorf("permission_mode is a Claude Code-specific option, not valid for Codex")
	}
	if cfg.Permissions.SkipAll {
		return fmt.Errorf("skip_all is a Claude Code-specific option, not valid for Codex")
	}
	if cfg.SystemPrompt != "" {
		return fmt.Errorf("system_prompt is a Claude Code-specific option, use base_instructions for Codex")
	}
	if len(cfg.CustomAgents) > 0 {
		return fmt.Errorf("custom_agents is a Claude Code-specific option, not valid for Codex")
	}

	return nil
}

// SupportedFeatures 返回此适配器支持的功能列表
func (a *Adapter) SupportedFeatures() []string {
	return []string{
		"model",
		"reasoning_effort",
		"sandbox_mode",
		"approval_policy",
		"full_auto",
		"additional_dirs",
		"base_instructions",
		"developer_instructions",
		"web_search",
		"max_tokens",
		"timeout",
		"config_overrides",
		"output_schema",
	}
}

// NOTE: DirectExecutor 接口已禁用
// codex.Run() 在宿主机进程中执行，而不是容器内，无法使用容器的环境变量和配置
// 改为使用 CLI 执行 + --json 输出格式，在 execViaCLI 中解析 JSONL 输出
//
// func (a *Adapter) Execute(ctx context.Context, opts *engine.ExecOptions) (*engine.ExecResult, error) {
//     ... 保留代码供将来本地执行场景使用 ...
// }

// CodexEvent Codex CLI --json 输出的事件结构
type CodexEvent struct {
	Type     string          `json:"type"`                // 事件类型: thread.started, turn.completed, item.completed, error
	ThreadID string          `json:"thread_id,omitempty"` // Thread ID (thread.started)
	Usage    *CodexUsage     `json:"usage,omitempty"`     // Token 使用统计 (turn.completed)
	Item     json.RawMessage `json:"item,omitempty"`      // 事件内容 (item.completed)
	Error    *CodexError     `json:"error,omitempty"`     // 错误信息 (error, turn.failed)
	Message  string          `json:"message,omitempty"`   // 错误消息 (error)
}

// CodexUsage Token 使用统计
type CodexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens"`
}

// CodexError 错误信息
type CodexError struct {
	Message string `json:"message"`
}

// CodexItem item.completed 事件中的 item
type CodexItem struct {
	Type     string `json:"type"`                // item 类型: agent_message, command_execution, reasoning, etc.
	Text     string `json:"text,omitempty"`      // agent_message 的文本内容
	ExitCode *int   `json:"exit_code,omitempty"` // command_execution 的退出码
}

// ParseJSONLOutput 实现 JSONOutputParser 接口
// 解析 Codex CLI --json 输出的 JSONL 格式
func (a *Adapter) ParseJSONLOutput(output string, includeEvents bool) (*engine.ExecResult, error) {
	var (
		message           string
		messageParts      []string
		threadID          string
		events            []engine.ExecEvent
		usage             *engine.TokenUsage
		exitCode          int
		execErr           string
		responseCompleted bool
		turnCompleted     bool
	)

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()

		// Codex --json 输出可能包含长度前缀，需要找到 JSON 对象的开始位置
		// 例如: "135{\"type\":\"item.completed\",...}"
		jsonStart := strings.Index(line, "{")
		if jsonStart < 0 {
			continue
		}
		line = line[jsonStart:]

		var event CodexEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// 跳过无法解析的行
			continue
		}

		// 如果需要包含事件，记录原始 JSON
		if includeEvents {
			events = append(events, engine.ExecEvent{
				Type: event.Type,
				Raw:  json.RawMessage(line),
			})
		}

		// 处理不同事件类型
		switch event.Type {
		case "thread.started":
			// 提取 thread_id 用于多轮对话 resume
			if event.ThreadID != "" {
				threadID = event.ThreadID
			}

		case "turn.completed":
			// 标记 turn 已完成
			turnCompleted = true
			// 获取 token 使用统计
			if event.Usage != nil {
				usage = &engine.TokenUsage{
					InputTokens:       event.Usage.InputTokens,
					CachedInputTokens: event.Usage.CachedInputTokens,
					OutputTokens:      event.Usage.OutputTokens,
				}
			}

		case "response.completed":
			// 标记 response 已完成（Codex 的最终完成事件）
			responseCompleted = true
		case "response.output_text.done":
			// 兼容部分版本的完成事件
			responseCompleted = true

		case "turn.failed":
			if event.Error != nil {
				execErr = event.Error.Message
			}

		case "item.completed":
			// 解析 item 内容
			if len(event.Item) > 0 {
				var item CodexItem
				if err := json.Unmarshal(event.Item, &item); err == nil {
					switch item.Type {
					case "agent_message":
						// 提取 agent_message 作为最终结果
						message = item.Text
					case "assistant_message", "assistant_text", "message", "output_text":
						if item.Text != "" {
							messageParts = append(messageParts, item.Text)
						}
					case "command_execution":
						// 提取命令执行的退出码
						if item.ExitCode != nil {
							exitCode = *item.ExitCode
						}
					}
				}
			}

		case "error":
			execErr = event.Message
		case "response.output_text.delta":
			if delta := extractStringField(line, "delta"); delta != "" {
				messageParts = append(messageParts, delta)
			}
		case "response.output_text":
			if text := extractStringField(line, "text"); text != "" {
				messageParts = append(messageParts, text)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan output: %w", err)
	}

	// 检查流是否正常完成
	// 如果收到了 turn.completed 或 response.completed，说明流正常完成
	// 如果没有收到完成事件，但也没有错误，可能是流提前关闭了
	if !responseCompleted && !turnCompleted && execErr == "" && len(events) > 0 {
		// 有事件但没有完成事件，可能是流提前关闭
		// 如果已经有 message，可以认为部分成功
		if message == "" {
			execErr = "stream disconnected before completion: stream closed before response.completed"
		}
	}

	if message == "" && len(messageParts) > 0 {
		message = strings.Join(messageParts, "")
	}

	// 如果没有解析到任何 JSON 事件，说明是纯文本输出（resume 模式）
	// 此时将原始输出（去除 Docker 流头）作为 message
	if message == "" && threadID == "" && len(events) == 0 && execErr == "" {
		message = strings.TrimSpace(stripDockerStreamHeaders(output))
	}

	result := &engine.ExecResult{
		Message:  message,
		ExitCode: exitCode,
		Usage:    usage,
		Error:    execErr,
		ThreadID: threadID,
	}

	if includeEvents {
		result.Events = events
	}

	return result, nil
}

func extractStringField(line string, field string) string {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return ""
	}
	value, ok := raw[field]
	if !ok {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

// stripDockerStreamHeaders 从 Docker 多路复用流中提取纯文本
func stripDockerStreamHeaders(raw string) string {
	data := []byte(raw)
	var result []byte

	for len(data) >= 8 {
		streamType := data[0]
		size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		data = data[8:]
		if size <= 0 || size > len(data) {
			return raw
		}
		if streamType == 0x01 {
			result = append(result, data[:size]...)
		}
		data = data[size:]
	}

	if len(result) == 0 && len(raw) > 0 {
		return raw
	}
	return string(result)
}

// init 自动注册到默认注册表
func init() {
	engine.Register(New())
}
