package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tmalldedede/agentbox/internal/agent"
	"github.com/tmalldedede/agentbox/internal/auth"
	"github.com/tmalldedede/agentbox/internal/batch"
	"github.com/tmalldedede/agentbox/internal/channel"
	"github.com/tmalldedede/agentbox/internal/channel/dingtalk"
	"github.com/tmalldedede/agentbox/internal/channel/feishu"
	"github.com/tmalldedede/agentbox/internal/channel/wecom"
	"github.com/tmalldedede/agentbox/internal/config"
	"github.com/tmalldedede/agentbox/internal/container"
	"github.com/tmalldedede/agentbox/internal/coordinate"
	"github.com/tmalldedede/agentbox/internal/cron"
	"github.com/tmalldedede/agentbox/internal/engine"
	"github.com/tmalldedede/agentbox/internal/history"
	"github.com/tmalldedede/agentbox/internal/mcp"
	"github.com/tmalldedede/agentbox/internal/oauth"
	"github.com/tmalldedede/agentbox/internal/plugin"
	"github.com/tmalldedede/agentbox/internal/provider"
	"github.com/tmalldedede/agentbox/internal/runtime"
	"github.com/tmalldedede/agentbox/internal/session"
	"github.com/tmalldedede/agentbox/internal/settings"
	"github.com/tmalldedede/agentbox/internal/skill"
	"github.com/tmalldedede/agentbox/internal/task"
	"github.com/tmalldedede/agentbox/internal/webhook"
)

// Server HTTP 服务器
type Server struct {
	httpServer         *http.Server
	engine             *gin.Engine
	authHandler        *AuthHandler
	authManager        *auth.Manager
	handler            *Handler
	fileHandler        *FileHandler
	publicFileHandler  *PublicFileHandler
	wsHandler          *WSHandler
	providerHandler    *ProviderHandler
	mcpHandler         *MCPHandler
	skillHandler       *SkillHandler
	imageHandler       *ImageHandler
	systemHandler      *SystemHandler
	taskHandler        *TaskHandler
	webhookHandler     *WebhookHandler
	runtimeHandler     *RuntimeHandler
	agentHandler       *AgentHandler
	historyHandler     *HistoryHandler
	dashboardHandler   *DashboardHandler
	batchHandler       *BatchHandler
	settingsHandler    *SettingsHandler
	cronHandler        *CronHandler
	channelHandler     *ChannelHandler
	feishuHandler      *FeishuHandler
	wecomHandler       *WecomHandler
	dingtalkHandler    *DingtalkHandler
	coordinateHandler  *CoordinateHandler
	gatewayHandler     *GatewayHandler
	oauthSyncHandler   *OAuthSyncAPI
	zhipuCompatHandler *ZhipuCompatHandler
	compatHandler      *CompatHandler
}

// Deps 服务器依赖（从 App 容器注入）
type Deps struct {
	Auth            *auth.Manager
	Session         *session.Manager
	Registry        *engine.Registry
	Container       container.Manager
	Provider        *provider.Manager
	Runtime         *runtime.Manager
	MCP             *mcp.Manager
	Skill           *skill.Manager
	Task            *task.Manager
	Webhook         *webhook.Manager
	Agent           *agent.Manager
	History         *history.Manager
	Batch           *batch.Manager
	GC              *container.GarbageCollector
	Settings        *settings.Manager
	Cron            *cron.Manager
	Channel         *channel.Manager
	FeishuChannel   *feishu.Channel
	WecomChannel    *wecom.Channel
	DingtalkChannel *dingtalk.Channel
	Plugin          *plugin.Manager
	Coordinate      *coordinate.Manager
	FilesConfig     config.FilesConfig
	FileStore       FileStore
	OAuthSync       *oauth.SyncManager
}

