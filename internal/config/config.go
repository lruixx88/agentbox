package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config 应用配置
type Config struct {
	Server    ServerConfig    `json:"server"`
	Container ContainerConfig `json:"container"`
	Storage   StorageConfig   `json:"storage"`
	Database  DatabaseConfig  `json:"database"`
	Files     FilesConfig     `json:"files"`
	Redis     RedisConfig     `json:"redis"`
	Runtime   RuntimeConfig   `json:"runtime"`
}

// RuntimeConfig 运行时镜像配置
type RuntimeConfig struct {
	DefaultImage  string `json:"default_image"`   // 默认运行时镜像
	LightImage    string `json:"light_image"`     // 轻量运行时镜像
	HeavyImage    string `json:"heavy_image"`     // 高性能运行时镜像
	BinaryREImage string `json:"binary_re_image"` // 二进制逆向分析镜像
}

// RedisConfig Redis 配置
type RedisConfig struct {
	Enabled         bool          `json:"enabled"`          // 是否启用 Redis 队列
	Addr            string        `json:"addr"`             // Redis 地址 host:port
	Password        string        `json:"password"`         // 密码
	DB              int           `json:"db"`               // 数据库编号
	PoolSize        int           `json:"pool_size"`        // 连接池大小
	ClaimTimeout    time.Duration `json:"claim_timeout"`    // 任务认领超时（超时后重新入队）
	RecoverInterval time.Duration `json:"recover_interval"` // 恢复扫描间隔
}

// FilesConfig 文件上传配置
type FilesConfig struct {
	UploadDir       string        `json:"upload_dir"`       // 上传文件存储目录
	RetentionHours  int           `json:"retention_hours"`  // 文件保留时长（小时），0 表示永不过期
	CleanupInterval time.Duration `json:"cleanup_interval"` // 清理扫描间隔
	MaxFileSize     int64         `json:"max_file_size"`    // 单文件最大大小 (bytes)
}

// ServerConfig HTTP 服务器配置
type ServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// ContainerConfig 容器默认配置
type ContainerConfig struct {
	CPULimit       float64       `json:"cpu_limit"`        // CPU 核心数
	MemoryLimit    int64         `json:"memory_limit"`     // 内存限制 (bytes)
	DiskLimit      int64         `json:"disk_limit"`       // 磁盘限制 (bytes)
	Timeout        time.Duration `json:"timeout"`          // 执行超时
	NetworkMode    string        `json:"network_mode"`     // 网络模式
	WorkspaceBase  string        `json:"workspace_base"`   // 工作空间基础目录
	GCInterval     time.Duration `json:"gc_interval"`      // GC 扫描间隔
	ContainerTTL   time.Duration `json:"container_ttl"`    // 容器最大存活时间
	IdleTimeout    time.Duration `json:"idle_timeout"`     // Stopped 状态后多久删除
	AutoPullImages bool          `json:"auto_pull_images"` // 镜像不存在时自动拉取
}

// StorageConfig 存储配置
type StorageConfig struct {
	Type string `json:"type"` // sqlite, postgres
	DSN  string `json:"dsn"`  // 数据源
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Driver   string `json:"driver"`    // sqlite, postgres
	DSN      string `json:"dsn"`       // 数据源名称
	LogLevel string `json:"log_level"` // silent, error, warn, info
}

// Default 返回默认配置
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 18080,
		},
		Container: ContainerConfig{
			CPULimit:       2.0,
			MemoryLimit:    4 * 1024 * 1024 * 1024,  // 4GB
			DiskLimit:      10 * 1024 * 1024 * 1024, // 10GB
			Timeout:        1 * time.Hour,
			NetworkMode:    "bridge",
			WorkspaceBase:  "data/workspaces",
			GCInterval:     60 * time.Second,
			ContainerTTL:   2 * time.Hour,
			IdleTimeout:    10 * time.Minute,
			AutoPullImages: true,
		},
		Storage: StorageConfig{
			Type: "sqlite",
			DSN:  "agentbox.db",
		},
		Database: DatabaseConfig{
			Driver:   "sqlite",
			DSN:      "", // Will use default path ~/.agentbox/data/agentbox.db
			LogLevel: "warn",
		},
		Files: FilesConfig{
			UploadDir:       "", // 为空时使用 {WorkspaceBase}/uploads
			RetentionHours:  72, // 默认 3 天
			CleanupInterval: 30 * time.Minute,
			MaxFileSize:     100 * 1024 * 1024, // 100MB
		},
		Redis: RedisConfig{
			Enabled:         true, // 默认启用 Redis 队列
			Addr:            "localhost:6379",
			Password:        "",
			DB:              0,
			PoolSize:        10,
			ClaimTimeout:    5 * time.Minute,  // 任务认领 5 分钟超时
			RecoverInterval: 30 * time.Second, // 每 30 秒扫描超时任务
		},
		Runtime: RuntimeConfig{
			DefaultImage:  "agentbox/agent:v2",
			LightImage:    "agentbox/agent:v2",
			HeavyImage:    "agentbox/agent:v2",
			BinaryREImage: "ghcr.io/tmalldedede/agentbox-agent:binary-re",
		},
	}
}

