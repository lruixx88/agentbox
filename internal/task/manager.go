package task

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tmalldedede/agentbox/internal/agent"
	"github.com/tmalldedede/agentbox/internal/apperr"
	"github.com/tmalldedede/agentbox/internal/logger"
	"github.com/tmalldedede/agentbox/internal/session"
)

// 模块日志器
var log *slog.Logger

func init() {
	log = logger.Module("task")
}

// WebhookNotifier 全局 Webhook 通知接口
type WebhookNotifier interface {
	Send(event string, data interface{})
}

// FileBindFunc 文件绑定回调（将 fileID 关联到 taskID）
type FileBindFunc func(fileID, taskID string) error

// FilePathFunc 文件路径解析回调（根据 fileID 获取磁盘路径和文件名）
type FilePathFunc func(fileID string) (path string, filename string, err error)

// Manager 任务管理器
type Manager struct {
	store      Store
	agentMgr   *agent.Manager
	sessionMgr *session.Manager

	// 调度配置
	maxConcurrent int           // 最大并发任务数
	pollInterval  time.Duration // 轮询间隔

	// Idle timeout 配置
	idleTimeout time.Duration          // 默认 30 分钟
	idleTimers  map[string]*time.Timer // taskID → idle timer
	idleMu      sync.Mutex

	// Webhook 通知
	webhookNotifier WebhookNotifier
	webhookClient   *http.Client

	// 文件管理
	fileBinder       FileBindFunc // 创建 task 时绑定附件
	filePathResolver FilePathFunc // 根据 fileID 解析磁盘路径

	// 运行时状态
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	running   map[string]context.CancelFunc // taskID -> cancel func
	runningMu sync.Mutex

	// Lane Queue (串行执行队列，防止同一 Backend 的并发请求)
	laneQueue *LaneQueue

	// 事件广播
	eventSubs   map[string][]chan *TaskEvent // taskID → subscriber channels
	eventSubsMu sync.RWMutex

	// Provider Fallback 执行器
	fallbackExecutor *FallbackExecutor
	providerMgr      ProviderKeyManager
}

// TaskEvent SSE 事件
type TaskEvent struct {
	Type string      `json:"type"` // task.started, agent.thinking, agent.tool_call, agent.message, task.completed, task.failed
	Data interface{} `json:"data,omitempty"`
}

// ManagerConfig 管理器配置
type ManagerConfig struct {
	MaxConcurrent int
	PollInterval  time.Duration
	IdleTimeout   time.Duration
}

// DefaultManagerConfig 默认配置
func DefaultManagerConfig() *ManagerConfig {
	return &ManagerConfig{
		MaxConcurrent: 5,
		PollInterval:  time.Second * 2,
		IdleTimeout:   30 * time.Minute,
	}
}

// NewManager 创建任务管理器
func NewManager(store Store, agentMgr *agent.Manager, sessionMgr *session.Manager, cfg *ManagerConfig) *Manager {
	if cfg == nil {
		cfg = DefaultManagerConfig()
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 5
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())

	return &Manager{
		store:         store,
		agentMgr:      agentMgr,
		sessionMgr:    sessionMgr,
		maxConcurrent: cfg.MaxConcurrent,
		pollInterval:  cfg.PollInterval,
		idleTimeout:   cfg.IdleTimeout,
		idleTimers:    make(map[string]*time.Timer),
		webhookClient: &http.Client{Timeout: 10 * time.Second},
		ctx:           ctx,
		cancel:        cancel,
		running:       make(map[string]context.CancelFunc),
		laneQueue:     NewLaneQueue(nil), // 使用默认配置
		eventSubs:     make(map[string][]chan *TaskEvent),
	}
}

// SetFileBinder 设置文件绑定回调
func (m *Manager) SetFileBinder(fn FileBindFunc) {
	m.fileBinder = fn
}

// SetFilePathResolver 设置文件路径解析回调
func (m *Manager) SetFilePathResolver(fn FilePathFunc) {
	m.filePathResolver = fn
}

// SetWebhookNotifier 设置全局 Webhook 通知器
func (m *Manager) SetWebhookNotifier(notifier WebhookNotifier) {
	m.webhookNotifier = notifier
}

// SetProviderManager 设置 Provider 管理器（用于故障转移）
func (m *Manager) SetProviderManager(mgr ProviderKeyManager) {
	m.providerMgr = mgr
	// 如果 fallback executor 还没有初始化，现在初始化
	if m.fallbackExecutor == nil && m.agentMgr != nil && m.sessionMgr != nil {
		m.fallbackExecutor = NewFallbackExecutor(m.agentMgr, mgr, m.sessionMgr)
	}
}

// GetFallbackStats 获取 fallback 执行器统计信息
func (m *Manager) GetFallbackStats() map[string]interface{} {
	if m.fallbackExecutor == nil {
		return nil
	}
	return m.fallbackExecutor.GetStats()
}

// Start 启动调度器
func (m *Manager) Start() {
	m.recoverStuckTasks()
	m.wg.Add(1)
	go m.scheduler()
	log.Info("task manager started", "max_concurrent", m.maxConcurrent, "poll_interval", m.pollInterval)
}

