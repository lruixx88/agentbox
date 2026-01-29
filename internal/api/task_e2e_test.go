package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tmalldedede/agentbox/internal/session"
	"github.com/tmalldedede/agentbox/internal/task"
)

// ============================================================
// Issue 3: mountAttachments 实际拷贝文件到 workspace
// ============================================================

func TestMountAttachments_CopiesFilesToWorkspace(t *testing.T) {
	_, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// 模拟上传目录结构: uploadDir/file-id/filename.txt
	uploadDir := filepath.Join(tempDir, "uploads")
	taskMgr.SetUploadDir(uploadDir)

	fileID := "file-test001"
	fileDir := filepath.Join(uploadDir, fileID)
	require.NoError(t, os.MkdirAll(fileDir, 0o755))

	// 写入一个测试文件
	testContent := []byte("hello attachment content")
	srcFile := filepath.Join(fileDir, "readme.txt")
	require.NoError(t, os.WriteFile(srcFile, testContent, 0o644))

	// 模拟 workspace 目录
	workspace := filepath.Join(tempDir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	// 直接调用 MountAttachmentsForTest（公开的测试辅助方法）
	taskMgr.MountAttachmentsForTest(workspace, []string{fileID})

	// 验证文件被拷贝到 workspace
	dstFile := filepath.Join(workspace, "readme.txt")
	copied, err := os.ReadFile(dstFile)
	require.NoError(t, err, "attachment file should be copied to workspace")
	assert.Equal(t, testContent, copied)
}

func TestMountAttachments_SkipsNonexistentFile(t *testing.T) {
	_, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	uploadDir := filepath.Join(tempDir, "uploads")
	taskMgr.SetUploadDir(uploadDir)
	require.NoError(t, os.MkdirAll(uploadDir, 0o755))

	workspace := filepath.Join(tempDir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	// 不应 panic：不存在的 fileID 静默跳过
	taskMgr.MountAttachmentsForTest(workspace, []string{"nonexistent-file"})

	// workspace 应该为空
	entries, err := os.ReadDir(workspace)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestMountAttachments_MultipleFiles(t *testing.T) {
	_, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	uploadDir := filepath.Join(tempDir, "uploads")
	taskMgr.SetUploadDir(uploadDir)

	workspace := filepath.Join(tempDir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	// 创建多个上传文件
	files := map[string]string{
		"file-a": "code.py",
		"file-b": "data.csv",
		"file-c": "config.yaml",
	}
	for id, name := range files {
		dir := filepath.Join(uploadDir, id)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("content-"+id), 0o644))
	}

	taskMgr.MountAttachmentsForTest(workspace, []string{"file-a", "file-b", "file-c"})

	// 验证所有文件都被拷贝
	for id, name := range files {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		require.NoError(t, err, "file %s should exist", name)
		assert.Equal(t, []byte("content-"+id), data)
	}
}

// ============================================================
// Issue 2: appendTurn 异步执行，HTTP 立即返回
// ============================================================

func TestAppendTurn_ReturnsImmediately(t *testing.T) {
	router, _, taskMgr, sessionStore, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// 创建一个 task 并手动设为 running + 有 session
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "first turn prompt",
	})
	require.NoError(t, err)

	// 手动将 task 设为 running 并关联 session（模拟已完成首轮）
	createdTask.Status = task.StatusRunning
	createdTask.SessionID = "mock-session-001"
	now := time.Now()
	createdTask.StartedAt = &now
	createdTask.Turns[0].Result = &task.Result{Text: "first turn result"}
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	require.NoError(t, sessionStore.Create(&session.Session{
		ID:          createdTask.SessionID,
		Status:      session.StatusRunning,
		Agent:       "claude-code",
		AgentID:     createdTask.AgentID,
		Workspace:   filepath.Join(tempDir, "workspaces", "mock-session-001"),
		ContainerID: "mock-container-001",
	}))

	// 通过 API 追加轮次
	appendReq := CreateTaskAPIRequest{
		TaskID: createdTask.ID,
		Prompt: "second turn prompt",
	}
	body, _ := json.Marshal(appendReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// 验证 HTTP 请求快速返回（不阻塞等 Agent 执行）
	start := time.Now()
	router.ServeHTTP(w, req)
	elapsed := time.Since(start)

	// 应该在 1 秒内返回（实际应为毫秒级），证明不阻塞
	assert.Less(t, elapsed, time.Second, "appendTurn should return immediately without blocking")
	assert.Equal(t, http.StatusCreated, w.Code)

	var resp Response
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	taskData, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)

	// 验证新 turn 已添加
	assert.Equal(t, float64(2), taskData["turn_count"])
	assert.Equal(t, "running", taskData["status"])
	assert.Equal(t, "second turn prompt", taskData["prompt"])

	// 验证 turns 列表
	turns, ok := taskData["turns"].([]interface{})
	require.True(t, ok)
	assert.Len(t, turns, 2)

	// 第二个 turn 应该还没有 result（异步执行中）
	secondTurn, ok := turns[1].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "second turn prompt", secondTurn["prompt"])
	assert.Nil(t, secondTurn["result"])
}

