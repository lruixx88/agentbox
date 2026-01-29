package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tmalldedede/agentbox/internal/agent"
	"github.com/tmalldedede/agentbox/internal/container"
	"github.com/tmalldedede/agentbox/internal/engine"
	"github.com/tmalldedede/agentbox/internal/logger"
	"github.com/tmalldedede/agentbox/internal/skill"
)

// 模块日志器
var log *slog.Logger

func init() {
	log = logger.Module("session")
}

// Manager 会话管理器
type Manager struct {
	store         Store
	containerMgr  container.Manager
	agentRegistry *engine.Registry
	agentMgr      *agent.Manager
	skillMgr      *skill.Manager
	workspaceBase string
}

// NewManager 创建会话管理器
func NewManager(store Store, containerMgr container.Manager, registry *engine.Registry, workspaceBase string) *Manager {
	return &Manager{
		store:         store,
		containerMgr:  containerMgr,
		agentRegistry: registry,
		workspaceBase: workspaceBase,
	}
}

// SetAgentManager 设置 Agent 管理器
func (m *Manager) SetAgentManager(mgr *agent.Manager) {
	m.agentMgr = mgr
}

// SetSkillManager 设置 Skill 管理器（可选依赖）
func (m *Manager) SetSkillManager(mgr *skill.Manager) {
	m.skillMgr = mgr
}

// Create 创建会话
func (m *Manager) Create(ctx context.Context, req *CreateRequest) (*Session, error) {
	// 确定适配器名称
	adapterName := req.Agent
	var fullConfig *agent.AgentFullConfig

	// 新模型：通过 AgentID 解析完整配置
	if req.AgentID != "" && m.agentMgr != nil {
		var err error
		fullConfig, err = m.agentMgr.GetFullConfig(req.AgentID)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve agent: %w", err)
		}
		adapterName = fullConfig.Agent.Adapter
	}

	if adapterName == "" {
		return nil, fmt.Errorf("agent adapter name is required (set agent_id or agent field)")
	}

	// 获取 Agent 适配器
	adapter, err := m.agentRegistry.Get(adapterName)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %s", adapterName)
	}

	// 生成会话 ID
	sessionID := uuid.New().String()[:8]

	// 解析工作空间路径
	workspace := req.Workspace
	if !filepath.IsAbs(workspace) {
		workspace = filepath.Join(m.workspaceBase, workspace)
	}
	if err := ensureWithinBase(workspace, m.workspaceBase); err != nil {
		return nil, err
	}

	// 确保工作空间存在
	if err := os.MkdirAll(workspace, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workspace: %w", err)
	}

	// 确定资源配置
	cpuLimit := 2.0
	memoryLimit := int64(4 * 1024 * 1024 * 1024)
	if fullConfig != nil && fullConfig.Runtime != nil {
		if fullConfig.Runtime.CPUs > 0 {
			cpuLimit = fullConfig.Runtime.CPUs
		}
		if fullConfig.Runtime.MemoryMB > 0 {
			memoryLimit = int64(fullConfig.Runtime.MemoryMB) * 1024 * 1024
		}
	}

	// 创建会话
	session := &Session{
		ID:        sessionID,
		AgentID:   req.AgentID,
		Agent:     adapterName,
		Status:    StatusCreating,
		Workspace: workspace,
		Env:       req.Env,
		Config: Config{
			CPULimit:    cpuLimit,
			MemoryLimit: memoryLimit,
		},
	}

	if req.Config != nil {
		if req.Config.CPULimit > 0 {
			session.Config.CPULimit = req.Config.CPULimit
		}
		if req.Config.MemoryLimit > 0 {
			session.Config.MemoryLimit = req.Config.MemoryLimit
		}
	}

	// 保存会话
	if err := m.store.Create(session); err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// 准备环境变量
	envVars := make(map[string]string)

	// 新模型：从 Provider 获取环境变量（包含 API Key）
	// 注意：环境变量（含 API Key）在 Session 创建时写入容器，之后修改 Provider 配置不会影响已有容器；
	// 只有新创建的 Session（新任务）才会使用当前 Provider 的配置。多轮对话复用同一 Session，故会继续使用创建时的 key。
	if fullConfig != nil && req.AgentID != "" && m.agentMgr != nil {
		// 检查 Provider 是否已配置 API key
		if fullConfig.Provider != nil && !fullConfig.Provider.IsConfigured {
			return nil, fmt.Errorf("provider %s (%s) does not have API key configured. Please configure it in the Providers page", fullConfig.Provider.ID, fullConfig.Provider.Name)
		}

		provEnv, err := m.agentMgr.GetProviderEnvVars(req.AgentID)
		if err != nil {
			return nil, fmt.Errorf("failed to get provider environment variables: %w", err)
		}

		// 验证 API key 是否存在
		hasAPIKey := false
		for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "API_KEY"} {
			if v, ok := provEnv[key]; ok && v != "" {
				hasAPIKey = true
				break
			}
		}
		if !hasAPIKey && fullConfig.Provider != nil && fullConfig.Provider.RequiresAK {
			return nil, fmt.Errorf("provider %s (%s) requires an API key but none was found. Please configure it in the Providers page", fullConfig.Provider.ID, fullConfig.Provider.Name)
		}

		for k, v := range provEnv {
			envVars[k] = v
		}
		// 便于排查「容器内 API Key 与界面不一致」：Session 创建时注入的是当前 Provider 的配置；
		// 若看到的是旧 key，说明该容器是此前创建的（多轮复用或历史任务），请用新任务验证新 key。
		if fullConfig.Provider != nil {
			log.Debug("session create: provider env injected",
				"session_id", sessionID, "agent_id", req.AgentID,
				"provider_id", fullConfig.Provider.ID, "env_keys", len(provEnv))
		}
		// Agent 的 base_url_override 覆盖 Provider 的 base_url
		if fullConfig.Agent.BaseURLOverride != "" {
			overrideURL := fullConfig.Agent.BaseURLOverride
			// 如果是 Codex adapter 且使用智谱AI，需要转换端点
			if fullConfig.Agent.Adapter == "codex" && fullConfig.Provider != nil {
				isZhipu := fullConfig.Provider.ID == "zhipu" || fullConfig.Provider.TemplateID == "zhipu" ||
					(strings.Contains(overrideURL, "open.bigmodel.cn") && strings.Contains(overrideURL, "/api/anthropic"))
				if isZhipu && strings.Contains(overrideURL, "/api/anthropic") {
					overrideURL = strings.Replace(overrideURL, "/api/anthropic", "/api/paas/v4", 1)
				}
			}
			envVars["OPENAI_BASE_URL"] = overrideURL
			envVars["ANTHROPIC_BASE_URL"] = fullConfig.Agent.BaseURLOverride // ANTHROPIC_BASE_URL 保持原值
		}
		// Agent 自身的 env 覆盖
		for k, v := range fullConfig.Agent.Env {
			envVars[k] = v
		}
	}

	// Route through compat proxy for provider-backed sessions (unless agent overrides base_url).
	if fullConfig != nil && fullConfig.Agent != nil && fullConfig.Provider != nil && fullConfig.Agent.BaseURLOverride == "" {
		if compatProxyEnabled() {
			if proxyURL := compatProxyURL(fullConfig.Agent.Adapter, fullConfig.Provider.ID); proxyURL != "" {
				switch fullConfig.Agent.Adapter {
				case engine.AdapterClaudeCode:
					envVars["ANTHROPIC_BASE_URL"] = proxyURL
				case engine.AdapterCodex, engine.AdapterOpenCode:
					envVars["OPENAI_BASE_URL"] = proxyURL
				}
			}
		}
	}

	// 请求中的 env 优先级最高
	for k, v := range req.Env {
		envVars[k] = v
	}

	// 准备容器配置
	// Codex resume requires skipping git repo check via env.
	if adapterName == engine.AdapterCodex {
		envVars["CODEX_SKIP_GIT_REPO_CHECK"] = "1"
	}

	containerConfig := adapter.PrepareContainer(&engine.SessionInfo{
		ID:        sessionID,
		Workspace: workspace,
		Env:       envVars,
	})

	// 应用资源限制
	containerConfig.Resources.CPULimit = session.Config.CPULimit
	containerConfig.Resources.MemoryLimit = session.Config.MemoryLimit

	// 应用 Runtime 配置（覆盖适配器默认值）
	if fullConfig != nil && fullConfig.Runtime != nil {
		// 镜像覆盖（关键：允许使用预装依赖的技能镜像）
		if fullConfig.Runtime.Image != "" {
			containerConfig.Image = fullConfig.Runtime.Image
		}
		if fullConfig.Runtime.Network != "" {
			containerConfig.NetworkMode = fullConfig.Runtime.Network
		}
		if fullConfig.Runtime.Privileged {
			containerConfig.Privileged = true
		}
	}

	// 添加 session_id 标签（便于 GC 关联）
	if containerConfig.Labels == nil {
		containerConfig.Labels = make(map[string]string)
	}
	containerConfig.Labels["agentbox.session_id"] = sessionID

	// 创建容器
	ctr, err := m.containerMgr.Create(ctx, containerConfig)
	if err != nil {
		session.Status = StatusError
		_ = m.store.Update(session)
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	session.ContainerID = ctr.ID

	// 启动容器
	if err := m.containerMgr.Start(ctx, ctr.ID); err != nil {
		session.Status = StatusError
		_ = m.store.Update(session)
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// 写入配置文件（如果适配器需要）
	// Codex resume does not accept --skip-git-repo-check, so trust workspace in git.
	if adapter.Name() == engine.AdapterCodex {
		_, err := m.containerMgr.Exec(ctx, ctr.ID, []string{"sh", "-c", "git init -q /workspace >/dev/null 2>&1 || true; git config --global --add safe.directory /workspace || true"})
		if err != nil {
			log.Warn("failed to set git safe.directory", "session_id", sessionID, "error", err)
		}
	}

	if err := m.writeConfigFiles(ctx, adapter, ctr.ID, req, envVars); err != nil {
		// 配置文件写入失败不中断创建，但记录警告
		log.Warn("failed to write config files", "session_id", sessionID, "error", err)
	} else {
		log.Debug("config files written", "session_id", sessionID)
	}

	// 注入 Skills 文件到容器（独立于配置文件）
	if err := m.injectSkills(ctx, ctr.ID, req, workspace); err != nil {
		log.Warn("failed to inject skills", "session_id", sessionID, "error", err)
	}

	session.Status = StatusRunning
	if err := m.store.Update(session); err != nil {
		return nil, fmt.Errorf("failed to update session: %w", err)
	}

	return session, nil
}

func ensureWithinBase(path, base string) error {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return fmt.Errorf("invalid workspace base: %w", err)
	}
	realBase := absBase
	if resolved, err := filepath.EvalSymlinks(absBase); err == nil {
		realBase = resolved
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid workspace path: %w", err)
	}

	// 尝试解析完整路径的符号链接
	realPath := absPath
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		realPath = resolved
	} else {
		// 如果完整路径不存在（目录尚未创建），递归解析父目录的符号链接
		// 这处理了 macOS 上 /tmp -> /private/tmp 的情况
		realPath = resolvePathWithSymlinks(absPath)
	}

	rel, err := filepath.Rel(realBase, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		log.Error("workspace validation failed",
			"path", path,
			"base", base,
			"absPath", absPath,
			"absBase", absBase,
			"realPath", realPath,
			"realBase", realBase,
			"rel", rel,
			"err", err)
		return fmt.Errorf("workspace path must be within base: %s", base)
	}
	return nil
}