// Load 从环境变量加载配置
func Load() *Config {
	cfg := Default()

	// 从环境变量覆盖
	if host := os.Getenv("AGENTBOX_HOST"); host != "" {
		cfg.Server.Host = host
	}
	if port := os.Getenv("AGENTBOX_PORT"); port != "" {
		if p, err := strconv.Atoi(port); err == nil {
			cfg.Server.Port = p
		}
	}
	if workspace := os.Getenv("AGENTBOX_WORKSPACE_BASE"); workspace != "" {
		cfg.Container.WorkspaceBase = workspace
	}

	// 确保 WorkspaceBase 是绝对路径 (处理 Windows 盘符问题)
	if abs, err := filepath.Abs(cfg.Container.WorkspaceBase); err == nil {
		cfg.Container.WorkspaceBase = abs
	}

	// GC 配置
	if v := os.Getenv("AGENTBOX_GC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Container.GCInterval = d
		}
	}
	if v := os.Getenv("AGENTBOX_CONTAINER_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Container.ContainerTTL = d
		}
	}
	if v := os.Getenv("AGENTBOX_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Container.IdleTimeout = d
		}
	}
	if v := os.Getenv("AGENTBOX_AUTO_PULL_IMAGES"); v != "" {
		cfg.Container.AutoPullImages = v == "true" || v == "1"
	}

	// 文件存储配置
	if v := os.Getenv("AGENTBOX_UPLOAD_DIR"); v != "" {
		cfg.Files.UploadDir = v
	}
	if v := os.Getenv("AGENTBOX_FILE_RETENTION_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil {
			cfg.Files.RetentionHours = h
		}
	}
	if v := os.Getenv("AGENTBOX_FILE_MAX_SIZE"); v != "" {
		if s, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Files.MaxFileSize = s
		}
	}

	// 数据库配置
	if driver := os.Getenv("AGENTBOX_DB_DRIVER"); driver != "" {
		cfg.Database.Driver = driver
	}
	if dsn := os.Getenv("AGENTBOX_DB_DSN"); dsn != "" {
		cfg.Database.DSN = dsn
	}
	if logLevel := os.Getenv("AGENTBOX_DB_LOG_LEVEL"); logLevel != "" {
		cfg.Database.LogLevel = logLevel
	}

	// Redis 配置
	if v := os.Getenv("AGENTBOX_REDIS_ENABLED"); v != "" {
		cfg.Redis.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("AGENTBOX_REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("AGENTBOX_REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("AGENTBOX_REDIS_DB"); v != "" {
		if db, err := strconv.Atoi(v); err == nil {
			cfg.Redis.DB = db
		}
	}
	if v := os.Getenv("AGENTBOX_REDIS_POOL_SIZE"); v != "" {
		if size, err := strconv.Atoi(v); err == nil {
			cfg.Redis.PoolSize = size
		}
	}
	if v := os.Getenv("AGENTBOX_REDIS_CLAIM_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Redis.ClaimTimeout = d
		}
	}

	// Runtime 镜像配置
	if v := os.Getenv("AGENTBOX_RUNTIME_DEFAULT_IMAGE"); v != "" {
		cfg.Runtime.DefaultImage = v
	}
	if v := os.Getenv("AGENTBOX_RUNTIME_LIGHT_IMAGE"); v != "" {
		cfg.Runtime.LightImage = v
	}
	if v := os.Getenv("AGENTBOX_RUNTIME_HEAVY_IMAGE"); v != "" {
		cfg.Runtime.HeavyImage = v
	}
	if v := os.Getenv("AGENTBOX_RUNTIME_BINARY_RE_IMAGE"); v != "" {
		cfg.Runtime.BinaryREImage = v
	}

	return cfg
}