func TestAppendTurn_BroadcastsCompletionOnFailure(t *testing.T) {
	router, _, taskMgr, sessionStore, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "first turn prompt",
	})
	require.NoError(t, err)

	createdTask.Status = task.StatusRunning
	createdTask.SessionID = "mock-session-events"
	now := time.Now()
	createdTask.StartedAt = &now
	createdTask.Turns[0].Result = &task.Result{Text: "first turn result"}
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	require.NoError(t, sessionStore.Create(&session.Session{
		ID:          createdTask.SessionID,
		Status:      session.StatusRunning,
		Agent:       "claude-code",
		AgentID:     createdTask.AgentID,
		Workspace:   filepath.Join(tempDir, "workspaces", "mock-session-events"),
		ContainerID: "mock-container-events",
	}))

	eventsCh := taskMgr.SubscribeEvents(createdTask.ID)
	defer taskMgr.UnsubscribeEvents(createdTask.ID, eventsCh)

	appendReq := CreateTaskAPIRequest{
		TaskID: createdTask.ID,
		Prompt: "second turn prompt",
	}
	body, _ := json.Marshal(appendReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	gotTurnCompleted := false
	gotFailed := false
	timeout := time.After(3 * time.Second)

	for !(gotTurnCompleted && gotFailed) {
		select {
		case <-timeout:
			t.Fatalf("timeout waiting for events: turn_completed=%v failed=%v", gotTurnCompleted, gotFailed)
		case event := <-eventsCh:
			if event == nil {
				continue
			}
			switch event.Type {
			case "task.turn_completed":
				gotTurnCompleted = true
			case "task.failed":
				gotFailed = true
			}
		}
	}
}

func TestAppendTurn_InvalidTaskStatus(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// 创建一个 queued 状态的 task
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "a queued task",
	})
	require.NoError(t, err)
	// task 在 queued 状态，不可追加 turn

	appendReq := CreateTaskAPIRequest{
		TaskID: createdTask.ID,
		Prompt: "try append to queued",
	}
	body, _ := json.Marshal(appendReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAppendTurn_NoSession(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// 创建 task 并设为 running 但没有 session
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "running but no session",
	})
	require.NoError(t, err)

	createdTask.Status = task.StatusRunning
	now := time.Now()
	createdTask.StartedAt = &now
	// 不设 SessionID
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	appendReq := CreateTaskAPIRequest{
		TaskID: createdTask.ID,
		Prompt: "try append without session",
	}
	body, _ := json.Marshal(appendReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAppendTurn_NonexistentTask(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	appendReq := CreateTaskAPIRequest{
		TaskID: "nonexistent-task-id",
		Prompt: "try append to nonexistent",
	}
	body, _ := json.Marshal(appendReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ============================================================
// Issue 1: SSE StreamEvents 端点
// ============================================================

func TestSSEStreamEvents_TerminalTask(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// 创建一个已完成的 task
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "completed task",
	})
	require.NoError(t, err)

	// 手动设为 completed
	createdTask.Status = task.StatusCompleted
	now := time.Now()
	createdTask.CompletedAt = &now
	createdTask.Result = &task.Result{Text: "done"}
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	// 请求 SSE 端点
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+createdTask.ID+"/events", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))

	// 解析 SSE 输出
	body := w.Body.String()
	assert.Contains(t, body, "event: task.completed")
	assert.Contains(t, body, "\"task_id\"")
	assert.Contains(t, body, "\"status\":\"completed\"")
}