// resolvePathWithSymlinks 递归解析路径中的符号链接
// 如果路径不存在，会找到第一个存在的父目录并解析其符号链接，然后重建完整路径
func resolvePathWithSymlinks(path string) string {
	// 尝试直接解析
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}

	// 路径不存在，递归查找存在的父目录
	parent := filepath.Dir(path)
	if parent == path || parent == "." || parent == "/" {
		// 已经到根目录，无法继续
		return path
	}

	// 递归解析父目录
	resolvedParent := resolvePathWithSymlinks(parent)

	// 重建路径
	return filepath.Join(resolvedParent, filepath.Base(path))
}

// writeConfigFiles 写入配置文件到容器
func (m *Manager) writeConfigFiles(ctx context.Context, adapter engine.Adapter, containerID string, req *CreateRequest, envVars map[string]string) error {
	// 检查适配器是否实现 ConfigFilesProvider 接口
	cfgProvider, ok := adapter.(engine.ConfigFilesProvider)
	if !ok {
		log.Debug("adapter does not implement ConfigFilesProvider", "adapter", adapter.Name())
		return nil // 适配器不需要配置文件
	}
	log.Debug("adapter implements ConfigFilesProvider", "adapter", adapter.Name(), "agent_id", req.AgentID)

	// 构建 AgentConfig
	cfg := &engine.AgentConfig{}

	// 从 AgentFullConfig 填充（如果通过 AgentID 创建）
	if req.AgentID != "" && m.agentMgr != nil {
		if fullConfig, err := m.agentMgr.GetFullConfig(req.AgentID); err == nil {
			hasBaseURLOverride := fullConfig.Agent.BaseURLOverride != ""
			cfg.ID = fullConfig.Agent.ID
			cfg.Name = fullConfig.Agent.Name
			cfg.Adapter = fullConfig.Agent.Adapter
			cfg.Model = engine.ModelConfig{
				Name:     fullConfig.Agent.Model,
				Provider: "", // 从 Provider 获取
			}
			if fullConfig.Provider != nil {
				cfg.Model.BaseURL = fullConfig.Provider.BaseURL
				cfg.Model.Provider = fullConfig.Provider.ID
				// Codex adapter 需要 OpenAI 兼容端点，自动转换 zhipu 的 Anthropic 端点
				if fullConfig.Agent.Adapter == "codex" {
					isZhipu := fullConfig.Provider.ID == "zhipu" || fullConfig.Provider.TemplateID == "zhipu" ||
						(strings.Contains(cfg.Model.BaseURL, "open.bigmodel.cn") && strings.Contains(cfg.Model.BaseURL, "/api/anthropic"))
					if isZhipu && strings.Contains(cfg.Model.BaseURL, "/api/anthropic") {
						// 使用通用端点 /api/paas/v4
						cfg.Model.BaseURL = strings.Replace(cfg.Model.BaseURL, "/api/anthropic", "/api/paas/v4", 1)
					}
				}
			}
			// Agent 层覆盖 base_url（同一服务商不同 adapter 可能 URL 不同）
			if fullConfig.Agent.BaseURLOverride != "" {
				cfg.Model.BaseURL = fullConfig.Agent.BaseURLOverride
				// 如果 BaseURLOverride 是 zhipu 的 Anthropic 端点，也需要转换为 Codex 兼容端点
				if fullConfig.Agent.Adapter == "codex" && fullConfig.Provider != nil {
					isZhipu := fullConfig.Provider.ID == "zhipu" || fullConfig.Provider.TemplateID == "zhipu" ||
						(strings.Contains(cfg.Model.BaseURL, "open.bigmodel.cn") && strings.Contains(cfg.Model.BaseURL, "/api/anthropic"))
					if isZhipu && strings.Contains(cfg.Model.BaseURL, "/api/anthropic") {
						cfg.Model.BaseURL = strings.Replace(cfg.Model.BaseURL, "/api/anthropic", "/api/paas/v4", 1)
					}
				}
			}
			// Apply compat proxy only when not explicitly overridden.
			if !hasBaseURLOverride && compatProxyEnabled() {
				if proxyURL := compatProxyURL(cfg.Adapter, cfg.Model.Provider); proxyURL != "" {
					cfg.Model.BaseURL = proxyURL
				}
			}
		}
	}

	// If base_url is still empty, do not force proxy.

	// 从环境变量补充 Model 配置（优先级低于 Agent 配置）
	if cfg.Model.BaseURL == "" {
		if baseURL, ok := req.Env["OPENAI_BASE_URL"]; ok && baseURL != "" {
			cfg.Model.BaseURL = baseURL
		}
	}
	if cfg.Model.Provider == "" {
		if provider, ok := req.Env["MODEL_PROVIDER"]; ok && provider != "" {
			cfg.Model.Provider = provider
		} else if cfg.Model.BaseURL != "" {
			cfg.Model.Provider = inferProviderFromBaseURL(cfg.Model.BaseURL)
		}
	}
	if cfg.Model.Name == "" {
		for _, key := range []string{"MODEL", "CODEX_MODEL", "OPENAI_MODEL"} {
			if model, ok := req.Env[key]; ok && model != "" {
				cfg.Model.Name = model
				break
			}
		}
	}
	if cfg.Model.WireAPI == "" {
		if wireAPI, ok := req.Env["WIRE_API"]; ok && wireAPI != "" {
			cfg.Model.WireAPI = wireAPI
		}
	}

	// 获取 API Key（从 envVars，包含 Provider 的 API Key）
	apiKey := ""
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "API_KEY"} {
		if v, ok := envVars[key]; ok && v != "" {
			apiKey = v
			break
		}
	}

	// 获取配置文件
	log.Debug("agent model config",
		"model", cfg.Model.Name,
		"provider", cfg.Model.Provider,
		"base_url", cfg.Model.BaseURL,
		"api_key_present", apiKey != "",
	)
	configFiles := cfgProvider.GetConfigFiles(cfg, apiKey)
	log.Debug("got config files", "count", len(configFiles))
	if len(configFiles) == 0 {
		log.Debug("no config files to write")
		return nil
	}

	// Resolve container HOME once to avoid relying on $HOME expansion in shell.
	containerHome := ""
	homeResult, err := m.containerMgr.Exec(ctx, containerID, []string{"sh", "-c", "echo $HOME"})
	if err != nil {
		log.Warn("failed to resolve container HOME, using fallback", "error", err)
	} else {
		containerHome = strings.TrimSpace(homeResult.Stdout)
	}
	if containerHome == "" || !strings.HasPrefix(containerHome, "/") {
		containerHome = "/home/node"
	}

	// 通过 exec 命令写入每个配置文件
	for filePath, content := range configFiles {
		log.Debug("writing config file", "path", filePath, "content_len", len(content))

		expandedPath := filePath
		if strings.HasPrefix(filePath, "~/") {
			expandedPath = pathpkg.Join(containerHome, filePath[2:])
		}

		// Use POSIX-style path handling for container paths (Windows filepath would break $HOME/.codex -> $HOME\.codex).
		dir := pathpkg.Dir(expandedPath)

		escapedContent := strings.ReplaceAll(content, "'", "'\"'\"'")
		writeCmd := []string{
			"sh", "-c",
			fmt.Sprintf("mkdir -p %s && cat > %s << 'AGENTBOX_EOF'\n%s\nAGENTBOX_EOF", dir, expandedPath, escapedContent),
		}
		log.Debug("exec write command", "dir", dir, "path", expandedPath)
		result, err := m.containerMgr.Exec(ctx, containerID, writeCmd)
		if err != nil {
			log.Error("failed to write config file", "path", filePath, "error", err)
			return fmt.Errorf("failed to write file %s: %w", filePath, err)
		}
		if result.ExitCode != 0 {
			log.Error("config file write failed", "path", filePath, "exit_code", result.ExitCode, "stderr", result.Stderr)
			return fmt.Errorf("failed to write file %s: exit code %d", filePath, result.ExitCode)
		}
		log.Debug("config file written", "path", filePath, "exit_code", result.ExitCode)
	}

	return nil
}