// Stop 停止调度器
func (m *Manager) Stop() {
	m.cancel()

	// 停止所有 idle timers
	m.idleMu.Lock()
	for _, timer := range m.idleTimers {
		timer.Stop()
	}
	m.idleMu.Unlock()

	m.wg.Wait()
	log.Info("task manager stopped")
}

// recoverStuckTasks 在启动时清理异常 running 任务
func (m *Manager) recoverStuckTasks() {
	if m.sessionMgr == nil {
		log.Warn("recoverStuckTasks: sessionMgr is nil, skipping recovery")
		return
	}

	tasks, err := m.store.List(&ListFilter{Status: []Status{StatusRunning}})
	if err != nil {
		log.Error("recoverStuckTasks: failed to list running tasks", "error", err)
		return
	}

	if len(tasks) == 0 {
		return
	}

	now := time.Now()
	for _, task := range tasks {
		// 超时任务直接标记失败
		timeout := task.Timeout
		if timeout <= 0 {
			timeout = 1800 // 默认 30 分钟
		}
		if task.StartedAt != nil {
			grace := 300 * time.Second
			if now.Sub(*task.StartedAt) > time.Duration(timeout)*time.Second+grace {
				task.Status = StatusFailed
				task.ErrorMessage = "task timed out during recovery"
				task.CompletedAt = &now
				if err := m.store.Update(task); err != nil {
					log.Error("recoverStuckTasks: failed to mark task failed", "task_id", task.ID, "error", err)
				} else {
					log.Warn("recoverStuckTasks: marked task failed", "task_id", task.ID)
				}
				continue
			}
		}

		// 无 session 或 session 非运行状态 -> 重新入队
		if task.SessionID == "" {
			m.requeueTask(task, "missing session_id")
			continue
		}
		sess, err := m.sessionMgr.Get(context.Background(), task.SessionID)
		if err != nil || sess.Status != session.StatusRunning {
			m.requeueTask(task, "session not running")
		}
	}
}

func (m *Manager) requeueTask(task *Task, reason string) {
	now := time.Now()
	task.Status = StatusQueued
	task.QueuedAt = &now
	task.StartedAt = nil
	task.CompletedAt = nil
	task.ErrorMessage = ""
	task.SessionID = ""
	task.ThreadID = ""

	if err := m.store.Update(task); err != nil {
		log.Error("requeueTask: failed", "task_id", task.ID, "reason", reason, "error", err)
		return
	}
	log.Warn("requeueTask: task requeued", "task_id", task.ID, "reason", reason)
}

// scheduler 调度循环
func (m *Manager) scheduler() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.scheduleNext()
		}
	}
}

// scheduleNext 调度下一个任务
func (m *Manager) scheduleNext() {
	m.runningMu.Lock()
	currentRunning := len(m.running)
	m.runningMu.Unlock()

	if currentRunning >= m.maxConcurrent {
		return
	}

	limit := m.maxConcurrent - currentRunning

	// 原子领取等待中的任务（避免多实例重复执行）
	tasks, err := m.store.ClaimQueued(limit)
	if err != nil {
		log.Error("failed to claim queued tasks", "error", err)
		return
	}

	for _, task := range tasks {
		m.runningMu.Lock()
		if len(m.running) >= m.maxConcurrent {
			m.runningMu.Unlock()
			break
		}
		m.runningMu.Unlock()

		m.wg.Add(1)
		go m.executeTask(task)
	}
}

