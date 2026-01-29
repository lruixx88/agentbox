package container

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/tmalldedede/agentbox/internal/config"
)

// DockerManager Docker 容器管理器实现
type DockerManager struct {
	client         *client.Client
	autoPullImages bool
}

// NewDockerManager 创建 Docker 管理器
func NewDockerManager() (*DockerManager, error) {
	return NewDockerManagerWithConfig(nil)
}

// NewDockerManagerWithConfig 创建 Docker 管理器（使用配置）
func NewDockerManagerWithConfig(cfg *config.Config) (*DockerManager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	autoPull := false
	if cfg != nil {
		autoPull = cfg.Container.AutoPullImages
	}

	return &DockerManager{client: cli, autoPullImages: autoPull}, nil
}

// Create 创建容器
func (m *DockerManager) Create(ctx context.Context, config *CreateConfig) (*Container, error) {
	if err := m.ensureImage(ctx, config.Image); err != nil {
		return nil, err
	}

	// 构建环境变量
	env := make([]string, 0, len(config.Env))
	for k, v := range config.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	// 构建挂载配置
	mounts := make([]mount.Mount, 0, len(config.Mounts))
	for _, m := range config.Mounts {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}

	// 构建资源限制
	resources := container.Resources{}
	if config.Resources.CPULimit > 0 {
		resources.NanoCPUs = int64(config.Resources.CPULimit * 1e9)
	}
	if config.Resources.MemoryLimit > 0 {
		resources.Memory = config.Resources.MemoryLimit
	}

	// 创建容器
	resp, err := m.client.ContainerCreate(ctx,
		&container.Config{
			Image:  config.Image,
			Cmd:    config.Cmd,
			Env:    env,
			Labels: config.Labels,
			Tty:    true,
		},
		&container.HostConfig{
			Mounts:      mounts,
			Resources:   resources,
			NetworkMode: container.NetworkMode(config.NetworkMode),
			Privileged:  config.Privileged,
		},
		nil,
		nil,
		config.Name,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	return &Container{
		ID:     resp.ID,
		Name:   config.Name,
		Image:  config.Image,
		Status: StatusCreated,
		Labels: config.Labels,
	}, nil
}

func (m *DockerManager) ensureImage(ctx context.Context, imageName string) error {
	if imageName == "" {
		return fmt.Errorf("image name is required")
	}

	_, _, err := m.client.ImageInspectWithRaw(ctx, imageName)
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("failed to inspect image %s: %w", imageName, err)
	}
	if !m.autoPullImages {
		return fmt.Errorf("image not found: %s", imageName)
	}
	if err := m.PullImage(ctx, imageName); err != nil {
		return err
	}
	_, _, err = m.client.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return fmt.Errorf("image not found after pull: %s", imageName)
	}
	return nil
}

// Start 启动容器
func (m *DockerManager) Start(ctx context.Context, containerID string) error {
	if err := m.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	return nil
}

// Stop 停止容器
func (m *DockerManager) Stop(ctx context.Context, containerID string) error {
	timeout := 10 // 10 秒超时
	if err := m.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}
	return nil
}

// Remove 删除容器
func (m *DockerManager) Remove(ctx context.Context, containerID string) error {
	if err := m.client.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}
	return nil
}

// Exec 在容器中执行命令
func (m *DockerManager) Exec(ctx context.Context, containerID string, cmd []string) (*ExecResult, error) {
	// 创建 exec 实例
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	execResp, err := m.client.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec: %w", err)
	}

	// 执行命令
	attachResp, err := m.client.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to attach exec: %w", err)
	}
	defer attachResp.Close()

	// 读取输出 - 使用 stdcopy 来正确解析多路复用的 stdout/stderr
	// 当 Tty=false 时，Docker 使用 8 字节头部的多路复用格式
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read exec output: %w", err)
	}

	// 获取退出码
	inspectResp, err := m.client.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}

	return &ExecResult{
		ExitCode: inspectResp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// ExecStream 在容器中执行命令并流式返回输出
func (m *DockerManager) ExecStream(ctx context.Context, containerID string, cmd []string) (*ExecStream, error) {
	// 创建 exec 实例
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	}

	execResp, err := m.client.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec: %w", err)
	}

	// 执行命令
	attachResp, err := m.client.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{
		Tty: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to attach exec: %w", err)
	}

	// 启动执行
	if err := m.client.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{}); err != nil {
		attachResp.Close()
		return nil, fmt.Errorf("failed to start exec: %w", err)
	}

	done := make(chan struct{})

	return &ExecStream{
		ExecID: execResp.ID,
		Reader: attachResp.Conn,
		Done:   done,
	}, nil
}