// injectSkills 注入 Skills 到容器（独立于适配器配置）
// 包括 Agent 配置的 Skills 和工作区 Skills
func (m *Manager) injectSkills(ctx context.Context, containerID string, req *CreateRequest, workspace string) error {
	var skillIDs []string
	if req.AgentID != "" && m.agentMgr != nil {
		if fullConfig, err := m.agentMgr.GetFullConfig(req.AgentID); err == nil {
			skillIDs = fullConfig.Agent.SkillIDs
		}
	}

	// 加载工作区 Skills
	var workspaceSkills []*skill.Skill
	if m.skillMgr != nil {
		ws, err := m.skillMgr.LoadWorkspaceSkills(workspace)
		if err != nil {
			log.Warn("failed to load workspace skills", "workspace", workspace, "error", err)
		} else {
			workspaceSkills = ws
			log.Debug("loaded workspace skills", "workspace", workspace, "count", len(ws))
		}
	}

	// 写入 Agent 配置的 Skills
	if err := m.writeSkillFiles(ctx, containerID, skillIDs); err != nil {
		return err
	}

	// 写入工作区 Skills（优先级较低，不覆盖已有的同名 Skill）
	if len(workspaceSkills) > 0 {
		if err := m.writeWorkspaceSkills(ctx, containerID, workspaceSkills, skillIDs); err != nil {
			log.Warn("failed to write workspace skills", "error", err)
		}
	}

	return nil
}

// writeWorkspaceSkills 写入工作区 Skills
func (m *Manager) writeWorkspaceSkills(ctx context.Context, containerID string, skills []*skill.Skill, existingIDs []string) error {
	// 构建已存在的 ID 集合
	existingSet := make(map[string]bool)
	for _, id := range existingIDs {
		existingSet[id] = true
	}

	// 获取容器内 HOME 目录
	homeResult, err := m.containerMgr.Exec(ctx, containerID, []string{"sh", "-c", "echo $HOME"})
	if err != nil {
		return err
	}
	containerHome := strings.TrimSpace(homeResult.Stdout)
	if containerHome == "" {
		containerHome = "/home/node"
	}

	for _, s := range skills {
		// 跳过已存在的 Skill（Agent 配置的优先级更高）
		if existingSet[s.ID] {
			log.Debug("skipping workspace skill (already exists)", "skill_id", s.ID)
			continue
		}

		skillDir := fmt.Sprintf("$HOME/.codex/skills/%s", s.ID)
		containerSkillDir := fmt.Sprintf("%s/.codex/skills/%s", containerHome, s.ID)

		// 复制 SourceDir（如果有）
		if s.SourceDir != "" {
			mkdirCmd := []string{"sh", "-c", fmt.Sprintf("mkdir -p %s/.codex/skills", containerHome)}
			if _, err := m.containerMgr.Exec(ctx, containerID, mkdirCmd); err != nil {
				log.Error("failed to create skills dir", "skill_id", s.ID, "error", err)
				continue
			}

			dstPath := fmt.Sprintf("%s/.codex/skills/", containerHome)
			if err := m.containerMgr.CopyToContainer(ctx, containerID, s.SourceDir, dstPath); err != nil {
				log.Error("failed to copy workspace skill", "skill_id", s.ID, "error", err)
				continue
			}

			// 修复权限
			chownCmd := []string{"sh", "-c", fmt.Sprintf("chmod -R 755 %s", containerSkillDir)}
			m.containerMgr.Exec(ctx, containerID, chownCmd)
		}

		// 生成 SKILL.md
		skillContent := s.ToSkillMD()
		skillPath := fmt.Sprintf("%s/SKILL.md", skillDir)

		escapedContent := strings.ReplaceAll(skillContent, "'", "'\"'\"'")
		writeCmd := []string{
			"sh", "-c",
			fmt.Sprintf("mkdir -p %s && cat > %s << 'AGENTBOX_SKILL_EOF'\n%s\nAGENTBOX_SKILL_EOF", skillDir, skillPath, escapedContent),
		}

		if _, err := m.containerMgr.Exec(ctx, containerID, writeCmd); err != nil {
			log.Error("failed to write workspace skill", "skill_id", s.ID, "error", err)
			continue
		}

		log.Debug("workspace skill injected", "skill_id", s.ID)
	}

	return nil
}