// CreateTaskRequest 创建任务请求（简化版）
type CreateTaskRequest struct {
	// 核心字段
	AgentID string `json:"agent_id,omitempty"` // 首次创建时必填
	Prompt  string `json:"prompt" binding:"required"`
	TaskID  string `json:"task_id,omitempty"` // 多轮时传入已有 task_id

	// 归属用户
	UserID string `json:"-"` // 由中间件注入，不从请求体读取

	// 附件
	Attachments []string `json:"attachments,omitempty"` // file IDs

	// 可选配置
	WebhookURL string            `json:"webhook_url,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// CreateTask 创建任务（或追加多轮）
func (m *Manager) CreateTask(req *CreateTaskRequest) (*Task, error) {
	// 多轮追加：传了 TaskID
	if req.TaskID != "" {
		return m.appendTurn(req)
	}

	// 新建任务
	if req.AgentID == "" {
		return nil, apperr.BadRequestf("agent_id is required for new task")
	}

	ag, err := m.agentMgr.Get(req.AgentID)
	if err != nil {
		return nil, apperr.BadRequestf("agent not found: %s", req.AgentID)
	}
	if ag.Status == "inactive" {
		return nil, apperr.BadRequestf("agent is inactive: %s", req.AgentID)
	}

	now := time.Now()
	task := &Task{
		ID:          "task-" + uuid.New().String()[:8],
		UserID:      req.UserID,
		AgentID:     req.AgentID,
		AgentName:   ag.Name,
		AgentType:   ag.Adapter,
		Prompt:      req.Prompt,
		Attachments: req.Attachments,
		TurnCount:   1,
		Turns: []Turn{
			{
				ID:        "turn-" + uuid.New().String()[:8],
				Prompt:    req.Prompt,
				CreatedAt: now,
			},
		},
		WebhookURL: req.WebhookURL,
		Timeout:    req.Timeout,
		Status:     StatusPending,
		Metadata:   req.Metadata,
		CreatedAt:  now,
	}

	// 保存到数据库
	if err := m.store.Create(task); err != nil {
		return nil, fmt.Errorf("failed to create task: %w", err)
	}

	// 绑定附件文件到 Task
	if m.fileBinder != nil && len(task.Attachments) > 0 {
		for _, fileID := range task.Attachments {
			if err := m.fileBinder(fileID, task.ID); err != nil {
				log.Warn("failed to bind attachment", "file_id", fileID, "task_id", task.ID, "error", err)
			}
		}
	}

	// 立即入队
	task.Status = StatusQueued
	queuedAt := time.Now()
	task.QueuedAt = &queuedAt
	if err := m.store.Update(task); err != nil {
		log.Error("failed to queue task", "task_id", task.ID, "error", err)
	}

	log.Info("task created", "task_id", task.ID, "agent_id", task.AgentID)
	return task, nil
}

// appendTurn 追加对话轮次（多轮对话）— 同步部分立即返回，异步执行 Agent
func (m *Manager) appendTurn(req *CreateTaskRequest) (*Task, error) {
	task, err := m.store.Get(req.TaskID)
	if err != nil {
		return nil, err
	}

	// 验证 task 状态：running / completed 可追加；failed 允许尝试继续（若 session 仍存在）
	if task.Status != StatusRunning && task.Status != StatusCompleted && task.Status != StatusFailed {
		return nil, apperr.BadRequestf("task %s cannot append turn (status: %s)", task.ID, task.Status)
	}

	// 验证 session 存在
	if task.SessionID == "" {
		return nil, apperr.BadRequestf("task %s has no active session", task.ID)
	}
	if m.sessionMgr == nil {
		return nil, apperr.BadRequestf("task %s session manager not available", task.ID)
	}

	// 验证 session 仍在运行，避免追加后无响应
	sess, err := m.sessionMgr.Get(context.Background(), task.SessionID)
	if err != nil || sess.Status != session.StatusRunning {
		return nil, apperr.BadRequestf("task %s session not running; please start a new task", task.ID)
	}

	// 如果是 failed 任务，允许恢复继续对话（重置错误信息与完成时间）
	if task.Status == StatusFailed {
		log.Warn("append turn on failed task, resuming", "task_id", task.ID, "session_id", task.SessionID)
		task.ErrorMessage = ""
		task.CompletedAt = nil
	}

	// 停止 idle timer（新的轮次进来了）
	m.stopIdleTimer(task.ID)

	// 创建新 Turn
	turn := Turn{
		ID:        "turn-" + uuid.New().String()[:8],
		Prompt:    req.Prompt,
		CreatedAt: time.Now(),
	}

	// 更新 task（同步部分：立即持久化新 turn）
	task.Turns = append(task.Turns, turn)
	task.TurnCount = len(task.Turns)
	task.Status = StatusRunning
	task.Prompt = req.Prompt

	if err := m.store.Update(task); err != nil {
		return nil, fmt.Errorf("failed to save turn: %w", err)
	}

	// 异步执行 Agent
	go m.executeTurn(task.ID, turn.ID, req.Prompt)

	log.Info("turn appended (async)", "task_id", task.ID, "turn_id", turn.ID, "turn_count", task.TurnCount)
	return task, nil
}

// executeTurn 异步执行单个 Turn
func (m *Manager) executeTurn(taskID, turnID, prompt string) {
	if m.sessionMgr == nil {
		log.Error("executeTurn: sessionMgr is nil, cannot execute turn", "task_id", taskID)
		m.failTask(taskID, "session manager not available")
		return
	}

	task, err := m.store.Get(taskID)
	if err != nil {
		log.Error("executeTurn: failed to get task", "task_id", taskID, "error", err)
		m.failTask(taskID, "failed to load task")
		return
	}

	// 广播事件
	m.broadcastEvent(taskID, &TaskEvent{Type: "task.turn_started", Data: map[string]interface{}{
		"turn_id": turnID,
		"prompt":  prompt,
	}})

	timeout := task.Timeout
	if timeout == 0 {
		timeout = 1800
	}

	execResp, err := m.sessionMgr.Exec(m.ctx, task.SessionID, &session.ExecRequest{
		Prompt:   prompt,
		Timeout:  timeout,
		ThreadID: task.ThreadID, // 传递 Thread ID 用于 resume 多轮对话
	})
	if err != nil {
		log.Error("executeTurn: exec failed", "task_id", taskID, "turn_id", turnID, "error", err)
		m.updateTurnResult(taskID, turnID, &Result{Text: "exec error: " + err.Error()})
		m.broadcastTurnCompleted(taskID, turnID, task.TurnCount, StatusFailed)
		m.failTask(taskID, "exec error: "+err.Error())
		return
	}

	log.Debug("executeTurn: exec completed",
		"task_id", taskID, "turn_id", turnID,
		"thread_id", task.ThreadID,
		"resp_message", truncateStr(execResp.Message, 200),
		"resp_thread_id", execResp.ThreadID,
		"resp_error", execResp.Error,
		"resp_exit_code", execResp.ExitCode,
	)

	// 保存 Thread ID（首轮执行后从 thread.started 事件获取）
	if execResp.ThreadID != "" && task.ThreadID == "" {
		task.ThreadID = execResp.ThreadID
		if err := m.store.Update(task); err != nil {
			log.Error("executeTurn: failed to save thread_id", "task_id", taskID, "error", err)
		}
		log.Debug("executeTurn: thread_id saved", "task_id", taskID, "thread_id", task.ThreadID)
	}

	// 等待执行完成
	turnSucceeded := false
	result, err := m.waitExecution(m.ctx, task.SessionID, execResp.ExecutionID, time.Duration(timeout)*time.Second)
	if err != nil {
		m.updateTurnResult(taskID, turnID, &Result{Text: err.Error()})
		m.broadcastTurnCompleted(taskID, turnID, task.TurnCount, StatusFailed)
		m.failTask(taskID, err.Error())
	} else {
		m.updateTurnResult(taskID, turnID, result)
		m.broadcastEvent(taskID, &TaskEvent{Type: "agent.message", Data: map[string]interface{}{
			"turn_id": turnID,
			"text":    result.Text,
		}})
		m.broadcastTurnCompleted(taskID, turnID, task.TurnCount, StatusRunning)
		turnSucceeded = true
	}

	// 重置 idle timer
	if turnSucceeded {
		m.resetIdleTimer(taskID)
	}
}

// broadcastTurnCompleted 发送 turn 完成事件（用于多轮对话 UI 收尾）
func (m *Manager) broadcastTurnCompleted(taskID, turnID string, turnCount int, status Status) {
	m.broadcastEvent(taskID, &TaskEvent{
		Type: "task.turn_completed",
		Data: map[string]interface{}{
			"task_id":    taskID,
			"turn_id":    turnID,
			"turn_count": turnCount,
			"status":     status,
		},
	})
}

// updateTurnResult 更新指定 Turn 的执行结果
func (m *Manager) updateTurnResult(taskID, turnID string, result *Result) {
	task, err := m.store.Get(taskID)
	if err != nil {
		log.Error("updateTurnResult: failed to get task", "task_id", taskID, "error", err)
		return
	}

	for i := range task.Turns {
		if task.Turns[i].ID == turnID {
			task.Turns[i].Result = result
			break
		}
	}
	task.Result = result // 最新结果

	if err := m.store.Update(task); err != nil {
		log.Error("updateTurnResult: failed to update", "task_id", taskID, "error", err)
	}
}

// GetTask 获取任务
func (m *Manager) GetTask(id string) (*Task, error) {
	return m.store.Get(id)
}

// ListTasks 列出任务
func (m *Manager) ListTasks(filter *ListFilter) ([]*Task, error) {
	return m.store.List(filter)
}

// CancelTask 取消任务
func (m *Manager) CancelTask(id string) error {
	task, err := m.store.Get(id)
	if err != nil {
		return err
	}

	if !task.CanCancel() {
		return fmt.Errorf("task cannot be cancelled in status: %s", task.Status)
	}

	// 停止 idle timer
	m.stopIdleTimer(id)

	// 如果正在运行，取消执行
	m.runningMu.Lock()
	if cancel, ok := m.running[id]; ok {
		cancel()
		delete(m.running, id)
	}
	m.runningMu.Unlock()

	// 停止关联 session
	if task.SessionID != "" {
		m.sessionMgr.Stop(context.Background(), task.SessionID)
	}

	// 更新状态
	task.Status = StatusCancelled
	now := time.Now()
	task.CompletedAt = &now

	// 广播事件
	m.broadcastEvent(id, &TaskEvent{Type: "task.cancelled"})

	return m.store.Update(task)
}

// SubscribeEvents 订阅任务事件
func (m *Manager) SubscribeEvents(taskID string) <-chan *TaskEvent {
	ch := make(chan *TaskEvent, 100)
	m.eventSubsMu.Lock()
	m.eventSubs[taskID] = append(m.eventSubs[taskID], ch)
	m.eventSubsMu.Unlock()
	return ch
}

// UnsubscribeEvents 取消订阅
func (m *Manager) UnsubscribeEvents(taskID string, ch <-chan *TaskEvent) {
	m.eventSubsMu.Lock()
	defer m.eventSubsMu.Unlock()

	subs := m.eventSubs[taskID]
	for i, sub := range subs {
		if sub == ch {
			m.eventSubs[taskID] = append(subs[:i], subs[i+1:]...)
			close(sub)
			break
		}
	}
	if len(m.eventSubs[taskID]) == 0 {
		delete(m.eventSubs, taskID)
	}
}

// broadcastEvent 广播事件到所有订阅者
func (m *Manager) broadcastEvent(taskID string, event *TaskEvent) {
	m.eventSubsMu.RLock()
	subCount := len(m.eventSubs[taskID])
	m.eventSubsMu.RUnlock()

	log.Info("broadcasting event",
		"task_id", taskID,
		"event_type", event.Type,
		"subscribers", subCount)

	m.eventSubsMu.RLock()
	for _, ch := range m.eventSubs[taskID] {
		select {
		case ch <- event:
		default:
			// channel 满了，跳过
			log.Warn("event channel full, dropping event",
				"task_id", taskID,
				"event_type", event.Type)
		}
	}
	m.eventSubsMu.RUnlock()

	// 通知全局 webhook
	if m.webhookNotifier != nil {
		m.webhookNotifier.Send(event.Type, event.Data)
	}
}

// executeTask 执行任务
func (m *Manager) executeTask(task *Task) {
	defer m.wg.Done()

	ctx, cancel := context.WithCancel(m.ctx)

	// 注册到运行中
	m.runningMu.Lock()
	m.running[task.ID] = cancel
	m.runningMu.Unlock()

	// 更新状态为运行中
	task.Status = StatusRunning
	now := time.Now()
	task.StartedAt = &now
	if err := m.store.Update(task); err != nil {
		log.Error("failed to update task to running", "task_id", task.ID, "error", err)
		cancel()
		return
	}

	// 广播事件
	m.broadcastEvent(task.ID, &TaskEvent{Type: "task.started", Data: map[string]interface{}{
		"task_id":  task.ID,
		"agent_id": task.AgentID,
	}})

	log.Info("executing task", "task_id", task.ID, "agent_id", task.AgentID)

	// 获取 lane key（Provider + Adapter 组合）
	laneKey := m.getLaneKey(task)

	// 通过 lane queue 串行执行（防止同一 backend 的并发请求）
	var err error
	done := m.laneQueue.Enqueue(laneKey, func() {
		err = m.doExecute(ctx, task)
	})
	<-done // 等待 lane 执行完成

	if err != nil {
		// 执行失败 → 终态
		completedAt := time.Now()
		task.CompletedAt = &completedAt
		task.Status = StatusFailed
		task.ErrorMessage = err.Error()
		log.Error("task failed", "task_id", task.ID, "error", err)

		m.broadcastEvent(task.ID, &TaskEvent{Type: "task.failed", Data: map[string]interface{}{
			"error": err.Error(),
		}})

		// 清理
		m.runningMu.Lock()
		delete(m.running, task.ID)
		m.runningMu.Unlock()
		cancel()

		if err := m.store.Update(task); err != nil {
			log.Error("failed to update task result", "task_id", task.ID, "error", err)
		}

		if task.WebhookURL != "" {
			go m.sendWebhook(task)
		}
		return
	}

	// 首轮执行成功 → 保持 running 状态等待多轮
	if err := m.store.Update(task); err != nil {
		log.Error("failed to update task after first turn", "task_id", task.ID, "error", err)
	}

	// 广播 turn_completed 事件，通知前端本轮已完成
	m.broadcastEvent(task.ID, &TaskEvent{
		Type: "task.turn_completed",
		Data: map[string]interface{}{
			"task_id":    task.ID,
			"turn_count": task.TurnCount,
			"status":     task.Status,
		},
	})

	// 启动 idle timer（等待后续轮次或超时自动完成）
	m.resetIdleTimer(task.ID)

	log.Info("task first turn completed, waiting for more turns or idle timeout",
		"task_id", task.ID, "idle_timeout", m.idleTimeout)

	// 注意：不再在这里清理 running map 和 cancel
	// 因为 task 保持 running 状态等待多轮
	// 清理会在 completeTask 或 CancelTask 中进行
}

// doExecute 实际执行任务（首轮）
func (m *Manager) doExecute(ctx context.Context, task *Task) error {
	// 创建新 Session
	if task.AgentID == "" {
		return fmt.Errorf("task has no agent_id, cannot create session")
	}

	// 获取 Agent 配置
	ag, err := m.agentMgr.Get(task.AgentID)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	// 如果启用了 Fallback 且有 FallbackExecutor，使用带故障转移的执行
	if ag.FallbackEnabled && len(ag.FallbackProviderIDs) > 0 && m.fallbackExecutor != nil {
		return m.doExecuteWithFallback(ctx, task, ag)
	}

	// 标准执行流程（无 Fallback）
	return m.doExecuteStandard(ctx, task, ag)
}

// doExecuteWithFallback 使用 Fallback 执行器执行任务
func (m *Manager) doExecuteWithFallback(ctx context.Context, task *Task, ag *agent.Agent) error {
	log.Info("executing task with fallback enabled",
		"task_id", task.ID,
		"agent_id", ag.ID,
		"primary_provider", ag.ProviderID,
		"fallback_providers", ag.FallbackProviderIDs)

	// 广播事件
	m.broadcastEvent(task.ID, &TaskEvent{Type: "agent.thinking"})

	// 使用 FallbackExecutor 执行
	result, err := m.fallbackExecutor.ExecuteWithFallback(ctx, task, ag)
	if err != nil {
		return fmt.Errorf("execution failed: %w", err)
	}

	// 保存 session ID 和 thread ID
	task.SessionID = result.Session.ID
	if result.ThreadID != "" {
		task.ThreadID = result.ThreadID
		log.Debug("doExecuteWithFallback: thread_id saved", "task_id", task.ID, "thread_id", task.ThreadID)
	}

	// 挂载附件到容器工作区（在 session 创建后）
	if len(task.Attachments) > 0 {
		m.mountAttachments(ctx, result.Session, task.Attachments)
	}

	// 等待执行完成
	timeout := task.Timeout
	if timeout == 0 {
		timeout = 1800
	}
	execResult, err := m.waitExecution(ctx, result.Session.ID, result.ExecResponse.ExecutionID, time.Duration(timeout)*time.Second)
	if err != nil {
		m.sessionMgr.Stop(ctx, task.SessionID)
		return err
	}

	// 保存结果
	if len(task.Turns) > 0 {
		task.Turns[0].Result = execResult
	}
	task.Result = execResult

	// 广播执行成功事件
	m.broadcastEvent(task.ID, &TaskEvent{Type: "agent.message", Data: map[string]interface{}{
		"text":          execResult.Text,
		"provider":      result.ProviderID,
		"used_fallback": result.UsedFallback,
	}})

	if result.UsedFallback {
		log.Info("task completed using fallback provider",
			"task_id", task.ID,
			"provider", result.ProviderID,
			"attempts", result.Attempts)
	}

	return nil
}

// doExecuteStandard 标准执行流程（无 Fallback）
func (m *Manager) doExecuteStandard(ctx context.Context, task *Task, ag *agent.Agent) error {
	// 从 Agent 配置获取 workspace
	workspace := ""
	if ag.Workspace != "" {
		workspace = ag.Workspace
	} else {
		workspace = fmt.Sprintf("agent-%s-%s", task.AgentID, task.ID)
	}

	createReq := &session.CreateRequest{
		AgentID:   task.AgentID,
		Workspace: workspace,
	}

	sess, err := m.sessionMgr.Create(ctx, createReq)
	if err != nil {
		// 记录创建 session 失败（可能是 provider 问题）
		m.recordProviderError(ag.ProviderID, err)
		return fmt.Errorf("failed to create session: %w", err)
	}
	task.SessionID = sess.ID

	// 启动 Session
	if err := m.sessionMgr.Start(ctx, sess.ID); err != nil {
		m.recordProviderError(ag.ProviderID, err)
		return fmt.Errorf("failed to start session: %w", err)
	}

	// 挂载附件到容器工作区
	if len(task.Attachments) > 0 {
		m.mountAttachments(ctx, sess, task.Attachments)
	}

	// 广播事件
	m.broadcastEvent(task.ID, &TaskEvent{Type: "agent.thinking"})

	// 执行任务 prompt
	timeout := task.Timeout
	if timeout == 0 {
		timeout = 1800 // 默认 30 分钟
	}

	execResp, err := m.sessionMgr.Exec(ctx, sess.ID, &session.ExecRequest{
		Prompt:  task.Prompt,
		Timeout: timeout,
	})
	if err != nil {
		m.sessionMgr.Stop(ctx, task.SessionID)
		// 记录执行失败（分类错误类型）
		m.recordProviderError(ag.ProviderID, err)
		return fmt.Errorf("failed to execute: %w", err)
	}

	// 保存 Thread ID（首轮执行后从 thread.started 事件获取，用于后续 resume）
	if execResp.ThreadID != "" {
		task.ThreadID = execResp.ThreadID
		log.Debug("doExecuteStandard: thread_id saved", "task_id", task.ID, "thread_id", task.ThreadID)
	}

	// 等待执行完成
	result, err := m.waitExecution(ctx, sess.ID, execResp.ExecutionID, time.Duration(timeout)*time.Second)
	if err != nil {
		m.sessionMgr.Stop(ctx, task.SessionID)
		// 记录执行失败
		m.recordProviderError(ag.ProviderID, err)
		return err
	}

	// 执行成功，记录成功
	m.recordProviderSuccess(ag.ProviderID)

	// 保存结果到首轮 Turn
	if len(task.Turns) > 0 {
		task.Turns[0].Result = result
	}
	task.Result = result

	// 广播事件
	m.broadcastEvent(task.ID, &TaskEvent{Type: "agent.message", Data: map[string]interface{}{
		"text": result.Text,
	}})

	return nil
}

// recordProviderError 记录 provider 执行失败（用于故障转移决策）
func (m *Manager) recordProviderError(providerID string, err error) {
	if m.providerMgr == nil {
		return
	}

	// 分类错误
	fe := apperr.ClassifyError(err)
	if fe == nil {
		return
	}

	// 只有需要 cooldown 的错误才记录
	if fe.ShouldCooldown() {
		m.providerMgr.MarkProfileFailed(providerID, "", string(fe.Reason))
		log.Info("provider error recorded",
			"provider", providerID,
			"reason", fe.Reason,
			"retryable", fe.Retryable)
	}
}

// recordProviderSuccess 记录 provider 执行成功
func (m *Manager) recordProviderSuccess(providerID string) {
	if m.providerMgr == nil {
		return
	}
	m.providerMgr.MarkProfileSuccess(providerID, "")
}

// waitExecution 等待执行完成
func (m *Manager) waitExecution(ctx context.Context, sessionID, executionID string, timeout time.Duration) (*Result, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("task cancelled")
			}
			return nil, fmt.Errorf("task timeout after %v", timeout)

		case <-ticker.C:
			exec, err := m.sessionMgr.GetExecution(ctx, sessionID, executionID)
			if err != nil {
				return nil, fmt.Errorf("failed to get execution: %w", err)
			}

			switch exec.Status {
			case session.ExecutionSuccess:
				return m.collectResult(exec), nil
			case session.ExecutionFailed:
				return nil, fmt.Errorf("execution failed: %s", exec.Error)
			}
			// ExecutionPending 或 ExecutionRunning 继续等待
		}
	}
}

// collectResult 收集执行结果
func (m *Manager) collectResult(exec *session.Execution) *Result {
	result := &Result{
		Summary: "Task completed",
		Text:    exec.Output,
	}

	if exec.EndedAt != nil {
		result.Usage = &Usage{
			DurationSeconds: int(exec.EndedAt.Sub(exec.StartedAt).Seconds()),
		}
	}

	return result
}

// mountAttachments 挂载附件文件到 Session 工作区
func (m *Manager) mountAttachments(ctx context.Context, sess *session.Session, fileIDs []string) {
	if m.filePathResolver == nil {
		log.Warn("filePathResolver not set, skipping attachment mount")
		return
	}

	for _, fileID := range fileIDs {
		srcPath, filename, err := m.filePathResolver(fileID)
		if err != nil {
			log.Warn("attachment file not found", "file_id", fileID, "error", err)
			continue
		}

		dstPath := filepath.Join(sess.Workspace, filename)
		if err := copyFile(srcPath, dstPath); err != nil {
			log.Error("failed to copy attachment", "file_id", fileID, "src", srcPath, "dst", dstPath, "error", err)
			continue
		}
		log.Info("mounted attachment", "file_id", fileID, "dst", dstPath, "session_id", sess.ID)
	}
}

// copyFile 复制文件
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy data: %w", err)
	}
	return out.Close()
}

// truncateStr 截断字符串用于日志
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// Idle Timer 管理

// resetIdleTimer 重置 idle timer
func (m *Manager) resetIdleTimer(taskID string) {
	m.idleMu.Lock()
	defer m.idleMu.Unlock()

	if timer, ok := m.idleTimers[taskID]; ok {
		timer.Stop()
	}
	m.idleTimers[taskID] = time.AfterFunc(m.idleTimeout, func() {
		m.completeTask(taskID, "idle timeout")
	})
}

// stopIdleTimer 停止 idle timer
func (m *Manager) stopIdleTimer(taskID string) {
	m.idleMu.Lock()
	defer m.idleMu.Unlock()

	if timer, ok := m.idleTimers[taskID]; ok {
		timer.Stop()
		delete(m.idleTimers, taskID)
	}
}

// completeTask 完成任务（idle timeout 或手动完成）
func (m *Manager) completeTask(taskID string, reason string) {
	task, err := m.store.Get(taskID)
	if err != nil {
		log.Error("failed to get task for completion", "task_id", taskID, "error", err)
		return
	}

	if task.Status != StatusRunning {
		return // 已经不是 running 状态，不处理
	}

	log.Info("completing task", "task_id", taskID, "reason", reason)

	// 停止关联 session
	if task.SessionID != "" {
		m.sessionMgr.Stop(context.Background(), task.SessionID)
	}

	// 更新状态
	task.Status = StatusCompleted
	now := time.Now()
	task.CompletedAt = &now

	if err := m.store.Update(task); err != nil {
		log.Error("failed to update task to completed", "task_id", taskID, "error", err)
	}

	// 清理 running map
	m.runningMu.Lock()
	if cancel, ok := m.running[taskID]; ok {
		cancel()
		delete(m.running, taskID)
	}
	m.runningMu.Unlock()

	// 清理 idle timer
	m.stopIdleTimer(taskID)

	// 广播事件
	m.broadcastEvent(taskID, &TaskEvent{Type: "task.completed", Data: map[string]interface{}{
		"reason": reason,
	}})

	// 发送 Webhook
	if task.WebhookURL != "" {
		go m.sendWebhook(task)
	}
}

// failTask 标记任务失败并广播失败事件（用于多轮执行失败收尾）
func (m *Manager) failTask(taskID, reason string) {
	task, err := m.store.Get(taskID)
	if err != nil {
		log.Error("failTask: failed to get task", "task_id", taskID, "error", err)
		return
	}

	if task.Status == StatusFailed || task.Status == StatusCancelled || task.Status == StatusCompleted {
		return
	}

	log.Warn("failing task", "task_id", taskID, "reason", reason)

	// 停止关联 session
	if task.SessionID != "" && m.sessionMgr != nil {
		m.sessionMgr.Stop(context.Background(), task.SessionID)
	}

	// 更新状态
	now := time.Now()
	task.Status = StatusFailed
	task.ErrorMessage = reason
	task.CompletedAt = &now

	if err := m.store.Update(task); err != nil {
		log.Error("failTask: failed to update", "task_id", taskID, "error", err)
	}

	// 清理 running map
	m.runningMu.Lock()
	if cancel, ok := m.running[taskID]; ok {
		cancel()
		delete(m.running, taskID)
	}
	m.runningMu.Unlock()

	// 清理 idle timer
	m.stopIdleTimer(taskID)

	// 广播失败事件
	m.broadcastEvent(taskID, &TaskEvent{Type: "task.failed", Data: map[string]interface{}{
		"error": reason,
	}})

	// 发送 Webhook
	if task.WebhookURL != "" {
		go m.sendWebhook(task)
	}
}

// RetryTask 重试失败的任务（创建新任务，复制原任务的 agent + prompt）
func (m *Manager) RetryTask(id string) (*Task, error) {
	oldTask, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}

	if oldTask.Status != StatusFailed && oldTask.Status != StatusCancelled {
		return nil, apperr.BadRequestf("only failed/cancelled tasks can be retried (current: %s)", oldTask.Status)
	}

	return m.CreateTask(&CreateTaskRequest{
		AgentID:     oldTask.AgentID,
		Prompt:      oldTask.Prompt,
		Attachments: oldTask.Attachments,
		WebhookURL:  oldTask.WebhookURL,
		Timeout:     oldTask.Timeout,
		Metadata:    oldTask.Metadata,
	})
}

// GetStats 获取任务统计
func (m *Manager) GetStats() (*TaskStats, error) {
	return m.store.Stats()
}

// DeleteTask 删除任务
func (m *Manager) DeleteTask(id string) error {
	task, err := m.store.Get(id)
	if err != nil {
		return err
	}

	// 只能删除终态任务
	if !task.Status.IsTerminal() {
		return apperr.BadRequestf("cannot delete task in status: %s", task.Status)
	}

	return m.store.Delete(id)
}

// CleanupTasks 清理旧任务
func (m *Manager) CleanupTasks(beforeDays int, statuses []Status) (int, error) {
	if beforeDays <= 0 {
		beforeDays = 7 // 默认清理 7 天前的
	}
	before := time.Now().AddDate(0, 0, -beforeDays)

	if len(statuses) == 0 {
		statuses = []Status{StatusCompleted, StatusFailed, StatusCancelled}
	}

	count, err := m.store.Cleanup(before, statuses)
	if err != nil {
		return 0, err
	}
	log.Info("cleaned up tasks", "count", count, "before_days", beforeDays)
	return count, nil
}

// CountTasks 统计任务数量
func (m *Manager) CountTasks(filter *ListFilter) (int, error) {
	return m.store.Count(filter)
}

// sendWebhook 发送 Per-Task Webhook 通知（POST task 结果到 task.WebhookURL）
func (m *Manager) sendWebhook(task *Task) {
	payload := map[string]interface{}{
		"task_id":   task.ID,
		"status":    string(task.Status),
		"agent_id":  task.AgentID,
		"result":    task.Result,
		"error":     task.ErrorMessage,
		"completed": task.CompletedAt,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Error("webhook: failed to marshal payload", "task_id", task.ID, "error", err)
		return
	}

	req, err := http.NewRequest("POST", task.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Error("webhook: failed to create request", "task_id", task.ID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Task-ID", task.ID)

	resp, err := m.webhookClient.Do(req)
	if err != nil {
		log.Error("webhook: failed to send", "task_id", task.ID, "url", task.WebhookURL, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Debug("webhook: sent", "task_id", task.ID, "status", resp.StatusCode)
	} else {
		log.Warn("webhook: non-2xx response", "task_id", task.ID, "url", task.WebhookURL, "status", resp.StatusCode)
	}
}

// getLaneKey 获取任务的 lane key（Provider + Adapter 组合）
// 同一 lane 的任务将串行执行，防止同一 backend 的并发请求
func (m *Manager) getLaneKey(task *Task) string {
	providerID := ""

	// 从 Agent 配置获取 Provider ID
	if m.agentMgr != nil && task.AgentID != "" {
		if fullConfig, err := m.agentMgr.GetFullConfig(task.AgentID); err == nil && fullConfig.Provider != nil {
			providerID = fullConfig.Provider.ID
		}
	}

	// 如果获取不到 Provider ID，使用 AgentID 作为 fallback
	if providerID == "" {
		providerID = task.AgentID
	}

	return GetLaneKey(providerID, task.AgentType)
}

// GetLaneStats 获取 lane queue 统计信息
func (m *Manager) GetLaneStats() *LaneStats {
	if m.laneQueue == nil {
		return nil
	}
	return m.laneQueue.Stats()
}
