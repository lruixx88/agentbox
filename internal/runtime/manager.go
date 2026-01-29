package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tmalldedede/agentbox/internal/config"
)

// Manager manages runtime configurations
type Manager struct {
	mu       sync.RWMutex
	runtimes map[string]*AgentRuntime
	dataDir  string
	cfg      *config.Config
}

// NewManager creates a new runtime manager
// cfg 可以为 nil，将使用默认配置
func NewManager(dataDir string, cfg *config.Config) *Manager {
	if cfg == nil {
		cfg = config.Default()
	}

	m := &Manager{
		runtimes: make(map[string]*AgentRuntime),
		dataDir:  dataDir,
		cfg:      cfg,
	}

	os.MkdirAll(dataDir, 0755)

	// Load built-in runtimes from config
	for _, r := range GetBuiltinRuntimes(cfg) {
		cp := *r
		m.runtimes[cp.ID] = &cp
	}

	// Load custom runtimes from disk
	m.loadCustomRuntimes()

	return m
}

// List returns all runtimes
func (m *Manager) List() []*AgentRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*AgentRuntime, 0, len(m.runtimes))
	for _, r := range m.runtimes {
		result = append(result, r)
	}
	return result
}

// Get returns a runtime by ID
func (m *Manager) Get(id string) (*AgentRuntime, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	r, ok := m.runtimes[id]
	if !ok {
		return nil, ErrRuntimeNotFound
	}
	return r, nil
}

// GetDefault returns the default runtime
func (m *Manager) GetDefault() *AgentRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, r := range m.runtimes {
		if r.IsDefault {
			return r
		}
	}
	// Fallback to "default" ID
	if r, ok := m.runtimes["default"]; ok {
		return r
	}
	// Final fallback to first built-in runtime
	builtins := GetBuiltinRuntimes(m.cfg)
	if len(builtins) > 0 {
		return builtins[0]
	}
	return nil
}

// Create creates a new custom runtime
func (m *Manager) Create(r *AgentRuntime) error {
	if err := r.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	r.IsBuiltIn = false
	r.CreatedAt = now
	r.UpdatedAt = now
	m.runtimes[r.ID] = r

	return m.saveCustomRuntimes()
}

// Update updates an existing runtime
func (m *Manager) Update(id string, updates *AgentRuntime) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.runtimes[id]
	if !ok {
		return ErrRuntimeNotFound
	}

	if existing.IsBuiltIn {
		return ErrRuntimeIsBuiltIn
	}

	if updates.Name != "" {
		existing.Name = updates.Name
	}
	if updates.Description != "" {
		existing.Description = updates.Description
	}
	if updates.Image != "" {
		existing.Image = updates.Image
	}
	if updates.CPUs > 0 {
		existing.CPUs = updates.CPUs
	}
	if updates.MemoryMB > 0 {
		existing.MemoryMB = updates.MemoryMB
	}
	if updates.Network != "" {
		existing.Network = updates.Network
	}
	existing.UpdatedAt = time.Now()

	return m.saveCustomRuntimes()
}

// Delete deletes a custom runtime
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.runtimes[id]
	if !ok {
		return ErrRuntimeNotFound
	}
	if r.IsBuiltIn {
		return ErrRuntimeIsBuiltIn
	}

	delete(m.runtimes, id)
	return m.saveCustomRuntimes()
}

// SetPrivileged sets the privileged flag for a runtime
func (m *Manager) SetPrivileged(id string, privileged bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.runtimes[id]
	if !ok {
		return ErrRuntimeNotFound
	}
	if existing.IsBuiltIn {
		return ErrRuntimeIsBuiltIn
	}

	existing.Privileged = privileged
	existing.UpdatedAt = time.Now()
	return m.saveCustomRuntimes()
}

// SetDefault sets a runtime as the default
func (m *Manager) SetDefault(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.runtimes[id]; !ok {
		return ErrRuntimeNotFound
	}

	// 取消所有 IsDefault 标记
	for _, rt := range m.runtimes {
		rt.IsDefault = false
	}
	// 设置新的默认
	m.runtimes[id].IsDefault = true

	return m.savePersisted()
}

// --- Storage ---

// persistedData 持久化格式（包含 default_id 和自定义运行时）
type persistedData struct {
	DefaultID string          `json:"default_id,omitempty"`
	Runtimes  []*AgentRuntime `json:"runtimes"`
}

func (m *Manager) customRuntimesFile() string {
	return filepath.Join(m.dataDir, "runtimes.json")
}

func (m *Manager) loadCustomRuntimes() {
	data, err := os.ReadFile(m.customRuntimesFile())
	if err != nil {
		return
	}
	builtinIDs := m.builtinIDSet()

	// 尝试新格式
	var persisted persistedData
	if err := json.Unmarshal(data, &persisted); err == nil && persisted.Runtimes != nil {
		for _, r := range persisted.Runtimes {
			if builtinIDs[r.ID] {
				continue
			}
			r.IsBuiltIn = false
			m.runtimes[r.ID] = r
		}
		// 应用 default_id
		if persisted.DefaultID != "" {
			for _, rt := range m.runtimes {
				rt.IsDefault = false
			}
			if rt, ok := m.runtimes[persisted.DefaultID]; ok {
				rt.IsDefault = true
			}
		}
		return
	}

	// 回退旧格式（纯数组）
	var runtimes []*AgentRuntime
	if err := json.Unmarshal(data, &runtimes); err != nil {
		return
	}
	for _, r := range runtimes {
		if builtinIDs[r.ID] {
			continue
		}
		r.IsBuiltIn = false
		m.runtimes[r.ID] = r
	}
}

func (m *Manager) builtinIDSet() map[string]bool {
	ids := make(map[string]bool)
	for _, r := range GetBuiltinRuntimes(m.cfg) {
		ids[r.ID] = true
	}
	return ids
}

// savePersisted 保存完整持久化数据（default_id + 自定义运行时）
func (m *Manager) savePersisted() error {
	var custom []*AgentRuntime
	for _, r := range m.runtimes {
		if !r.IsBuiltIn {
			custom = append(custom, r)
		}
	}

	// 找到当前默认 ID
	defaultID := ""
	for _, r := range m.runtimes {
		if r.IsDefault {
			defaultID = r.ID
			break
		}
	}

	// 如果没有自定义运行时且默认 ID 是内置的 "default"，可以清除文件
	if len(custom) == 0 && (defaultID == "" || defaultID == "default") {
		os.Remove(m.customRuntimesFile())
		return nil
	}

	persisted := persistedData{
		DefaultID: defaultID,
		Runtimes:  custom,
	}

	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(m.customRuntimesFile(), data, 0644)
}

func (m *Manager) saveCustomRuntimes() error {
	return m.savePersisted()
}