// writeSkillFiles 写入 Skills 文件到容器
// Skills 文件存放位置: ~/.codex/skills/{skill-id}/SKILL.md
func (m *Manager) writeSkillFiles(ctx context.Context, containerID string, skillIDs []string) error {
	if m.skillMgr == nil {
		log.Debug("skill manager not set, skipping skill injection")
		return nil
	}

	if len(skillIDs) == 0 {
		log.Debug("no skills configured")
		return nil
	}

	log.Debug("writing skills to container", "skill_ids", skillIDs)

	// 先获取容器内用户 HOME 目录
	homeResult, err := m.containerMgr.Exec(ctx, containerID, []string{"sh", "-c", "echo $HOME"})
	if err != nil {
		log.Error("failed to get container HOME", "error", err)
		return err
	}
	containerHome := strings.TrimSpace(homeResult.Stdout)
	if containerHome == "" {
		containerHome = "/home/node" // fallback
	}

	for _, skillID := range skillIDs {
		// 获取 Skill
		s, err := m.skillMgr.Get(skillID)
		if err != nil {
			log.Warn("skill not found", "skill_id", skillID, "error", err)
			continue
		}

		if !s.IsEnabled {
			log.Debug("skill is disabled, skipping", "skill_id", skillID)
			continue
		}

		// 检查依赖要求（仅记录警告，不阻止注入）
		if s.Requirements != nil && s.Requirements.HasRequirements() {
			var missing []string
			if len(s.Requirements.Bins) > 0 {
				missing = append(missing, fmt.Sprintf("bins: %v", s.Requirements.Bins))
			}
			if len(s.Requirements.Env) > 0 {
				missing = append(missing, fmt.Sprintf("env: %v", s.Requirements.Env))
			}
			if len(s.Requirements.Pip) > 0 {
				missing = append(missing, fmt.Sprintf("pip: %v", s.Requirements.Pip))
			}
			if len(s.Requirements.Npm) > 0 {
				missing = append(missing, fmt.Sprintf("npm: %v", s.Requirements.Npm))
			}
			if len(missing) > 0 {
				log.Warn("skill has requirements that may not be satisfied",
					"skill_id", skillID,
					"requirements", strings.Join(missing, "; "))
			}
		}

		// Skills 目录: ~/.codex/skills/{skill-id}/
		skillDir := fmt.Sprintf("$HOME/.codex/skills/%s", skillID)
		containerSkillDir := fmt.Sprintf("%s/.codex/skills/%s", containerHome, skillID)

		// 如果有 SourceDir，先复制整个目录
		if s.SourceDir != "" {
			log.Debug("copying skill source directory", "skill_id", skillID, "source_dir", s.SourceDir)

			// 创建目标目录
			mkdirCmd := []string{"sh", "-c", fmt.Sprintf("mkdir -p %s/.codex/skills", containerHome)}
			if _, err := m.containerMgr.Exec(ctx, containerID, mkdirCmd); err != nil {
				log.Error("failed to create skills dir", "skill_id", skillID, "error", err)
				continue
			}

			// 复制整个目录到容器
			dstPath := fmt.Sprintf("%s/.codex/skills/", containerHome)
			if err := m.containerMgr.CopyToContainer(ctx, containerID, s.SourceDir, dstPath); err != nil {
				log.Error("failed to copy skill directory", "skill_id", skillID, "source_dir", s.SourceDir, "error", err)
				continue
			}
			log.Debug("skill directory copied", "skill_id", skillID, "source_dir", s.SourceDir, "dest", containerSkillDir)

			// 修复权限（确保容器用户可读写）
			chownCmd := []string{"sh", "-c", fmt.Sprintf("chmod -R 755 %s", containerSkillDir)}
			if _, err := m.containerMgr.Exec(ctx, containerID, chownCmd); err != nil {
				log.Warn("failed to fix permissions", "skill_id", skillID, "error", err)
			}
		}

		// 生成并写入 SKILL.md（始终动态生成，覆盖 SourceDir 中的 SKILL.md）
		skillContent := s.ToSkillMD()
		skillPath := fmt.Sprintf("%s/SKILL.md", skillDir)

		escapedContent := strings.ReplaceAll(skillContent, "'", "'\"'\"'")
		writeCmd := []string{
			"sh", "-c",
			fmt.Sprintf("mkdir -p %s && cat > %s << 'AGENTBOX_SKILL_EOF'\n%s\nAGENTBOX_SKILL_EOF", skillDir, skillPath, escapedContent),
		}

		result, err := m.containerMgr.Exec(ctx, containerID, writeCmd)
		if err != nil {
			log.Error("failed to write skill file", "skill_id", skillID, "error", err)
			continue
		}
		log.Debug("skill file written", "skill_id", skillID, "path", skillPath, "exit_code", result.ExitCode)

		// 写入附加文件 (从 Files 字段，仅当没有 SourceDir 时)
		if s.SourceDir == "" {
			for _, file := range s.Files {
				filePath := fmt.Sprintf("%s/%s", skillDir, file.Path)
				fileDir := filepath.Dir(filePath)

				escapedFileContent := strings.ReplaceAll(file.Content, "'", "'\"'\"'")
				writeFileCmd := []string{
					"sh", "-c",
					fmt.Sprintf("mkdir -p %s && cat > %s << 'AGENTBOX_SKILL_EOF'\n%s\nAGENTBOX_SKILL_EOF", fileDir, filePath, escapedFileContent),
				}

				if _, err := m.containerMgr.Exec(ctx, containerID, writeFileCmd); err != nil {
					log.Warn("failed to write skill file", "skill_id", skillID, "file", file.Path, "error", err)
				} else {
					log.Debug("skill reference file written", "skill_id", skillID, "file", file.Path)
				}
			}
		}
	}

	log.Info("skills injected to container", "count", len(skillIDs))
	return nil
}

// Get 获取会话
func (m *Manager) Get(ctx context.Context, id string) (*Session, error) {
	return m.store.Get(id)
}

// List 列出会话
func (m *Manager) List(ctx context.Context, filter *ListFilter) ([]*Session, error) {
	return m.store.List(filter)
}

// ListWithCount 列出会话并返回总数 (用于分页)
func (m *Manager) ListWithCount(ctx context.Context, filter *ListFilter) ([]*Session, int, error) {
	sessions, err := m.store.List(filter)
	if err != nil {
		return nil, 0, err
	}
	total, err := m.store.Count(filter)
	if err != nil {
		return nil, 0, err
	}
	return sessions, total, nil
}

// Delete 删除会话
func (m *Manager) Delete(ctx context.Context, id string) error {
	session, err := m.store.Get(id)
	if err != nil {
		return err
	}

	// 删除容器（忽略容器不存在的错误）
	if session.ContainerID != "" {
		_ = m.containerMgr.Stop(ctx, session.ContainerID)
		_ = m.containerMgr.Remove(ctx, session.ContainerID)
		// 忽略错误，容器可能已经被删除
	}

	// 删除会话记录
	return m.store.Delete(id)
}

// Stop 停止会话
func (m *Manager) Stop(ctx context.Context, id string) error {
	session, err := m.store.Get(id)
	if err != nil {
		return err
	}

	if session.ContainerID != "" {
		if err := m.containerMgr.Stop(ctx, session.ContainerID); err != nil {
			return fmt.Errorf("failed to stop container: %w", err)
		}
	}

	session.Status = StatusStopped
	return m.store.Update(session)
}

// Start 启动已停止的会话
func (m *Manager) Start(ctx context.Context, id string) error {
	session, err := m.store.Get(id)
	if err != nil {
		return err
	}

	if session.ContainerID != "" {
		if err := m.containerMgr.Start(ctx, session.ContainerID); err != nil {
			return fmt.Errorf("failed to start container: %w", err)
		}
	}

	session.Status = StatusRunning
	return m.store.Update(session)
}

// Reconnect 重连会话
// 如果会话已停止，尝试重新启动容器
// 如果会话正在运行，直接返回会话信息
func (m *Manager) Reconnect(ctx context.Context, id string) (*Session, error) {
	session, err := m.store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("session not found: %s", id)
	}

	// 如果会话已在运行，直接返回
	if session.Status == StatusRunning {
		// 验证容器是否真的在运行
		if session.ContainerID != "" {
			ctr, err := m.containerMgr.Inspect(ctx, session.ContainerID)
			if err == nil && ctr.Status == container.StatusRunning {
				return session, nil
			}
		}
	}

	// 尝试重新启动容器
	if session.ContainerID != "" {
		// 先检查容器状态
		ctr, err := m.containerMgr.Inspect(ctx, session.ContainerID)
		if err != nil {
			return nil, fmt.Errorf("failed to inspect container: %w", err)
		}

		// 如果容器已停止，重新启动
		if ctr.Status != container.StatusRunning {
			if err := m.containerMgr.Start(ctx, session.ContainerID); err != nil {
				return nil, fmt.Errorf("failed to restart container: %w", err)
			}
		}

		session.Status = StatusRunning
		if err := m.store.Update(session); err != nil {
			return nil, fmt.Errorf("failed to update session: %w", err)
		}
	} else {
		return nil, fmt.Errorf("session has no associated container")
	}

	return session, nil
}