// InspectExec 检查 exec 的退出码
func (m *DockerManager) InspectExec(ctx context.Context, execID string) (int, error) {
	inspectResp, err := m.client.ContainerExecInspect(ctx, execID)
	if err != nil {
		return -1, fmt.Errorf("failed to inspect exec: %w", err)
	}
	return inspectResp.ExitCode, nil
}

// Logs 获取容器日志
func (m *DockerManager) Logs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
	}

	reader, err := m.client.ContainerLogs(ctx, containerID, options)
	if err != nil {
		return nil, fmt.Errorf("failed to get container logs: %w", err)
	}

	return reader, nil
}

// Inspect 获取容器信息
func (m *DockerManager) Inspect(ctx context.Context, containerID string) (*Container, error) {
	info, err := m.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	status := parseStatus(info.State.Status)

	return &Container{
		ID:      info.ID,
		Name:    strings.TrimPrefix(info.Name, "/"),
		Image:   info.Config.Image,
		Status:  status,
		Created: 0, // info.Created is string, needs parsing
		Labels:  info.Config.Labels,
	}, nil
}

// Close 关闭客户端
func (m *DockerManager) Close() error {
	return m.client.Close()
}

// parseStatus 解析容器状态
func parseStatus(status string) ContainerStatus {
	switch status {
	case "created":
		return StatusCreated
	case "running":
		return StatusRunning
	case "paused":
		return StatusPaused
	case "restarting":
		return StatusRestarting
	case "exited":
		return StatusExited
	case "removing":
		return StatusRemoving
	case "dead":
		return StatusDead
	default:
		return ContainerStatus(status)
	}
}

// Ping 测试 Docker 连接
func (m *DockerManager) Ping(ctx context.Context) error {
	_, err := m.client.Ping(ctx)
	return err
}

// ListContainers 列出所有 AgentBox 管理的容器
func (m *DockerManager) ListContainers(ctx context.Context) ([]*Container, error) {
	containers, err := m.client.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	result := make([]*Container, 0)
	for _, c := range containers {
		// 只返回带有 agentbox 标签的容器
		if _, ok := c.Labels["agentbox.managed"]; ok {
			result = append(result, &Container{
				ID:      c.ID,
				Name:    strings.TrimPrefix(c.Names[0], "/"),
				Image:   c.Image,
				Status:  parseStatus(c.State),
				Created: c.Created,
				Labels:  c.Labels,
			})
		}
	}

	return result, nil
}

// ListImages 列出所有镜像
func (m *DockerManager) ListImages(ctx context.Context) ([]*Image, error) {
	images, err := m.client.ImageList(ctx, image.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list images: %w", err)
	}

	// 获取所有容器以检查镜像是否在使用中
	containers, err := m.client.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	// 构建镜像使用映射
	imageInUse := make(map[string]bool)
	for _, c := range containers {
		imageInUse[c.ImageID] = true
	}

	// Agent 镜像前缀列表
	agentImagePrefixes := []string{
		"anthropic/claude-code",
		"ghcr.io/openai/codex",
		"agentbox/",
	}

	result := make([]*Image, 0, len(images))
	for _, img := range images {
		// 检查是否是 Agent 镜像
		isAgentImage := false
		for _, tag := range img.RepoTags {
			for _, prefix := range agentImagePrefixes {
				if strings.HasPrefix(tag, prefix) {
					isAgentImage = true
					break
				}
			}
			if isAgentImage {
				break
			}
		}

		result = append(result, &Image{
			ID:           img.ID,
			Tags:         img.RepoTags,
			Size:         img.Size,
			Created:      img.Created,
			InUse:        imageInUse[img.ID],
			IsAgentImage: isAgentImage,
		})
	}

	return result, nil
}

