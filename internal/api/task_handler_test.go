package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tmalldedede/agentbox/internal/agent"
	"github.com/tmalldedede/agentbox/internal/container"
	"github.com/tmalldedede/agentbox/internal/engine"
	"github.com/tmalldedede/agentbox/internal/provider"
	"github.com/tmalldedede/agentbox/internal/session"
	"github.com/tmalldedede/agentbox/internal/task"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func setupTaskTestRouter(t *testing.T) (*gin.Engine, *TaskHandler, *task.Manager, *session.MemoryStore, string) {
	// Create temp directory for test data
	tempDir, err := os.MkdirTemp("", "task_test")
	require.NoError(t, err)

	// Create provider manager with a test provider
	providerDataDir := filepath.Join(tempDir, "providers")
	providerMgr := provider.NewManager(providerDataDir, "test-key-32bytes-for-aes256!!")
	providerMgr.Create(&provider.Provider{
		ID:     "test-provider",
		Name:   "Test Provider",
		Agents: []string{"claude-code"},
	})

	// Create agent manager with a test agent
	agentDataDir := filepath.Join(tempDir, "agents")
	agentMgr := agent.NewManager(agentDataDir, providerMgr, nil, nil, nil)

	// Create a test agent
	testAgent := &agent.Agent{
		ID:         "test-agent",
		Name:       "Test Agent",
		Adapter:    "claude-code",
		ProviderID: "test-provider",
		Status:     "active",
	}
	err = agentMgr.Create(testAgent)
	require.NoError(t, err)

	// Create task store (GORM with SQLite)
	dbPath := filepath.Join(tempDir, "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	require.NoError(t, err)
	store, err := task.NewGormStore(db)
	require.NoError(t, err)

	// Create session manager with noop container manager
	sessionStore := session.NewMemoryStore()
	sessionMgr := session.NewManager(sessionStore, container.NewNoopManager(), engine.DefaultRegistry(), filepath.Join(tempDir, "workspaces"))
	sessionMgr.SetAgentManager(agentMgr)

	// Create task manager (without starting the scheduler)
	taskMgr := task.NewManager(store, agentMgr, sessionMgr, nil)

	handler := NewTaskHandler(taskMgr)

	router := gin.New()
	v1 := router.Group("/api/v1")
	handler.RegisterRoutes(v1)

	return router, handler, taskMgr, sessionStore, tempDir
}

func TestTaskList(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp Response
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, 0, resp.Code)

	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, data, "tasks")
	assert.Contains(t, data, "total")
}

func TestTaskCreate(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	createReq := CreateTaskAPIRequest{
		AgentID: "test-agent",
		Prompt:  "Hello, write a test",
	}

	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp Response
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, 0, resp.Code)

	taskData, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	assert.NotEmpty(t, taskData["id"])
	assert.Equal(t, "test-agent", taskData["agent_id"])
	assert.Equal(t, "Hello, write a test", taskData["prompt"])
	// Task is immediately queued after creation
	assert.Equal(t, "queued", taskData["status"])
}

func TestTaskCreateWithMetadata(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	createReq := CreateTaskAPIRequest{
		AgentID: "test-agent",
		Prompt:  "Test with metadata",
		Metadata: map[string]string{
			"user_id": "user-123",
			"source":  "api-test",
		},
	}

	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp Response
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	taskData, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)

	metadata, ok := taskData["metadata"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "user-123", metadata["user_id"])
	assert.Equal(t, "api-test", metadata["source"])
}

func TestTaskGet(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// Create a task first
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "Get test task",
	})
	require.NoError(t, err)

	// Get the task
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+createdTask.ID, nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp Response
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	taskData, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, createdTask.ID, taskData["id"])
}

func TestTaskGetNotFound(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/nonexistent-task-id", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var resp Response
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, 404, resp.Code)
}

func TestTaskCancel(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// Create a task first
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "Task to cancel",
	})
	require.NoError(t, err)

	// Cancel the task
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+createdTask.ID+"/cancel", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp Response
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	taskData, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "cancelled", taskData["status"])
}

func TestTaskGetOutput(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// Create a task first
	createdTask, err := taskMgr.CreateTask(&task.CreateTaskRequest{
		AgentID: "test-agent",
		Prompt:  "Task for output",
	})
	require.NoError(t, err)

	// Get output (should return no output message for pending task)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+createdTask.ID+"/output", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp Response
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, data, "status")
	assert.Contains(t, data, "message")
}

func TestTaskListWithFilter(t *testing.T) {
	router, _, taskMgr, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	// Create some tasks
	for i := 0; i < 3; i++ {
		_, err := taskMgr.CreateTask(&task.CreateTaskRequest{
			AgentID: "test-agent",
			Prompt:  "Test task",
		})
		require.NoError(t, err)
	}

	// List with limit
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks?limit=2", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp Response
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)

	tasks, ok := data["tasks"].([]interface{})
	require.True(t, ok)
	assert.Len(t, tasks, 2)

	total, ok := data["total"].(float64)
	require.True(t, ok)
	assert.Equal(t, float64(3), total)
}

func TestTaskValidation(t *testing.T) {
	router, _, _, _, tempDir := setupTaskTestRouter(t)
	defer os.RemoveAll(tempDir)

	tests := []struct {
		name       string
		request    interface{}
		statusCode int
	}{
		{
			name: "missing agent_id",
			request: map[string]string{
				"prompt": "Test",
			},
			statusCode: http.StatusBadRequest,
		},
		{
			name: "missing prompt",
			request: map[string]string{
				"agent_id": "test-agent",
			},
			statusCode: http.StatusBadRequest,
		},
		{
			name: "non-existent agent",
			request: map[string]string{
				"agent_id": "nonexistent-agent",
				"prompt":   "Test",
			},
			statusCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.request)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			assert.Equal(t, tt.statusCode, w.Code)
		})
	}
}