// Exec 在会话中执行命令
func (m *Manager) Exec(ctx context.Context, id string, req *ExecRequest) (*ExecResponse, error) {
	session, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}

	if session.Status != StatusRunning {
		return nil, fmt.Errorf("session is not running: %s", session.Status)
	}

	// 检查容器是否存在
	_, err = m.containerMgr.Inspect(ctx, session.ContainerID)
	if err != nil {
		// 容器不存在，更新 session 状态
		session.Status = StatusError
		_ = m.store.Update(session)
		return nil, fmt.Errorf("container no longer exists, session marked as error: %w", err)
	}

	// 获取 Agent 适配器
	adapter, err := m.agentRegistry.Get(session.Agent)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %s", session.Agent)
	}

	// 准备执行选项
	execOpts := &engine.ExecOptions{
		Prompt:           req.Prompt,
		MaxTurns:         req.MaxTurns,
		Timeout:          req.Timeout,
		AllowedTools:     req.AllowedTools,
		DisallowedTools:  req.DisallowedTools,
		IncludeEvents:    req.IncludeEvents,
		ThreadID:         req.ThreadID,
		WorkingDirectory: session.Workspace,
	}

	// 获取 AgentConfig (如果有 AgentID)
	// 这样 PrepareExecWithConfig 才能使用完整的 Agent 配置（model、permissions 等）
	if session.AgentID != "" && m.agentMgr != nil {
		if fullConfig, err := m.agentMgr.GetFullConfig(session.AgentID); err == nil {
			execOpts.Config = buildEngineConfig(fullConfig)
		}
	}

	// 设置默认值
	if execOpts.MaxTurns <= 0 {
		execOpts.MaxTurns = 10
	}
	if execOpts.Timeout <= 0 {
		execOpts.Timeout = 300 // 默认 5 分钟
	}

	// 创建执行记录
	execID := uuid.New().String()[:8]
	execution := &Execution{
		ID:        execID,
		SessionID: id,
		Prompt:    req.Prompt,
		Status:    ExecutionRunning,
		StartedAt: time.Now(),
	}
	if err := m.store.CreateExecution(execution); err != nil {
		return nil, fmt.Errorf("failed to create execution: %w", err)
	}

	// 创建带超时的上下文
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(execOpts.Timeout)*time.Second)
	defer cancel()

	// 检查 adapter 是否实现了 DirectExecutor 接口
	if directExec, ok := adapter.(engine.DirectExecutor); ok {
		// 使用 Go SDK 直接执行
		return m.execDirect(execCtx, directExec, execOpts, execution)
	}

	// 回退到 CLI 执行方式
	return m.execViaCLI(execCtx, adapter, execOpts, session.ContainerID, execution)
}

// execDirect 使用 Go SDK 直接执行 (Codex)
func (m *Manager) execDirect(ctx context.Context, executor engine.DirectExecutor, opts *engine.ExecOptions, execution *Execution) (*ExecResponse, error) {
	result, err := executor.Execute(ctx, opts)
	if err != nil {
		execution.Status = ExecutionFailed
		if ctx.Err() == context.DeadlineExceeded {
			execution.Error = fmt.Sprintf("execution timeout after %d seconds", opts.Timeout)
		} else {
			execution.Error = err.Error()
		}
		now := time.Now()
		execution.EndedAt = &now
		_ = m.store.UpdateExecution(execution)
		return nil, fmt.Errorf("failed to execute: %w", err)
	}

	// 更新执行记录
	now := time.Now()
	execution.EndedAt = &now
	execution.Output = result.Message
	execution.ExitCode = result.ExitCode
	if result.ExitCode == 0 && result.Error == "" {
		execution.Status = ExecutionSuccess
	} else {
		execution.Status = ExecutionFailed
		execution.Error = result.Error
	}
	_ = m.store.UpdateExecution(execution)

	// 构建响应
	resp := &ExecResponse{
		ExecutionID: execution.ID,
		Message:     result.Message,
		Output:      result.Message, // 兼容旧版
		ExitCode:    result.ExitCode,
		Error:       result.Error,
		ThreadID:    result.ThreadID,
	}

	// 添加 token 使用统计
	if result.Usage != nil {
		resp.Usage = &TokenUsage{
			InputTokens:       result.Usage.InputTokens,
			CachedInputTokens: result.Usage.CachedInputTokens,
			OutputTokens:      result.Usage.OutputTokens,
		}
	}

	// 添加事件列表
	if opts.IncludeEvents && len(result.Events) > 0 {
		resp.Events = make([]ExecEvent, len(result.Events))
		for i, e := range result.Events {
			resp.Events[i] = ExecEvent{
				Type: e.Type,
				Raw:  e.Raw,
			}
		}
	}

	return resp, nil
}

// execViaCLI 通过 CLI 在容器中执行 (Claude Code, OpenCode, Codex)
func (m *Manager) execViaCLI(ctx context.Context, adapter engine.Adapter, opts *engine.ExecOptions, containerID string, execution *Execution) (*ExecResponse, error) {
	// 准备执行命令
	// 如果有 AgentConfig，使用 PrepareExecWithConfig 获取完整配置
	var cmd []string
	if opts.Config != nil {
		cmd = adapter.PrepareExecWithConfig(opts, opts.Config)
	} else {
		cmd = adapter.PrepareExec(opts)
	}

	log.Debug("execViaCLI: running command", "cmd", strings.Join(cmd, " "), "thread_id", opts.ThreadID)

	// 在容器中执行
	result, err := m.containerMgr.Exec(ctx, containerID, cmd)
	if err != nil {
		execution.Status = ExecutionFailed
		if ctx.Err() == context.DeadlineExceeded {
			execution.Error = fmt.Sprintf("execution timeout after %d seconds", opts.Timeout)
		} else {
			execution.Error = err.Error()
		}
		now := time.Now()
		execution.EndedAt = &now
		_ = m.store.UpdateExecution(execution)
		return nil, fmt.Errorf("failed to execute: %w", err)
	}

	// 检查 adapter 是否实现了 JSONOutputParser 接口
	if parser, ok := adapter.(engine.JSONOutputParser); ok {
		// 使用 JSON 解析器解析输出 (Codex --json / Claude Code --output-format stream-json)
		return m.execViaCLIWithJSONParser(parser, opts, result, execution)
	}

	// 非 JSON adapter 的 resume 模式：纯文本输出
	if opts.ThreadID != "" {
		return m.execViaCLIPlainText(opts, result, execution)
	}

	// 更新执行记录 (普通文本输出模式)
	now := time.Now()
	execution.EndedAt = &now
	execution.Output = result.Stdout
	execution.ExitCode = result.ExitCode
	if result.ExitCode == 0 {
		execution.Status = ExecutionSuccess
	} else {
		execution.Status = ExecutionFailed
		execution.Error = result.Stderr
	}
	_ = m.store.UpdateExecution(execution)

	return &ExecResponse{
		ExecutionID: execution.ID,
		Message:     result.Stdout, // CLI 模式下，output 就是 message
		Output:      result.Stdout,
		ExitCode:    result.ExitCode,
		Error:       result.Stderr,
	}, nil
}

// execViaCLIWithJSONParser 使用 JSON 解析器处理 CLI 输出 (如 Codex --json 模式)
func (m *Manager) execViaCLIWithJSONParser(parser engine.JSONOutputParser, opts *engine.ExecOptions, result *container.ExecResult, execution *Execution) (*ExecResponse, error) {
	// 记录原始 CLI 输出
	log.Debug("CLI raw output",
		"stdout_len", len(result.Stdout),
		"stderr_len", len(result.Stderr),
		"exit_code", result.ExitCode,
		"stdout_preview", truncateStr(result.Stdout, 500),
		"stderr_preview", truncateStr(result.Stderr, 300),
	)

	// 解析 JSONL 输出
	parsed, err := parser.ParseJSONLOutput(result.Stdout, opts.IncludeEvents)
	if err != nil {
		// 解析失败，回退到普通文本模式
		now := time.Now()
		execution.EndedAt = &now
		execution.Output = result.Stdout
		execution.ExitCode = result.ExitCode
		if result.ExitCode == 0 {
			execution.Status = ExecutionSuccess
		} else {
			execution.Status = ExecutionFailed
			execution.Error = result.Stderr
		}
		_ = m.store.UpdateExecution(execution)

		return &ExecResponse{
			ExecutionID: execution.ID,
			Message:     result.Stdout,
			Output:      result.Stdout,
			ExitCode:    result.ExitCode,
			Error:       fmt.Sprintf("JSON parse failed: %v; stderr: %s", err, result.Stderr),
		}, nil
	}

	// 更新执行记录
	now := time.Now()
	if parsed.Message == "" {
		stderrMessage := strings.TrimSpace(stripDockerStreamHeaders(result.Stderr))
		if stderrMessage != "" {
			parsed.Message = stderrMessage
		}
	}
	execution.EndedAt = &now
	execution.Output = parsed.Message
	execution.ExitCode = parsed.ExitCode
	if parsed.ExitCode == 0 && parsed.Error == "" {
		execution.Status = ExecutionSuccess
	} else {
		execution.Status = ExecutionFailed
		execution.Error = parsed.Error
	}
	_ = m.store.UpdateExecution(execution)

	// 构建响应
	resp := &ExecResponse{
		ExecutionID: execution.ID,
		Message:     parsed.Message,
		Output:      parsed.Message, // 兼容旧版
		ExitCode:    parsed.ExitCode,
		Error:       parsed.Error,
		ThreadID:    parsed.ThreadID,
	}

	// 添加 token 使用统计
	if parsed.Usage != nil {
		resp.Usage = &TokenUsage{
			InputTokens:       parsed.Usage.InputTokens,
			CachedInputTokens: parsed.Usage.CachedInputTokens,
			OutputTokens:      parsed.Usage.OutputTokens,
		}
	}

	// 添加事件列表
	if opts.IncludeEvents && len(parsed.Events) > 0 {
		resp.Events = make([]ExecEvent, len(parsed.Events))
		for i, e := range parsed.Events {
			resp.Events[i] = ExecEvent{
				Type: e.Type,
				Raw:  e.Raw,
			}
		}
	}

	return resp, nil
}