// PullImage 拉取镜像
func (m *DockerManager) PullImage(ctx context.Context, imageName string) error {
	reader, err := m.client.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	defer reader.Close()

	// 消费输出直到完成
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return fmt.Errorf("failed to complete image pull: %w", err)
	}

	return nil
}

// RemoveImage 删除镜像
func (m *DockerManager) RemoveImage(ctx context.Context, imageID string) error {
	_, err := m.client.ImageRemove(ctx, imageID, image.RemoveOptions{
		Force:         false,
		PruneChildren: true,
	})
	if err != nil {
		return fmt.Errorf("failed to remove image: %w", err)
	}
	return nil
}

// CopyToContainer 复制文件/目录到容器
// srcPath: 本地源路径
// dstPath: 容器内目标路径（必须是目录）
func (m *DockerManager) CopyToContainer(ctx context.Context, containerID string, srcPath string, dstPath string) error {
	// 创建 tar archive
	tarData, err := createTarFromPath(srcPath)
	if err != nil {
		return fmt.Errorf("failed to create tar archive: %w", err)
	}

	// 转换为 bytes.Buffer 以获取数据
	buf, ok := tarData.(*bytes.Buffer)
	if !ok {
		return fmt.Errorf("expected bytes.Buffer from createTarFromPath")
	}

	// 使用 Docker SDK 的 CopyToContainer
	reader := bytes.NewReader(buf.Bytes())
	err = m.client.CopyToContainer(ctx, containerID, dstPath, reader, container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
	if err != nil {
		return fmt.Errorf("failed to copy to container: %w", err)
	}

	return nil
}

// createTarFromPath 从本地路径创建 tar 归档
func createTarFromPath(srcPath string) (io.Reader, error) {
	srcPath = filepath.Clean(srcPath)
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat source path: %w", err)
	}

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	if srcInfo.IsDir() {
		// 遍历目录
		baseName := filepath.Base(srcPath)
		err = filepath.Walk(srcPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// 跳过 .git 目录
			if info.IsDir() && info.Name() == ".git" {
				return filepath.SkipDir
			}

			// 计算相对路径（相对于 srcPath 的父目录，保留目录名）
			relPath, err := filepath.Rel(filepath.Dir(srcPath), path)
			if err != nil {
				return err
			}

			// 处理符号链接
			if info.Mode()&os.ModeSymlink != 0 {
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				header := &tar.Header{
					Name:     relPath,
					Linkname: link,
					Mode:     int64(info.Mode()),
					Typeflag: tar.TypeSymlink,
				}
				return tw.WriteHeader(header)
			}

			// 创建 tar header
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = relPath

			if err := tw.WriteHeader(header); err != nil {
				return err
			}

			// 如果是普通文件，写入内容
			if info.Mode().IsRegular() {
				file, err := os.Open(path)
				if err != nil {
					return err
				}

				_, copyErr := io.Copy(tw, file)
				file.Close()
				if copyErr != nil {
					return copyErr
				}
			}

			return nil
		})
		if err != nil {
			tw.Close()
			return nil, fmt.Errorf("failed to walk directory: %w", err)
		}
		_ = baseName // 用于保留目录结构
	} else {
		// 单个文件
		header, err := tar.FileInfoHeader(srcInfo, "")
		if err != nil {
			tw.Close()
			return nil, err
		}
		header.Name = filepath.Base(srcPath)

		if err := tw.WriteHeader(header); err != nil {
			tw.Close()
			return nil, err
		}

		file, err := os.Open(srcPath)
		if err != nil {
			tw.Close()
			return nil, err
		}

		_, copyErr := io.Copy(tw, file)
		file.Close()
		if copyErr != nil {
			tw.Close()
			return nil, copyErr
		}
	}

	// 关闭 tar writer 以完成归档
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}

	return buf, nil
}