// NewServer 创建服务器
func NewServer(deps *Deps) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(corsMiddleware())
	engine.Use(loggerMiddleware())

	authHandler := NewAuthHandler(deps.Auth)
	handler := NewHandler(deps.Session, deps.Registry)
	fileHandler := NewFileHandler(deps.Session)
	publicFileHandler := NewPublicFileHandler(deps.FilesConfig, deps.FileStore)
	wsHandler := NewWSHandler(deps.Session, deps.Registry, deps.Container)
	providerHandler := NewProviderHandler(deps.Provider)
	runtimeHandler := NewRuntimeHandler(deps.Runtime)
	mcpHandler := NewMCPHandler(deps.MCP)
	skillHandler := NewSkillHandler(deps.Skill)
	imageHandler := NewImageHandler(deps.Container)
	systemHandler := NewSystemHandler(deps.Container, deps.Session, deps.Batch, deps.GC)
	taskHandler := NewTaskHandler(deps.Task)
	webhookHandler := NewWebhookHandler(deps.Webhook)
	agentHandler := NewAgentHandler(deps.Agent, deps.Session, deps.History)
	historyHandler := NewHistoryHandler(deps.History)
	dashboardHandler := NewDashboardHandler(deps.Task, deps.Agent, deps.Session, deps.Provider, deps.MCP, deps.Container, deps.History)
	batchHandler := NewBatchHandler(deps.Batch)
	settingsHandler := NewSettingsHandler(deps.Settings)
	cronHandler := NewCronHandler(deps.Cron)
	channelHandler := NewChannelHandler(deps.Channel, deps.FeishuChannel, deps.WecomChannel, deps.DingtalkChannel)
	feishuHandler := NewFeishuHandler()
	wecomHandler := NewWecomHandler()
	dingtalkHandler := NewDingtalkHandler()
	coordinateHandler := NewCoordinateHandler(deps.Coordinate)
	gatewayHandler := NewGatewayHandler(deps.Auth, deps.Task)
	oauthSyncHandler := NewOAuthSyncAPI(deps.OAuthSync, deps.Provider)
	zhipuCompatHandler := NewZhipuCompatHandler(deps.Provider)
	compatHandler := NewCompatHandler(deps.Provider)

	s := &Server{
		engine:             engine,
		authHandler:        authHandler,
		authManager:        deps.Auth,
		handler:            handler,
		fileHandler:        fileHandler,
		publicFileHandler:  publicFileHandler,
		wsHandler:          wsHandler,
		providerHandler:    providerHandler,
		runtimeHandler:     runtimeHandler,
		mcpHandler:         mcpHandler,
		skillHandler:       skillHandler,
		imageHandler:       imageHandler,
		systemHandler:      systemHandler,
		taskHandler:        taskHandler,
		webhookHandler:     webhookHandler,
		agentHandler:       agentHandler,
		historyHandler:     historyHandler,
		dashboardHandler:   dashboardHandler,
		batchHandler:       batchHandler,
		settingsHandler:    settingsHandler,
		cronHandler:        cronHandler,
		channelHandler:     channelHandler,
		feishuHandler:      feishuHandler,
		wecomHandler:       wecomHandler,
		dingtalkHandler:    dingtalkHandler,
		coordinateHandler:  coordinateHandler,
		gatewayHandler:     gatewayHandler,
		oauthSyncHandler:   oauthSyncHandler,
		zhipuCompatHandler: zhipuCompatHandler,
		compatHandler:      compatHandler,
	}

	s.setupRoutes()

	// 启动文件清理
	publicFileHandler.StartCleanup(deps.FilesConfig.CleanupInterval)

	return s
}