// execViaCLIPlainText 处理 resume 模式的纯文本输出（不带 --json）
func (m *Manager) execViaCLIPlainText(opts *engine.ExecOptions, result *container.ExecResult, execution *Execution) (*ExecResponse, error) {
	// 从 Docker 多路复用流中提取纯文本
	message := stripDockerStreamHeaders(result.Stdout)
	message = strings.TrimSpace(message)
	if message == "" {
		stderrMessage := strings.TrimSpace(stripDockerStreamHeaders(result.Stderr))
		if stderrMessage != "" {
			message = stderrMessage
		}
	}
	message = cleanCodexResumeOutput(message)

	log.Debug("execViaCLIPlainText: resume output",
		"raw_len", len(result.Stdout),
		"message_len", len(message),
		"exit_code", result.ExitCode,
		"message_preview", truncateStr(message, 200),
	)

	now := time.Now()
	execution.EndedAt = &now
	execution.Output = message
	execution.ExitCode = result.ExitCode
	if result.ExitCode == 0 {
		execution.Status = ExecutionSuccess
	} else {
		execution.Status = ExecutionFailed
		execution.Error = stripDockerStreamHeaders(result.Stderr)
	}
	_ = m.store.UpdateExecution(execution)

	return &ExecResponse{
		ExecutionID: execution.ID,
		Message:     message,
		Output:      message,
		ExitCode:    result.ExitCode,
		Error:       execution.Error,
	}, nil
}

// stripDockerStreamHeaders 从 Docker 多路复用流中提取纯文本
// Docker exec 非 TTY 模式下，stdout/stderr 使用 8 字节头部复用：
// [stream_type(1)][0][0][0][size(4, big-endian)]
// stream_type: 0x01=stdout, 0x02=stderr
func stripDockerStreamHeaders(raw string) string {
	data := []byte(raw)
	var result []byte

	for len(data) >= 8 {
		// 读取 8 字节头部
		streamType := data[0]
		// 大端序 4 字节 size
		size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])

		data = data[8:]

		if size <= 0 || size > len(data) {
			// 格式不匹配，可能不是多路复用流，返回原始内容
			return raw
		}

		// 只提取 stdout (0x01) 的数据
		if streamType == 0x01 {
			result = append(result, data[:size]...)
		}

		data = data[size:]
	}

	// 如果没有成功解析任何帧，返回原始内容（可能本身就是纯文本）
	if len(result) == 0 && len(raw) > 0 {
		return raw
	}

	return string(result)
}

// cleanCodexResumeOutput removes noisy Codex CLI logs from resume plain-text output.
func cleanCodexResumeOutput(raw string) string {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return clean
	}

	lines := strings.Split(clean, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		lower := strings.ToLower(l)
		switch {
		case strings.HasPrefix(l, "ERROR "),
			strings.HasPrefix(lower, "deprecated:"),
			strings.HasPrefix(lower, "mcp startup:"),
			strings.HasPrefix(lower, "openai codex"),
			strings.HasPrefix(lower, "workdir:"),
			strings.HasPrefix(lower, "exec "),
			strings.HasPrefix(lower, "model:"),
			strings.HasPrefix(lower, "provider:"),
			strings.HasPrefix(lower, "approval:"),
			strings.HasPrefix(lower, "sandbox:"),
			strings.HasPrefix(lower, "session id:"),
			strings.HasPrefix(lower, "user "),
			strings.HasPrefix(lower, "user:"),
			strings.Contains(lower, "failed to refresh available models"),
			strings.Contains(lower, "exceeded retry limit"):
			continue
		}
		kept = append(kept, l)
	}

	if len(kept) == 0 {
		return clean
	}
	return strings.Join(kept, "\n")
}

// ExecStream 流式执行命令，返回事件通道 (目前仅支持 Codex)
func (m *Manager) ExecStream(ctx context.Context, id string, req *ExecRequest) (<-chan *StreamEvent, string, error) {
	session, err := m.store.Get(id)
	if err != nil {
		return nil, "", err
	}

	if session.Status != StatusRunning {
		return nil, "", fmt.Errorf("session is not running: %s", session.Status)
	}

	// 检查容器是否存在
	_, err = m.containerMgr.Inspect(ctx, session.ContainerID)
	if err != nil {
		session.Status = StatusError
		_ = m.store.Update(session)
		return nil, "", fmt.Errorf("container no longer exists: %w", err)
	}

	// 获取 Agent 适配器
	adapter, err := m.agentRegistry.Get(session.Agent)
	if err != nil {
		return nil, "", fmt.Errorf("agent not found: %s", session.Agent)
	}

	// 目前只有 Codex 支持流式输出 (--json 模式)
	if session.Agent != "codex" {
		return nil, "", fmt.Errorf("streaming exec only supported for codex agent, got: %s", session.Agent)
	}

	// 准备执行选项
	execOpts := &engine.ExecOptions{
		Prompt:           req.Prompt,
		MaxTurns:         req.MaxTurns,
		Timeout:          req.Timeout,
		IncludeEvents:    true,
		WorkingDirectory: session.Workspace,
	}

	if execOpts.MaxTurns <= 0 {
		execOpts.MaxTurns = 10
	}
	if execOpts.Timeout <= 0 {
		execOpts.Timeout = 300
	}

	// 创建执行记录
	execID := uuid.New().String()[:8]
	execution := &Execution{
		ID:        execID,
		SessionID: id,
		Prompt:    req.Prompt,
		Status:    ExecutionRunning,
		StartedAt: time.Now(),
	}
	if err := m.store.CreateExecution(execution); err != nil {
		return nil, "", fmt.Errorf("failed to create execution: %w", err)
	}

	// 准备执行命令
	cmd := adapter.PrepareExec(execOpts)

	// 启动流式执行
	stream, err := m.containerMgr.ExecStream(ctx, session.ContainerID, cmd)
	if err != nil {
		execution.Status = ExecutionFailed
		execution.Error = err.Error()
		now := time.Now()
		execution.EndedAt = &now
		_ = m.store.UpdateExecution(execution)
		return nil, "", fmt.Errorf("failed to start exec stream: %w", err)
	}

	// 创建事件通道
	eventCh := make(chan *StreamEvent, 100)

	// 启动 goroutine 读取输出并解析
	go m.processExecStream(ctx, stream, execution, eventCh)

	return eventCh, execID, nil
}