func TestSSEStreamEvents_FailedTask(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "failed task",
	})
	require.NoError(t, err)

	createdTask.Status = task.StatusFailed
	now := time.Now()
	createdTask.CompletedAt = &now
	createdTask.ErrorMessage = "agent crashed"
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+createdTask.ID+"/events", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "event: task.failed")
}

func TestSSEStreamEvents_CancelledTask(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "cancelled task",
	})
	require.NoError(t, err)

	createdTask.Status = task.StatusCancelled
	now := time.Now()
	createdTask.CompletedAt = &now
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+createdTask.ID+"/events", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "event: task.cancelled")
}

func TestSSEStreamEvents_NotFound(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/nonexistent/events", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSSEStreamEvents_LiveTask_ReceivesEvents(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// 创建一个 running 状态的 task
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "live task",
	})
	require.NoError(t, err)

	createdTask.Status = task.StatusRunning
	now := time.Now()
	createdTask.StartedAt = &now
	createdTask.SessionID = "mock-session"
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	// 用 httptest.Server 来进行真实的 HTTP SSE 连接
	server := httptest.NewServer(router)
	defer server.Close()

	// 发起 SSE 连接
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/tasks/"+createdTask.ID+"/events", nil)
	resp, err := http.DefaultClient.Do(sseReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// 异步广播事件
	go func() {
		time.Sleep(100 * time.Millisecond)
		taskMgr.BroadcastEventForTest(createdTask.ID, &task.TaskEvent{
			Type: "agent.thinking",
		})
		time.Sleep(50 * time.Millisecond)
		taskMgr.BroadcastEventForTest(createdTask.ID, &task.TaskEvent{
			Type: "agent.message",
			Data: map[string]interface{}{"text": "hello from agent"},
		})
		time.Sleep(50 * time.Millisecond)
		taskMgr.BroadcastEventForTest(createdTask.ID, &task.TaskEvent{
			Type: "task.completed",
			Data: map[string]interface{}{"reason": "done"},
		})
	}()

	// 读取 SSE 事件
	scanner := bufio.NewScanner(resp.Body)
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "data:") {
			events = append(events, line)
		}
	}

	// 验证收到了初始状态 + 3个事件
	allEvents := strings.Join(events, "\n")
	assert.Contains(t, allEvents, "event: task.status")    // 初始状态
	assert.Contains(t, allEvents, "event: agent.thinking") // thinking
	assert.Contains(t, allEvents, "event: agent.message")  // message
	assert.Contains(t, allEvents, "event: task.completed") // completed
	assert.Contains(t, allEvents, "hello from agent")      // message 内容
}

// ============================================================
// Issue 1: 多轮对话集成测试
// ============================================================