// setupRoutes 设置路由
func (s *Server) setupRoutes() {
	v1 := s.engine.Group("/api/v1")

	// ==================== 公开路由（无需认证）====================
	v1.GET("/health", s.handler.HealthCheck)
	v1.POST("/auth/login", s.authHandler.Login)
	// Zhipu OpenAI-compatible proxy (role rewrite)
	v1.Any("/compat/zhipu/*path", s.zhipuCompatHandler.Proxy)
	// Provider-agnostic compat proxy
	v1.Any("/compat/openai/:provider/*path", s.compatHandler.ProxyOpenAI)
	v1.Any("/compat/anthropic/:provider/*path", s.compatHandler.ProxyAnthropic)

	// WebSocket Gateway（通过消息认证，不需要 HTTP 层认证）
	s.gatewayHandler.RegisterPublicRoutes(v1)

	// 通道 Webhook 回调（飞书等）
	s.channelHandler.RegisterWebhookRoutes(v1)

	// ==================== 认证路由 ====================
	authenticated := v1.Group("")
	authenticated.Use(authMiddleware(s.authManager))
	{
		// Auth - 当前用户信息 & 修改密码
		authenticated.GET("/auth/me", s.authHandler.Me)
		authenticated.PUT("/auth/password", s.authHandler.ChangePassword)

		// API Keys - 用户自己的 API Key 管理
		authenticated.POST("/auth/api-keys", s.authHandler.CreateAPIKey)
		authenticated.GET("/auth/api-keys", s.authHandler.ListAPIKeys)
		authenticated.DELETE("/auth/api-keys/:id", s.authHandler.DeleteAPIKey)

		// Tasks (核心) - 创建/多轮/取消/SSE 事件流
		s.taskHandler.RegisterRoutes(authenticated)

		// Batches (批量任务) - Worker 池模式批量处理
		s.batchHandler.RegisterRoutes(authenticated)

		// Files (附件) - 独立文件上传
		s.publicFileHandler.RegisterRoutes(authenticated)

		// Agents (只读) - 列表可用 Agent
		authenticated.GET("/agents", s.agentHandler.ListPublic)
		authenticated.GET("/agents/:id", s.agentHandler.GetPublic)

		// Webhooks (CRUD) - Webhook 管理
		s.webhookHandler.RegisterRoutes(authenticated)

		// OAuth Sync - OAuth 令牌同步
		s.oauthSyncHandler.RegisterRoutes(authenticated)

		// Engines (只读) - 底层引擎适配器列表
		authenticated.GET("/engines", s.handler.ListAgents)

		// History (只读) - 执行历史记录
		s.historyHandler.RegisterRoutes(authenticated)

		// Coordinate (跨会话协调) - Agent 可用
		s.coordinateHandler.RegisterRoutes(authenticated)

		// System Health (所有认证用户可访问)
		authenticated.GET("/system/health", s.systemHandler.Health)
	}

	// ==================== Admin API（需要 admin 角色）====================
	admin := v1.Group("/admin")
	admin.Use(authMiddleware(s.authManager))
	admin.Use(adminOnly())
	{
		// Users 管理
		admin.POST("/users", s.authHandler.CreateUser)
		admin.GET("/users", s.authHandler.ListUsers)
		admin.DELETE("/users/:id", s.authHandler.DeleteUser)

		// Sessions (调试用) - 完整的 Session CRUD
		sessions := admin.Group("/sessions")
		{
			sessions.POST("", s.handler.CreateSession)
			sessions.GET("", s.handler.ListSessions)
			sessions.GET("/:id", s.handler.GetSession)
			sessions.DELETE("/:id", s.handler.DeleteSession)
			sessions.POST("/:id/start", s.handler.StartSession)
			sessions.POST("/:id/stop", s.handler.StopSession)
			sessions.POST("/:id/exec", s.handler.ExecSession)
			sessions.POST("/:id/exec/stream", s.handler.ExecSessionStream)
			sessions.GET("/:id/executions", s.handler.GetExecutions)
			sessions.GET("/:id/executions/:execId", s.handler.GetExecution)
			sessions.GET("/:id/logs", s.handler.GetSessionLogs)
			sessions.GET("/:id/logs/stream", s.handler.StreamSessionLogs)

			// Session 文件管理
			sessions.GET("/:id/files", s.fileHandler.ListFiles)
			sessions.GET("/:id/files/*path", s.fileHandler.DownloadFile)
			sessions.POST("/:id/files", s.fileHandler.UploadFile)
			sessions.DELETE("/:id/files/*path", s.fileHandler.DeleteFile)
			sessions.POST("/:id/directories", s.fileHandler.CreateDirectory)

			// WebSocket 流式执行
			sessions.GET("/:id/stream", s.wsHandler.ExecStream)
		}

		// Agents (完整 CRUD) - 管理 Agent 配置
		s.agentHandler.RegisterRoutes(admin)

		// Providers (CRUD + Key 管理)
		s.providerHandler.RegisterRoutes(admin)

		// Runtimes 管理
		s.runtimeHandler.RegisterRoutes(admin)

		// MCP Servers 管理
		s.mcpHandler.RegisterRoutes(admin)

		// Skills 管理
		s.skillHandler.RegisterRoutes(admin)

		// Images 管理
		s.imageHandler.RegisterRoutes(admin)

		// System 管理
		s.systemHandler.RegisterRoutes(admin)

		// Dashboard (态势感知大屏)
		s.dashboardHandler.RegisterRoutes(admin)

		// Settings (业务配置)
		s.settingsHandler.RegisterRoutes(admin)

		// Cron (定时任务)
		s.cronHandler.RegisterRoutes(admin)

		// Channels (多通道)
		s.channelHandler.RegisterRoutes(admin)

		// Feishu Config (飞书配置)
		s.feishuHandler.RegisterRoutes(admin)

		// WeCom Config (企业微信配置)
		s.wecomHandler.RegisterRoutes(admin)

		// DingTalk Config (钉钉配置)
		s.dingtalkHandler.RegisterRoutes(admin)

		// Gateway (WebSocket 统计)
		s.gatewayHandler.RegisterRoutes(admin)
	}
}

// Run 启动服务器
func (s *Server) Run(addr string) error {
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: s.engine,
	}
	return s.httpServer.ListenAndServe()
}

// Shutdown 优雅关闭服务器
func (s *Server) Shutdown(ctx context.Context) error {
	if s.gatewayHandler != nil {
		s.gatewayHandler.Stop()
	}
	if s.publicFileHandler != nil {
		s.publicFileHandler.Stop()
	}
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// Engine 获取 Gin 引擎 (用于测试)
func (s *Server) Engine() *gin.Engine {
	return s.engine
}