// processExecStream 处理流式执行输出
func (m *Manager) processExecStream(ctx context.Context, stream *container.ExecStream, execution *Execution, eventCh chan<- *StreamEvent) {
	defer close(eventCh)
	defer stream.Reader.Close()

	// 发送开始事件
	eventCh <- &StreamEvent{
		Type:        "execution.started",
		ExecutionID: execution.ID,
	}

	var outputBuilder strings.Builder
	var lastMessage string
	var responseCompleted bool
	var turnCompleted bool
	scanner := bufio.NewScanner(stream.Reader)
	// 增大缓冲区以处理长行
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			eventCh <- &StreamEvent{
				Type:        "execution.cancelled",
				ExecutionID: execution.ID,
				Error:       "context cancelled",
			}
			return
		default:
		}

		line := scanner.Text()
		outputBuilder.WriteString(line)
		outputBuilder.WriteString("\n")

		// 记录原始行（用于调试）
		log.Debug("stream line received", "execution_id", execution.ID, "line_preview", truncateStr(line, 200))

		// 找到 JSON 对象的开始位置 (Codex 输出可能有长度前缀)
		jsonStart := strings.Index(line, "{")
		if jsonStart < 0 {
			log.Debug("line has no JSON, skipping", "execution_id", execution.ID, "line", truncateStr(line, 100))
			continue
		}
		jsonLine := line[jsonStart:]

		// 解析 Codex 事件
		var rawEvent map[string]json.RawMessage
		if err := json.Unmarshal([]byte(jsonLine), &rawEvent); err != nil {
			log.Debug("failed to parse JSON line", "execution_id", execution.ID, "error", err, "line", truncateStr(jsonLine, 200))
			continue
		}

		// 获取事件类型
		var eventType string
		if typeData, ok := rawEvent["type"]; ok {
			json.Unmarshal(typeData, &eventType)
			log.Debug("parsed event type", "execution_id", execution.ID, "event_type", eventType)
		} else {
			log.Debug("event has no type field", "execution_id", execution.ID, "raw_event", truncateStr(jsonLine, 200))
			continue
		}

		// 跟踪完成事件
		if eventType == "response.completed" {
			responseCompleted = true
		}
		if eventType == "turn.completed" {
			turnCompleted = true
		}

		// 构建流式事件
		streamEvent := &StreamEvent{
			Type:        eventType,
			ExecutionID: execution.ID,
			Data:        json.RawMessage(jsonLine),
		}

		// 提取 agent_message 文本
		if eventType == "item.completed" {
			if itemData, ok := rawEvent["item"]; ok {
				var item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(itemData, &item); err == nil {
					log.Debug("parsed item.completed", "execution_id", execution.ID, "item_type", item.Type, "has_text", item.Text != "", "text_len", len(item.Text))
					if item.Type == "agent_message" && item.Text != "" {
						streamEvent.Text = item.Text
						// 累积消息（Codex 可能分多次发送）
						if lastMessage != "" {
							lastMessage += "\n" + item.Text
						} else {
							lastMessage = item.Text
						}
						log.Info("extracted agent message", "execution_id", execution.ID, "message_len", len(lastMessage))
					}
				} else {
					log.Debug("failed to parse item data", "execution_id", execution.ID, "error", err)
				}
			} else {
				log.Debug("item.completed event has no item field", "execution_id", execution.ID)
			}
		} else {
			// 记录其他事件类型（用于调试）
			log.Debug("received event", "execution_id", execution.ID, "event_type", eventType)
		}

		// 处理错误
		if eventType == "error" || eventType == "turn.failed" {
			if msgData, ok := rawEvent["message"]; ok {
				var msg string
				json.Unmarshal(msgData, &msg)
				streamEvent.Error = msg
			}
			if errData, ok := rawEvent["error"]; ok {
				var errObj struct {
					Message string `json:"message"`
				}
				if err := json.Unmarshal(errData, &errObj); err == nil {
					streamEvent.Error = errObj.Message
				}
			}
		}

		// 发送事件到通道
		select {
		case eventCh <- streamEvent:
			// 事件已发送
		case <-ctx.Done():
			// 上下文已取消，停止发送
			return
		default:
			// 通道满了，记录警告但继续处理
			log.Warn("event channel full, dropping event", "execution_id", execution.ID, "event_type", eventType)
		}
	}

	// 检查流是否正常完成
	scannerErr := scanner.Err()

	// 检查容器 exec 的退出码（如果可能）
	var exitCode int = -1
	if stream.ExecID != "" {
		// 尝试获取 exec 的退出码
		// 使用类型断言访问 DockerManager 的 InspectExec 方法
		type InspectExecer interface {
			InspectExec(ctx context.Context, execID string) (int, error)
		}
		if inspectable, ok := m.containerMgr.(InspectExecer); ok {
			if code, err := inspectable.InspectExec(context.Background(), stream.ExecID); err == nil {
				exitCode = code
				log.Debug("exec exit code", "exec_id", stream.ExecID, "exit_code", exitCode)
			} else {
				log.Debug("failed to inspect exec", "exec_id", stream.ExecID, "error", err)
			}
		}
	}

	// 记录诊断信息
	log.Info("stream processing completed",
		"execution_id", execution.ID,
		"scanner_err", scannerErr,
		"response_completed", responseCompleted,
		"turn_completed", turnCompleted,
		"has_message", lastMessage != "",
		"message_len", len(lastMessage),
		"exit_code", exitCode,
		"output_len", outputBuilder.Len(),
	)

	if scannerErr != nil {
		execution.Status = ExecutionFailed
		execution.Error = fmt.Sprintf("stream scanner error: %v (exit_code: %d)", scannerErr, exitCode)
		execution.ExitCode = exitCode
	} else if !responseCompleted && !turnCompleted {
		// 流关闭了但没有收到完成事件
		// 如果已经有消息，可以认为部分成功，否则标记为失败
		if lastMessage == "" {
			// 检查是否有任何输出（即使不是有效的 JSON）
			rawOutput := outputBuilder.String()
			if len(rawOutput) > 0 {
				// 有输出但没有解析到消息，可能是格式问题
				execution.Status = ExecutionFailed
				// 提取前500字符用于错误消息，完整输出保存在 Output 字段
				outputPreview := rawOutput
				if len(outputPreview) > 500 {
					outputPreview = outputPreview[:500] + "..."
				}
				execution.Error = fmt.Sprintf("stream disconnected before completion: stream closed before response.completed (exit_code: %d, output_len: %d). This may indicate: 1) Container process was killed (OOM/timeout), 2) Network connection issue, 3) Codex CLI crashed, 4) API authentication failed. Raw output preview: %s. Check container logs and execution.Output for details.", exitCode, len(rawOutput), outputPreview)
				execution.Output = rawOutput // 保存原始输出用于调试
			} else {
				// 完全没有输出，可能是进程根本没有启动
				execution.Status = ExecutionFailed
				execution.Error = fmt.Sprintf("stream disconnected before completion: no output received (exit_code: %d). Container process may have failed to start or was immediately terminated.", exitCode)
			}
		} else {
			// 有消息但没有完成事件，可能是流提前关闭但已有结果
			execution.Status = ExecutionSuccess
			log.Info("stream completed with message but no completion event", "execution_id", execution.ID, "message_len", len(lastMessage))
		}
		execution.ExitCode = exitCode
	} else {
		// 正常完成
		execution.Status = ExecutionSuccess
		execution.ExitCode = exitCode
	}

	// 更新执行记录
	now := time.Now()
	execution.EndedAt = &now
	execution.Output = lastMessage
	_ = m.store.UpdateExecution(execution)

	// 发送完成事件
	eventCh <- &StreamEvent{
		Type:        "execution.completed",
		ExecutionID: execution.ID,
		Text:        lastMessage,
		Error:       execution.Error,
	}
}

// GetExecutions 获取会话的执行历史
func (m *Manager) GetExecutions(ctx context.Context, sessionID string) ([]*Execution, error) {
	return m.store.ListExecutions(sessionID)
}

// GetExecution 获取单个执行记录
func (m *Manager) GetExecution(ctx context.Context, sessionID, execID string) (*Execution, error) {
	exec, err := m.store.GetExecution(execID)
	if err != nil {
		return nil, err
	}
	// 验证执行记录属于该会话
	if exec.SessionID != sessionID {
		return nil, fmt.Errorf("execution %s does not belong to session %s", execID, sessionID)
	}
	return exec, nil
}

// ListContainerIDs 列出所有会话关联的容器 ID（实现 container.SessionLister 接口）
func (m *Manager) ListContainerIDs(ctx context.Context) ([]string, error) {
	sessions, err := m.store.List(nil)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s.ContainerID != "" {
			ids = append(ids, s.ContainerID)
		}
	}
	return ids, nil
}

// GetWorkspace 获取会话工作空间路径
func (m *Manager) GetWorkspace(sessionID string) (string, error) {
	session, err := m.store.Get(sessionID)
	if err != nil {
		return "", err
	}
	return session.Workspace, nil
}

// ANSI color codes
const (
	ansiReset       = "\x1b[0m"
	ansiBold        = "\x1b[1m"
	ansiDim         = "\x1b[2m"
	ansiRed         = "\x1b[31m"
	ansiGreen       = "\x1b[32m"
	ansiYellow      = "\x1b[33m"
	ansiBlue        = "\x1b[34m"
	ansiMagenta     = "\x1b[35m"
	ansiCyan        = "\x1b[36m"
	ansiGray        = "\x1b[90m"
	ansiBrightWhite = "\x1b[97m"
)