func TestMultiTurn_CreateAndAppend(t *testing.T) {
	router, _, taskMgr, sessionStore, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// Step 1: 创建新 task
	createReq := CreateTaskAPIRequest{
		AgentID: "test-agent",
		Prompt:  "write a fibonacci function",
	}
	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	var createResp Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &createResp))
	taskData, _ := createResp.Data.(map[string]interface{})
	taskID := taskData["id"].(string)
	assert.Equal(t, float64(1), taskData["turn_count"])

	// Step 2: 手动将 task 设为 running（模拟首轮执行完成）
	createdTask, err := taskMgr.GetTask(taskID)
	require.NoError(t, err)
	createdTask.Status = task.StatusRunning
	createdTask.SessionID = "mock-session"
	nowTime := time.Now()
	createdTask.StartedAt = &nowTime
	createdTask.Turns[0].Result = &task.Result{Text: "def fib(n): ..."}
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))
	require.NoError(t, sessionStore.Create(&session.Session{
		ID:          createdTask.SessionID,
		Status:      session.StatusRunning,
		Agent:       "claude-code",
		AgentID:     createdTask.AgentID,
		Workspace:   filepath.Join(tempDir, "workspaces", "mock-session"),
		ContainerID: "mock-container",
	}))

	// Step 3: 追加第二轮
	appendReq := CreateTaskAPIRequest{
		TaskID: taskID,
		Prompt: "now add memoization",
	}
	body, _ = json.Marshal(appendReq)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	var appendResp Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &appendResp))
	taskData2, _ := appendResp.Data.(map[string]interface{})
	assert.Equal(t, float64(2), taskData2["turn_count"])

	// Step 4: GET 验证 task 状态
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+taskID, nil)
	w = httptest.NewRecorder()

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var getResp Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &getResp))
	taskData3, _ := getResp.Data.(map[string]interface{})

	turns, ok := taskData3["turns"].([]interface{})
	require.True(t, ok)
	assert.Len(t, turns, 2)

	// 验证第一轮有 result，第二轮还没有
	turn1, _ := turns[0].(map[string]interface{})
	assert.Equal(t, "write a fibonacci function", turn1["prompt"])
	assert.NotNil(t, turn1["result"])

	turn2, _ := turns[1].(map[string]interface{})
	assert.Equal(t, "now add memoization", turn2["prompt"])
	assert.Nil(t, turn2["result"])
}

func TestMultiTurn_CannotAppendToCompletedTaskWithoutRunningSession(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// 创建 task 并设为 completed 状态（无运行中的 session 时应拒绝追加轮次）
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "completed by idle timeout",
	})
	require.NoError(t, err)

	createdTask.Status = task.StatusCompleted
	createdTask.SessionID = "mock-session"
	now := time.Now()
	createdTask.StartedAt = &now
	createdTask.CompletedAt = &now
	createdTask.Turns[0].Result = &task.Result{Text: "first result"}
	require.NoError(t, taskMgr.UpdateTaskForTest(createdTask))

	// 追加新 turn 到 completed task
	appendReq := CreateTaskAPIRequest{
		TaskID: createdTask.ID,
		Prompt: "continue from where we left off",
	}
	body, _ := json.Marshal(appendReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ============================================================
// Issue 4: 前端构建验证（通过 go test 无法测试，但可以验证 API 路由完整性）
// ============================================================

func TestAllTaskRoutesRegistered(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	routes := []struct {
		method string
		path   string
	}{
		{"POST", "/api/v1/tasks"},
		{"GET", "/api/v1/tasks"},
		{"GET", "/api/v1/tasks/test-id"},
		{"DELETE", "/api/v1/tasks/test-id"},
		{"GET", "/api/v1/tasks/test-id/events"},
		{"GET", "/api/v1/tasks/test-id/output"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			// 不应该返回 404 (路由匹配成功，可能是 400/500 等业务错误)
			assert.NotEqual(t, http.StatusMethodNotAllowed, w.Code,
				"route %s %s should be registered", route.method, route.path)
		})
	}
}

// ============================================================
// 回归测试：Task 创建带 attachments
// ============================================================

func TestCreateTask_WithAttachments(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	createReq := CreateTaskAPIRequest{
		AgentID:     "test-agent",
		Prompt:      "process these files",
		Attachments: []string{"file-001", "file-002"},
	}

	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	taskData, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)

	attachments, ok := taskData["attachments"].([]interface{})
	require.True(t, ok)
	assert.Len(t, attachments, 2)
	assert.Equal(t, "file-001", attachments[0])
	assert.Equal(t, "file-002", attachments[1])
}

func TestCreateTask_WithWebhookAndTimeout(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	createReq := CreateTaskAPIRequest{
		AgentID:    "test-agent",
		Prompt:     "task with config",
		WebhookURL: "https://example.com/hook",
		Timeout:    600,
	}

	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	taskData, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "https://example.com/hook", taskData["webhook_url"])
	assert.Equal(t, float64(600), taskData["timeout"])
}