// GetLogs 获取会话的执行日志（带 ANSI 颜色）
func (m *Manager) GetLogs(ctx context.Context, id string) (string, error) {
	// 验证会话存在
	_, err := m.store.Get(id)
	if err != nil {
		return "", err
	}

	// 从执行记录聚合日志
	executions, err := m.store.ListExecutions(id)
	if err != nil {
		return "", fmt.Errorf("failed to get executions: %w", err)
	}

	if len(executions) == 0 {
		return "", nil
	}

	// 按开始时间排序
	sort.Slice(executions, func(i, j int) bool {
		return executions[i].StartedAt.Before(executions[j].StartedAt)
	})

	var logs strings.Builder
	for i, exec := range executions {
		if i > 0 {
			logs.WriteString("\n")
		}

		// 状态颜色
		statusColor := ansiGray
		switch exec.Status {
		case ExecutionSuccess:
			statusColor = ansiGreen
		case ExecutionFailed:
			statusColor = ansiRed
		case ExecutionRunning:
			statusColor = ansiYellow
		}

		// 执行头部信息
		logs.WriteString(fmt.Sprintf("%s%s=== Execution %s %s%s[%s]%s %s===%s\n",
			ansiBold, ansiCyan, exec.ID, ansiReset,
			statusColor, exec.Status, ansiReset,
			ansiBold+ansiCyan, ansiReset))
		logs.WriteString(fmt.Sprintf("%sStarted: %s%s\n", ansiGray, exec.StartedAt.Format(time.RFC3339), ansiReset))
		if exec.EndedAt != nil {
			logs.WriteString(fmt.Sprintf("%sEnded: %s%s\n", ansiGray, exec.EndedAt.Format(time.RFC3339), ansiReset))
		}

		// Prompt
		logs.WriteString(fmt.Sprintf("\n%s❯%s %s%s%s\n", ansiGreen, ansiReset, ansiBrightWhite, exec.Prompt, ansiReset))

		// 输出内容
		if exec.Output != "" {
			logs.WriteString(fmt.Sprintf("\n%s--- Output ---%s\n", ansiDim, ansiReset))
			logs.WriteString(exec.Output)
			if !strings.HasSuffix(exec.Output, "\n") {
				logs.WriteString("\n")
			}
		}

		// 错误内容
		if exec.Error != "" {
			logs.WriteString(fmt.Sprintf("\n%s--- Error ---%s\n", ansiRed, ansiReset))
			logs.WriteString(fmt.Sprintf("%s%s%s\n", ansiRed, exec.Error, ansiReset))
		}

		// 退出码
		if exec.ExitCode != 0 {
			logs.WriteString(fmt.Sprintf("\n%sExit Code: %d%s\n", ansiRed, exec.ExitCode, ansiReset))
		}
	}

	return logs.String(), nil
}

// StreamLogs 获取容器实时日志流
func (m *Manager) StreamLogs(ctx context.Context, id string) (io.ReadCloser, error) {
	// 获取会话
	sess, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}

	// 检查容器是否存在
	if sess.ContainerID == "" {
		return nil, fmt.Errorf("session has no container")
	}

	// 获取容器日志流
	return m.containerMgr.Logs(ctx, sess.ContainerID)
}

// inferProviderFromBaseURL 从 BaseURL 推断 Provider 名称
func inferProviderFromBaseURL(baseURL string) string {
	// 常见的 Provider URL 模式
	providerPatterns := map[string][]string{
		"openai":     {"api.openai.com"},
		"azure":      {"azure.com", "openai.azure.com"},
		"deepseek":   {"api.deepseek.com"},
		"zhipu":      {"open.bigmodel.cn", "bigmodel.cn"},
		"qwen":       {"dashscope.aliyuncs.com"},
		"kimi":       {"api.moonshot.cn", "moonshot.cn"},
		"minimax":    {"api.minimax.chat", "api.minimaxi.com"},
		"baichuan":   {"api.baichuan-ai.com"},
		"openrouter": {"openrouter.ai"},
		"together":   {"api.together.xyz"},
		"groq":       {"api.groq.com"},
		"fireworks":  {"api.fireworks.ai"},
	}

	baseURLLower := strings.ToLower(baseURL)
	for provider, patterns := range providerPatterns {
		for _, pattern := range patterns {
			if strings.Contains(baseURLLower, pattern) {
				return provider
			}
		}
	}

	// 默认返回 "custom"
	return "custom"
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}

// buildEngineConfig 从 AgentFullConfig 构建 engine.AgentConfig
func buildEngineConfig(fullConfig *agent.AgentFullConfig) *engine.AgentConfig {
	if fullConfig == nil || fullConfig.Agent == nil {
		return nil
	}

	cfg := &engine.AgentConfig{
		ID:      fullConfig.Agent.ID,
		Name:    fullConfig.Agent.Name,
		Adapter: fullConfig.Agent.Adapter,
		Model: engine.ModelConfig{
			Name:            fullConfig.Agent.Model,
			ReasoningEffort: fullConfig.Agent.ModelConfig.ReasoningEffort,
			HaikuModel:      fullConfig.Agent.ModelConfig.HaikuModel,
			SonnetModel:     fullConfig.Agent.ModelConfig.SonnetModel,
			OpusModel:       fullConfig.Agent.ModelConfig.OpusModel,
			TimeoutMS:       fullConfig.Agent.ModelConfig.TimeoutMS,
			MaxOutputTokens: fullConfig.Agent.ModelConfig.MaxOutputTokens,
			DisableTraffic:  fullConfig.Agent.ModelConfig.DisableTraffic,
			WireAPI:         fullConfig.Agent.ModelConfig.WireAPI,
		},
		Permissions: engine.PermissionConfig{
			Mode:            fullConfig.Agent.Permissions.Mode,
			AllowedTools:    fullConfig.Agent.Permissions.AllowedTools,
			DisallowedTools: fullConfig.Agent.Permissions.DisallowedTools,
			Tools:           fullConfig.Agent.Permissions.Tools,
			SkipAll:         fullConfig.Agent.Permissions.SkipAll,
			SandboxMode:     fullConfig.Agent.Permissions.SandboxMode,
			ApprovalPolicy:  fullConfig.Agent.Permissions.ApprovalPolicy,
			FullAuto:        fullConfig.Agent.Permissions.FullAuto,
			AdditionalDirs:  fullConfig.Agent.Permissions.AdditionalDirs,
		},
		SystemPrompt:       fullConfig.Agent.SystemPrompt,
		AppendSystemPrompt: fullConfig.Agent.AppendSystemPrompt,
		OutputFormat:       fullConfig.Agent.OutputFormat,
		CustomAgents:       fullConfig.Agent.CustomAgents,
		ConfigOverrides:    fullConfig.Agent.ConfigOverrides,
		OutputSchema:       fullConfig.Agent.OutputSchema,
		Features: engine.FeaturesConfig{
			WebSearch: fullConfig.Agent.Features.WebSearch,
		},
	}

	// 从 Provider 填充
	if fullConfig.Provider != nil {
		cfg.Model.BaseURL = fullConfig.Provider.BaseURL
		cfg.Model.Provider = fullConfig.Provider.ID
	}

	// Agent 层覆盖 base_url
	if fullConfig.Agent.BaseURLOverride != "" {
		cfg.Model.BaseURL = fullConfig.Agent.BaseURLOverride
	}

	// 从 Runtime 填充资源限制
	if fullConfig.Runtime != nil {
		cfg.Resources.CPUs = fullConfig.Runtime.CPUs
		cfg.Resources.MemoryMB = fullConfig.Runtime.MemoryMB
	}

	// Route through compat proxy for provider-backed sessions (unless agent overrides base_url).
	if fullConfig.Provider != nil && fullConfig.Agent.BaseURLOverride == "" && compatProxyEnabled() {
		if proxyURL := compatProxyURL(cfg.Adapter, cfg.Model.Provider); proxyURL != "" {
			cfg.Model.BaseURL = proxyURL
		}
	}

	return cfg
}

func compatProxyEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("AGENTBOX_COMPAT_PROXY")))
	if raw == "" {
		return true
	}
	switch raw {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func compatProxyBaseURL() string {
	if base := strings.TrimSpace(os.Getenv("AGENTBOX_COMPAT_PROXY_BASE_URL")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return "http://host.docker.internal:18080"
}

func compatProxyURL(adapter, providerID string) string {
	if providerID == "" {
		return ""
	}
	base := compatProxyBaseURL()
	switch adapter {
	case engine.AdapterClaudeCode:
		return base + "/api/v1/compat/anthropic/" + providerID
	case engine.AdapterCodex, engine.AdapterOpenCode:
		return base + "/api/v1/compat/openai/" + providerID
	default:
		return ""
	}
}
