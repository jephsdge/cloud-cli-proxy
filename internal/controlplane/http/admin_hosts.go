package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	nethttp "net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
	"github.com/zanel1u/cloud-cli-proxy/internal/controlplane/credgen"
	"github.com/zanel1u/cloud-cli-proxy/internal/network"
	"github.com/zanel1u/cloud-cli-proxy/internal/runtime"
	"github.com/zanel1u/cloud-cli-proxy/internal/store/repository"
)

func expandHostMountSources(mounts repository.HostMounts) repository.HostMounts {
	return mounts
}

type AdminHostStore interface {
	ListHostsWithUsername(context.Context) ([]repository.HostWithUsername, error)
	GetHostDetail(context.Context, string) (repository.HostDetail, error)
	GetHost(context.Context, string) (repository.Host, error)
	UpsertHost(context.Context, repository.UpsertHostParams) (repository.Host, error)
	GetUser(context.Context, string) (repository.User, error)
	BindEgressIPToHost(context.Context, string, string) (repository.HostBinding, error)
	DeleteHost(context.Context, string) error
	ListHostsByUserID(context.Context, string) ([]repository.Host, error)
	ListRunningHosts(ctx context.Context) ([]repository.Host, error)
	GetHostWithClaudeAccount(ctx context.Context, hostID string) (repository.HostWithClaudeAccount, error) // Phase 33 D-22
	UpdateHostMounts(ctx context.Context, hostID string, mounts repository.HostMounts) error
	UpdateHostPorts(ctx context.Context, hostID string, ports repository.HostPorts) error
	UpdateHostTimezone(ctx context.Context, hostID, timezone string) error
	UpdateHostIdentity(ctx context.Context, hostID string, identity repository.WorkerIdentity) error
	UpdateHostGatewayConfig(ctx context.Context, hostID, mode string, config json.RawMessage) error
}

type AdminHostsHandler struct {
	logger        *slog.Logger
	store         AdminHostStore
	queue         HostActionQueuer
	events        EventRecorder
	imageLockPath string
}

func NewAdminHostsHandler(logger *slog.Logger, store AdminHostStore, queue HostActionQueuer, events EventRecorder, imageLockPath string) *AdminHostsHandler {
	return &AdminHostsHandler{logger: logger, store: store, queue: queue, events: events, imageLockPath: imageLockPath}
}

func (h *AdminHostsHandler) List() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hosts, err := h.store.ListHostsWithUsername(r.Context())
		if err != nil {
			h.logger.Error("list hosts failed", "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "list hosts failed"})
			return
		}

		writeJSON(w, nethttp.StatusOK, map[string]any{"hosts": hosts})
	})
}

// getDockerStatuses runs `docker ps -a` once and returns a map of container
// name → status string (e.g. "running", "exited", "created").
func getDockerStatuses() map[string]string {
	cmd := exec.CommandContext(context.Background(), "docker", "ps", "-a",
		"--filter", "label=cloud-cli-proxy.managed=true",
		"--format", "{{.Names}}\t{{.State}}")
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("docker ps failed", "error", err)
		return nil
	}
	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

type adminHostDetailResponse struct {
	repository.HostDetail
	ConnectionInfo       *repository.ConnectionInfo `json:"connection_info,omitempty"`
	PersistentVolumeName string                     `json:"persistent_volume_name,omitempty"` // Phase 33 D-22
}

func (h *AdminHostsHandler) Get() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		detail, err := h.store.GetHostDetail(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host detail failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host detail failed"})
			return
		}

		resp := adminHostDetailResponse{HostDetail: detail}
		// Phase 33 D-22：从 LEFT JOIN 取 persistent_volume_name，失败仅记日志不影响 detail 主路径。
		if hostWithCA, err := h.store.GetHostWithClaudeAccount(r.Context(), hostID); err == nil {
			resp.PersistentVolumeName = hostWithCA.PersistentVolumeName
		} else if !errors.Is(err, pgx.ErrNoRows) {
			h.logger.Warn("get host with claude_account failed (degraded)", "host_id", hostID, "error", err)
		}
		resp.User.PasswordHash = ""
		resp.User.EntryPassword = ""
		sshTarget := detail.User.Username
		if sshTarget != "" {
			scheme := "https"
			if r.TLS == nil {
				scheme = "http"
			}
			host := r.Host
			if idx := strings.Index(host, ":"); idx != -1 {
				host = host[:idx]
			}
			baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)
			vncPath := fmt.Sprintf("/v1/admin/hosts/%s/vnc/vnc.html", detail.Host.ID)
			resp.ConnectionInfo = &repository.ConnectionInfo{
				CurlCommand: fmt.Sprintf("curl -sSL %s/entry/%s | bash", baseURL, sshTarget),
				SSHCommand:  fmt.Sprintf("ssh %s@%s -p 2222", sshTarget, host),
				SSHPort:     2222,
				VNCURL:      fmt.Sprintf("%s%s", baseURL, vncPath),
			}
		}

		writeJSON(w, nethttp.StatusOK, resp)
	})
}

func (h *AdminHostsHandler) Create() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		var body struct {
			UserID         string                    `json:"user_id"`
			EgressIPID     string                    `json:"egress_ip_id"`
			Timezone       string                    `json:"timezone"`
			MemoryLimitMB  int                       `json:"memory_limit_mb"`
			CPULimit       float64                   `json:"cpu_limit"`
			DiskLimitGB    int                       `json:"disk_limit_gb"`
			HostMounts     repository.HostMounts     `json:"host_mounts"`
			HostPorts      repository.HostPorts      `json:"host_ports"`
			WorkerIdentity repository.WorkerIdentity `json:"worker_identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "user_id is required"})
			return
		}
		if body.EgressIPID == "" {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "egress_ip_id is required"})
			return
		}

		timezone := body.Timezone
		if timezone == "" {
			timezone = "America/New_York"
		}
		hostname := generateHostname()
		identity, identityErr := normalizeWorkerIdentity(body.WorkerIdentity, defaultWorkerIdentity(hostname, timezone, mustGenerateMachineID()))
		if identityErr != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": identityErr.Error()})
			return
		}
		hostname = identity.Hostname
		timezone = identity.Timezone
		hostShortID := credgen.GenerateShortID()

		imageLockPath := h.imageLockPath
		if imageLockPath == "" {
			imageLockPath = runtime.DefaultImageLockPath
		}
		runtimeSpec, specErr := runtime.LoadRuntimeSpec(imageLockPath)
		if specErr != nil {
			h.logger.Error("load image.lock failed", "error", specErr)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "image.lock load failed"})
			return
		}
		if runtimeSpec.ImageName == "" {
			h.logger.Error("image.lock missing image_name")
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "image.lock missing image_name"})
			return
		}

		if _, err := h.store.GetUser(r.Context(), body.UserID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "user not found"})
				return
			}
			h.logger.Error("get user failed", "user_id", body.UserID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get user failed"})
			return
		}

		// 一用户一主机硬约束（与 0018 迁移的 partial unique index 对齐）：
		// 同一 user 已存在 status 不在 deleted/archived 的主机时拒绝创建，避免迁移期 race。
		existing, err := h.store.ListHostsByUserID(r.Context(), body.UserID)
		if err != nil {
			h.logger.Error("list hosts for active host check failed", "user_id", body.UserID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "check existing hosts failed"})
			return
		}
		for i := range existing {
			if existing[i].Status != "deleted" && existing[i].Status != "archived" {
				writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "user already has an active host"})
				return
			}
		}

		const maxRetries = 5
		var host repository.Host
		for attempt := 0; attempt < maxRetries; attempt++ {
			host, err = h.store.UpsertHost(r.Context(), repository.UpsertHostParams{
				UserID:           body.UserID,
				Status:           "pending",
				ShortID:          hostShortID,
				TemplateImageRef: runtimeSpec.ImageName,
				HomeVolumeName:   "",
				SlotKey:          "primary",
				Timezone:         timezone,
				Hostname:         hostname,
				MemoryLimitMB:    body.MemoryLimitMB,
				CPULimit:         body.CPULimit,
				DiskLimitGB:      body.DiskLimitGB,
				HostMounts:       expandHostMountSources(body.HostMounts),
				HostPorts:        body.HostPorts,
				WorkerIdentity:   identity,
			})
			if err == nil {
				break
			}
			if strings.Contains(err.Error(), "short_id") && (strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate")) {
				hostShortID = credgen.GenerateShortID()
				continue
			}
			break
		}
		if err != nil {
			h.logger.Error("create host failed", "user_id", body.UserID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "create host failed"})
			return
		}

		if _, err := h.store.BindEgressIPToHost(r.Context(), host.ID, body.EgressIPID); err != nil {
			h.logger.Error("bind egress IP failed", "host_id", host.ID, "egress_ip_id", body.EgressIPID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "bind egress IP failed"})
			return
		}

		task, err := h.queue.QueueHostAction(r.Context(), host.ID, agentapi.ActionCreateHost, "admin")
		if err != nil {
			h.logger.Error("queue create_host failed", "host_id", host.ID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "queue create action failed"})
			return
		}

		if h.events != nil {
			hostID := host.ID
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:  &hostID,
				Level:   "info",
				Type:    "admin.host.create",
				Message: "管理员创建主机",
				Metadata: map[string]any{
					"operator": "admin",
					"user_id":  body.UserID,
				},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.create", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusAccepted, map[string]any{
			"host":    host,
			"task_id": task.ID,
			"status":  "202 Accepted",
		})
	})
}

func (h *AdminHostsHandler) Start() nethttp.Handler {
	return h.lifecycleAction(agentapi.ActionStartHost)
}

func (h *AdminHostsHandler) Stop() nethttp.Handler {
	return h.lifecycleAction(agentapi.ActionStopHost)
}

func (h *AdminHostsHandler) Rebuild() nethttp.Handler {
	return h.lifecycleAction(agentapi.ActionRebuildHost)
}

func (h *AdminHostsHandler) lifecycleAction(action agentapi.HostAction) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		if action == agentapi.ActionRebuildHost {
			var body struct {
				Mode string `json:"mode"`
			}
			if r.Body != nil && r.ContentLength > 0 {
				_ = json.NewDecoder(r.Body).Decode(&body)
			}
		}

		task, err := h.queue.QueueHostAction(r.Context(), hostID, action, "admin")
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("queue host action failed", "host_id", hostID, "action", action, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "queue action failed"})
			return
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:   &hostID,
				Level:    "info",
				Type:     "admin.host.action",
				Message:  "管理员发起主机操作",
				Metadata: map[string]any{"operator": "admin", "action": string(action)},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.action", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusAccepted, map[string]any{
			"task_id": task.ID,
			"status":  "202 Accepted",
		})
	})
}

func (h *AdminHostsHandler) Delete() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		force := r.URL.Query().Get("force") == "true"

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}

		if !force && host.Status == "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "主机正在运行中，请先停止或使用强制删除"})
			return
		}

		containerName := "cloudproxy-" + hostID
		gwName := "cloudproxy-gw-" + hostID
		netName := "cloudproxy-net-" + hostID

		_ = dockerRm(containerName)
		_ = dockerRm(gwName)
		_ = dockerNetworkRm(netName)

		if err := h.store.DeleteHost(r.Context(), hostID); err != nil {
			h.logger.Error("delete host failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "delete host failed"})
			return
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				Level:   "warn",
				Type:    "admin.host.delete",
				Message: "管理员删除主机",
				Metadata: map[string]any{
					"operator": "admin",
					"host_id":  hostID,
					"force":    force,
				},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.delete", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "deleted"})
	})
}

func (h *AdminHostsHandler) RestartVNC() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for vnc restart failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		if err := restartContainerVNC(containerName); err != nil {
			h.logger.Error("restart vnc failed", "host_id", hostID, "container", containerName, "error", err)
			writeJSON(w, nethttp.StatusBadGateway, map[string]string{"error": "restart vnc failed"})
			return
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:   &hostID,
				Level:    "info",
				Type:     "admin.host.vnc_restarted",
				Message:  "管理员重启 VNC 服务",
				Metadata: map[string]any{"operator": "admin"},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.vnc_restarted", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, map[string]any{"status": "restarted"})
	})
}

func (h *AdminHostsHandler) ChangeRootPassword() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "password is required"})
			return
		}

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for root password change failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		if err := syncContainerPassword(containerName, "root", body.Password); err != nil {
			h.logger.Error("change root password failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusBadGateway, map[string]string{"error": "change root password failed"})
			return
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:   &hostID,
				Level:    "info",
				Type:     "admin.host.root_password_changed",
				Message:  "管理员修改容器 root 密码",
				Metadata: map[string]any{"operator": "admin"},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.root_password_changed", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	})
}

func (h *AdminHostsHandler) GetImageInfo() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		containerName := "cloudproxy-" + hostID

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		containerCmd := exec.CommandContext(ctx, "docker", "inspect",
			"--format", "{{.Image}}|{{.Config.Image}}|{{.Created}}", containerName)
		containerOut, err := containerCmd.Output()
		if err != nil {
			writeJSON(w, nethttp.StatusOK, map[string]any{
				"container_image_id":  "",
				"latest_image_id":     "",
				"update_available":    false,
				"container_available": false,
			})
			return
		}

		parts := strings.SplitN(strings.TrimSpace(string(containerOut)), "|", 3)
		containerImageID := parts[0]
		containerCreated := ""
		if len(parts) > 2 {
			containerCreated = parts[2]
		}

		spec, specErr := runtime.LoadRuntimeSpec(runtime.DefaultImageLockPath)
		if specErr != nil {
			writeJSON(w, nethttp.StatusOK, map[string]any{
				"container_image_id":  shortImageID(containerImageID),
				"container_created":   containerCreated,
				"latest_image_id":     "",
				"update_available":    false,
				"container_available": true,
			})
			return
		}

		latestCmd := exec.CommandContext(ctx, "docker", "inspect",
			"--format", "{{.Id}}|{{.Created}}", spec.ImageName)
		latestOut, err := latestCmd.Output()
		if err != nil {
			writeJSON(w, nethttp.StatusOK, map[string]any{
				"container_image_id":  shortImageID(containerImageID),
				"container_created":   containerCreated,
				"latest_image_id":     "",
				"latest_image_name":   spec.ImageName,
				"update_available":    false,
				"container_available": true,
			})
			return
		}

		latestParts := strings.SplitN(strings.TrimSpace(string(latestOut)), "|", 2)
		latestImageID := latestParts[0]
		latestCreated := ""
		if len(latestParts) > 1 {
			latestCreated = latestParts[1]
		}

		updateAvailable := containerImageID != latestImageID

		writeJSON(w, nethttp.StatusOK, map[string]any{
			"container_image_id":  shortImageID(containerImageID),
			"container_created":   containerCreated,
			"latest_image_id":     shortImageID(latestImageID),
			"latest_image_name":   spec.ImageName,
			"latest_created":      latestCreated,
			"update_available":    updateAvailable,
			"container_available": true,
		})
	})
}

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (h *AdminHostsHandler) ExportConfig() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for config export failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName,
			"tar", "czf", "-",
			"-C", "/workspace", ".claude", ".claude.json", ".chrome-data",
			"-C", "/var/lib/claude-persist", ".", ".cache")

		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"host-%s-config.tar.gz\"", hostID))
		w.WriteHeader(nethttp.StatusOK)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			h.logger.Error("docker exec stdout pipe failed", "host_id", hostID, "error", err)
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			h.logger.Error("docker exec stderr pipe failed", "host_id", hostID, "error", err)
			return
		}

		if err := cmd.Start(); err != nil {
			h.logger.Error("docker exec tar start failed", "host_id", hostID, "error", err)
			return
		}

		go func() {
			stderrBytes, _ := io.ReadAll(stderr)
			if len(stderrBytes) > 0 {
				h.logger.Warn("docker exec tar stderr", "host_id", hostID, "stderr", string(stderrBytes))
			}
		}()

		if _, err := io.Copy(w, stdout); err != nil {
			h.logger.Error("copy tar output failed", "host_id", hostID, "error", err)
			return
		}

		if err := cmd.Wait(); err != nil {
			h.logger.Error("docker exec tar failed", "host_id", hostID, "error", err)
			return
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:   &hostID,
				Level:    "info",
				Type:     "admin.host.config_exported",
				Message:  "管理员导出容器配置",
				Metadata: map[string]any{"operator": "admin"},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.config_exported", "error", err)
			}
		}
	})
}

func (h *AdminHostsHandler) ImportConfig() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for config import failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		if err := r.ParseMultipartForm(100 << 20); err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "invalid multipart form: " + err.Error()})
			return
		}
		defer r.MultipartForm.RemoveAll()

		file, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "file is required"})
			return
		}
		defer file.Close()

		containerName := "cloudproxy-" + hostID
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "tar", "xzf", "-", "-C", "/")
		cmd.Stdin = file

		output, err := cmd.CombinedOutput()
		if err != nil {
			h.logger.Error("docker exec tar extract failed", "host_id", hostID, "error", err, "output", string(output))
			writeJSON(w, nethttp.StatusBadGateway, map[string]string{"error": "import failed: " + strings.TrimSpace(string(output))})
			return
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:   &hostID,
				Level:    "info",
				Type:     "admin.host.config_imported",
				Message:  "管理员导入容器配置",
				Metadata: map[string]any{"operator": "admin"},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.config_imported", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	})
}

func (h *AdminHostsHandler) GetClaudeSettings() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for claude settings failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName,
			"cat", "/workspace/.claude/settings.json")
		output, err := cmd.CombinedOutput()
		if err != nil {
			writeJSON(w, nethttp.StatusOK, map[string]any{"settings": map[string]any{}})
			return
		}

		var settings json.RawMessage
		if err := json.Unmarshal(bytes.TrimSpace(output), &settings); err != nil {
			writeJSON(w, nethttp.StatusOK, map[string]any{"settings": map[string]any{}})
			return
		}

		writeJSON(w, nethttp.StatusOK, map[string]any{"settings": settings})
	})
}

func (h *AdminHostsHandler) UpdateClaudeSettings() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		var body struct {
			Settings json.RawMessage `json:"settings"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Settings) == 0 {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "settings is required"})
			return
		}

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for claude settings update failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		mkdirCmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName,
			"mkdir", "-p", "/workspace/.claude")
		if out, err := mkdirCmd.CombinedOutput(); err != nil {
			h.logger.Error("mkdir .claude failed", "host_id", hostID, "error", err, "output", string(out))
			writeJSON(w, nethttp.StatusBadGateway, map[string]string{"error": "prepare directory failed"})
			return
		}

		prettySettings, _ := json.MarshalIndent(json.RawMessage(body.Settings), "", "  ")
		teeCmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName,
			"tee", "/workspace/.claude/settings.json")
		teeCmd.Stdin = bytes.NewReader(prettySettings)
		if out, err := teeCmd.CombinedOutput(); err != nil {
			h.logger.Error("write claude settings failed", "host_id", hostID, "error", err, "output", string(out))
			writeJSON(w, nethttp.StatusBadGateway, map[string]string{"error": "write settings failed"})
			return
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:   &hostID,
				Level:    "info",
				Type:     "admin.host.claude_settings_updated",
				Message:  "管理员更新容器 Claude 配置",
				Metadata: map[string]any{"operator": "admin"},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.claude_settings_updated", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	})
}

func (h *AdminHostsHandler) GetClaudeInfo() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for claude info failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		script := `echo '===CLAUDE_JSON===' && cat /workspace/.claude.json 2>/dev/null || echo '{}' && echo '===PROJECT_SETTINGS===' && cat /workspace/.claude/settings.json 2>/dev/null || echo '{}' && echo '===UNAME===' && uname -a 2>/dev/null || echo 'unknown' && echo '===HOSTNAME===' && hostname 2>/dev/null || echo 'unknown' && echo '===NODE===' && node --version 2>/dev/null || echo 'unknown'`
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "bash", "-c", script)
		output, err := cmd.CombinedOutput()
		if err != nil {
			writeJSON(w, nethttp.StatusOK, map[string]any{
				"claude_json": map[string]any{},
				"uname":       "unknown",
				"hostname":    "unknown",
				"node":        "unknown",
			})
			return
		}

		raw := string(output)
		result := map[string]any{
			"uname":    "unknown",
			"hostname": "unknown",
			"node":     "unknown",
		}

		extractSection := func(marker, next string) string {
			start := strings.Index(raw, marker)
			if start == -1 {
				return ""
			}
			start += len(marker) + 1
			end := len(raw)
			if next != "" {
				if idx := strings.Index(raw[start:], next); idx >= 0 {
					end = start + idx
				}
			}
			return strings.TrimSpace(raw[start:end])
		}

		claudeJSON := extractSection("===CLAUDE_JSON===", "===PROJECT_SETTINGS===")
		var cj json.RawMessage
		if err := json.Unmarshal([]byte(claudeJSON), &cj); err == nil {
			result["claude_json"] = cj
		} else {
			result["claude_json"] = map[string]any{}
		}

		projectSettings := extractSection("===PROJECT_SETTINGS===", "===UNAME===")
		var ps json.RawMessage
		if err := json.Unmarshal([]byte(projectSettings), &ps); err == nil {
			result["project_settings"] = ps
		} else {
			result["project_settings"] = map[string]any{}
		}

		result["uname"] = extractSection("===UNAME===", "===HOSTNAME===")
		result["hostname"] = extractSection("===HOSTNAME===", "===NODE===")
		result["node"] = extractSection("===NODE===", "")

		writeJSON(w, nethttp.StatusOK, result)
	})
}

type claudeProcess struct {
	PID            int    `json:"pid"`
	WorkDir        string `json:"work_dir"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
}

func (h *AdminHostsHandler) GetClaudeStatus() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for claude status failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		script := `ps -eo pid=,etimes=,args= 2>/dev/null | grep '[c]laude ' | while read -r pid etime rest; do
  cwd=$(readlink /proc/$pid/cwd 2>/dev/null || echo "unknown")
  printf '%s|%s|%s\n' "$pid" "$etime" "$cwd"
done`
		procCmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "bash", "-c", script)
		procOut, _ := procCmd.CombinedOutput()

		var processes []claudeProcess
		for _, line := range strings.Split(strings.TrimSpace(string(procOut)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "|", 3)
			if len(parts) < 3 {
				continue
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			elapsed, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			cwd := strings.TrimSpace(parts[2])
			if pid > 0 {
				processes = append(processes, claudeProcess{
					PID:            pid,
					WorkDir:        cwd,
					ElapsedSeconds: elapsed,
				})
			}
		}

		versionCmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName,
			"bash", "-c", "claude --version 2>/dev/null || echo unknown")
		versionOut, _ := versionCmd.CombinedOutput()
		version := strings.TrimSpace(string(versionOut))
		if version == "" {
			version = "unknown"
		}

		writeJSON(w, nethttp.StatusOK, map[string]any{
			"running_instances": len(processes),
			"version":           version,
			"processes":         processes,
		})
	})
}

func (h *AdminHostsHandler) UpdateClaude() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		host, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for claude update failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}
		if host.Status != "running" {
			writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "host is not running"})
			return
		}

		containerName := "cloudproxy-" + hostID
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()

		updateScript := `npm install -g @anthropic-ai/claude-code@latest 2>&1`
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "bash", "-c", updateScript)
		output, err := cmd.CombinedOutput()
		if err != nil {
			h.logger.Error("update claude failed", "host_id", hostID, "error", err, "output", string(output))
			writeJSON(w, nethttp.StatusBadGateway, map[string]string{"error": "update claude failed: " + strings.TrimSpace(string(output))})
			return
		}

		versionCmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName,
			"bash", "-c", "claude --version 2>/dev/null || echo unknown")
		versionOut, _ := versionCmd.CombinedOutput()
		version := strings.TrimSpace(string(versionOut))
		if version == "" {
			version = "unknown"
		}

		if h.events != nil {
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID:   &hostID,
				Level:    "info",
				Type:     "admin.host.claude_updated",
				Message:  "管理员更新容器 Claude Code",
				Metadata: map[string]any{"operator": "admin", "version": version},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.claude_updated", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, map[string]any{"status": "ok", "version": version})
	})
}

func (h *AdminHostsHandler) UpdateMounts() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		var body struct {
			Mounts repository.HostMounts `json:"mounts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		for _, m := range body.Mounts {
			if m.Source == "" || m.Target == "" {
				writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "source and target are required"})
				return
			}
			if !strings.HasPrefix(m.Source, "/") || !strings.HasPrefix(m.Target, "/") {
				writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "paths must be absolute"})
				return
			}
		}
		expandedMounts := expandHostMountSources(body.Mounts)
		if err := h.store.UpdateHostMounts(r.Context(), hostID, expandedMounts); err != nil {
			h.logger.Error("update host mounts failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "update mounts failed"})
			return
		}
		if h.events != nil {
			hid := hostID
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID: &hid, Level: "info", Type: "admin.host.update_mounts",
				Message:  "管理员更新主机挂载配置",
				Metadata: map[string]any{"operator": "admin", "mount_count": len(body.Mounts)},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.update_mounts", "error", err)
			}
		}
		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	})
}

func (h *AdminHostsHandler) UpdatePorts() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		var body struct {
			Ports repository.HostPorts `json:"ports"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		for _, p := range body.Ports {
			if p.HostPort <= 0 || p.HostPort > 65535 {
				writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid host port: %d", p.HostPort)})
				return
			}
			if p.ContainerPort <= 0 || p.ContainerPort > 65535 {
				writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid container port: %d", p.ContainerPort)})
				return
			}
			if p.Protocol != "" && p.Protocol != "tcp" && p.Protocol != "udp" {
				writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid protocol: %s (must be tcp or udp)", p.Protocol)})
				return
			}
		}
		if err := h.store.UpdateHostPorts(r.Context(), hostID, body.Ports); err != nil {
			h.logger.Error("update host ports failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "update ports failed"})
			return
		}
		if h.events != nil {
			hid := hostID
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID: &hid, Level: "info", Type: "admin.host.update_ports",
				Message:  "管理员更新主机端口映射配置",
				Metadata: map[string]any{"operator": "admin", "port_count": len(body.Ports)},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.update_ports", "error", err)
			}
		}
		writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	})
}

type updateHostTimezoneResponse struct {
	HostID           string `json:"host_id"`
	Timezone         string `json:"timezone"`
	RequiresRebuild  bool   `json:"requires_rebuild"`
	PreviousTimezone string `json:"previous_timezone,omitempty"`
}

type hostIdentityResponse struct {
	HostID          string                    `json:"host_id"`
	Identity        repository.WorkerIdentity `json:"identity"`
	RequiresRebuild bool                      `json:"requires_rebuild"`
}

func (h *AdminHostsHandler) UpdateTimezone() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		var body struct {
			Timezone string `json:"timezone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}

		timezone, err := normalizeHostTimezone(body.Timezone)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		detail, err := h.store.GetHostDetail(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for timezone update failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}

		if err := h.store.UpdateHostTimezone(r.Context(), hostID, timezone); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("update host timezone failed", "host_id", hostID, "timezone", timezone, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "update timezone failed"})
			return
		}

		requiresRebuild := hostTimezoneRequiresRebuild(detail.Host.Status)
		if h.events != nil {
			hid := hostID
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID: &hid, Level: "info", Type: "admin.host.update_timezone",
				Message: "管理员更新主机时区",
				Metadata: map[string]any{
					"operator":          "admin",
					"previous_timezone": detail.Host.Timezone,
					"timezone":          timezone,
					"requires_rebuild":  requiresRebuild,
				},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.update_timezone", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, updateHostTimezoneResponse{
			HostID:           hostID,
			Timezone:         timezone,
			RequiresRebuild:  requiresRebuild,
			PreviousTimezone: detail.Host.Timezone,
		})
	})
}

func (h *AdminHostsHandler) GetIdentity() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		detail, err := h.store.GetHostDetail(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host identity failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host identity failed"})
			return
		}

		identity := workerIdentityWithDefaults(detail.Host.WorkerIdentity, defaultWorkerIdentity(detail.Host.Hostname, detail.Host.Timezone, ""))
		writeJSON(w, nethttp.StatusOK, hostIdentityResponse{
			HostID:          hostID,
			Identity:        identity,
			RequiresRebuild: hostTimezoneRequiresRebuild(detail.Host.Status),
		})
	})
}

func (h *AdminHostsHandler) UpdateIdentity() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		var body struct {
			Identity       repository.WorkerIdentity `json:"identity"`
			WorkerIdentity repository.WorkerIdentity `json:"worker_identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}

		input := body.Identity
		if workerIdentityIsZero(input) {
			input = body.WorkerIdentity
		}

		detail, err := h.store.GetHostDetail(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for identity update failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}

		fallback := workerIdentityWithDefaults(detail.Host.WorkerIdentity, defaultWorkerIdentity(detail.Host.Hostname, detail.Host.Timezone, ""))
		identity, err := normalizeWorkerIdentity(input, fallback)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		if err := h.store.UpdateHostIdentity(r.Context(), hostID, identity); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("update host identity failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "update host identity failed"})
			return
		}

		requiresRebuild := hostTimezoneRequiresRebuild(detail.Host.Status)
		if h.events != nil {
			hid := hostID
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID: &hid, Level: "info", Type: "admin.host.update_identity",
				Message: "管理员更新主机系统指纹",
				Metadata: map[string]any{
					"operator":         "admin",
					"hostname":         identity.Hostname,
					"timezone":         identity.Timezone,
					"vnc_resolution":   identity.VNCResolution,
					"browser_language": identity.BrowserLanguage,
					"requires_rebuild": requiresRebuild,
				},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.update_identity", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, hostIdentityResponse{
			HostID:          hostID,
			Identity:        identity,
			RequiresRebuild: requiresRebuild,
		})
	})
}

type hostGatewayConfigResponse struct {
	HostID          string          `json:"host_id"`
	Mode            string          `json:"mode"`
	GatewayConfig   json.RawMessage `json:"gateway_config"`
	EffectiveConfig json.RawMessage `json:"effective_config"`
	Source          string          `json:"source"`
	Applied         bool            `json:"applied"`
}

func (h *AdminHostsHandler) GetGatewayConfig() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		detail, err := h.store.GetHostDetail(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host gateway config failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get gateway config failed"})
			return
		}

		mode := gatewayConfigModeOrDefault(detail.Host.GatewayConfigMode)
		effective, err := buildEffectiveGatewayConfig(detail, mode, detail.Host.GatewayConfig)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, nethttp.StatusOK, hostGatewayConfigResponse{
			HostID:          hostID,
			Mode:            mode,
			GatewayConfig:   nullableRaw(detail.Host.GatewayConfig),
			EffectiveConfig: effective,
			Source:          gatewayConfigSource(mode),
		})
	})
}

func (h *AdminHostsHandler) UpdateGatewayConfig() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")
		var body struct {
			Mode          string          `json:"mode"`
			GatewayConfig json.RawMessage `json:"gateway_config"`
			Config        json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}

		raw := body.GatewayConfig
		if len(raw) == 0 {
			raw = body.Config
		}

		mode, err := normalizeGatewayConfigMode(body.Mode)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if body.Mode == "" {
			if gatewayConfigRawEmpty(raw) {
				mode = repository.GatewayConfigModeAuto
			} else {
				mode = repository.GatewayConfigModeCustom
			}
		}
		raw = bytes.TrimSpace(raw)
		if mode == repository.GatewayConfigModeAuto {
			raw = nil
		} else if gatewayConfigRawEmpty(raw) {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "custom gateway_config is required"})
			return
		}
		if !gatewayConfigRawEmpty(raw) && !json.Valid(raw) {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "gateway_config must be valid JSON"})
			return
		}

		detail, err := h.store.GetHostDetail(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for gateway config failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}

		effective, err := buildEffectiveGatewayConfig(detail, mode, raw)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		applied := false
		if detail.Host.Status == "running" {
			if err := applyRunningGatewayConfig(r.Context(), hostID, effective); err != nil {
				h.logger.Error("apply running gateway config failed", "host_id", hostID, "error", err)
				writeJSON(w, nethttp.StatusBadGateway, map[string]string{"error": "apply gateway config failed: " + err.Error()})
				return
			}
			applied = true
		}

		if err := h.store.UpdateHostGatewayConfig(r.Context(), hostID, mode, raw); err != nil {
			if applied {
				previousMode := gatewayConfigModeOrDefault(detail.Host.GatewayConfigMode)
				if previous, buildErr := buildEffectiveGatewayConfig(detail, previousMode, detail.Host.GatewayConfig); buildErr == nil {
					if rollbackErr := applyRunningGatewayConfig(context.Background(), hostID, previous); rollbackErr != nil {
						h.logger.Error("rollback running gateway config after db failure failed", "host_id", hostID, "error", rollbackErr)
					}
				}
			}
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("update host gateway config failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "update gateway config failed"})
			return
		}

		if h.events != nil {
			hid := hostID
			if _, err := h.events.RecordEvent(r.Context(), repository.RecordEventParams{
				HostID: &hid, Level: "info", Type: "admin.host.gateway_config_updated",
				Message:  "管理员更新主机 gateway sing-box 配置",
				Metadata: map[string]any{"operator": "admin", "mode": mode, "source": gatewayConfigSource(mode), "applied": applied},
			}); err != nil {
				h.logger.Error("record event failed", "type", "admin.host.gateway_config_updated", "error", err)
			}
		}

		writeJSON(w, nethttp.StatusOK, hostGatewayConfigResponse{
			HostID:          hostID,
			Mode:            mode,
			GatewayConfig:   nullableRaw(raw),
			EffectiveConfig: effective,
			Source:          gatewayConfigSource(mode),
			Applied:         applied,
		})
	})
}

func buildEffectiveGatewayConfig(detail repository.HostDetail, mode string, gatewayConfig json.RawMessage) (json.RawMessage, error) {
	mode = gatewayConfigModeOrDefault(mode)
	if mode == repository.GatewayConfigModeCustom {
		if gatewayConfigRawEmpty(gatewayConfig) {
			return nil, fmt.Errorf("custom gateway_config is required")
		}
		cfg, err := network.BuildGatewayCustomSingBoxConfig(gatewayConfig)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(cfg), nil
	}

	if len(detail.Bindings) == 0 {
		return nil, fmt.Errorf("host has no egress binding")
	}
	proxyRaw := detail.Bindings[0].EgressIP.ProxyConfig
	if len(proxyRaw) == 0 {
		return nil, fmt.Errorf("bound egress IP has empty proxy_config")
	}

	dnsServer := gatewayDNSServer(proxyRaw)
	serverIP, err := network.ResolveGatewayProxyServerIP(proxyRaw, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve gateway proxy server: %w", err)
	}
	cfg, err := network.BuildGatewaySingBoxConfig(proxyRaw, nil, dnsServer, serverIP)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(cfg), nil
}

func gatewayDNSServer(raw json.RawMessage) string {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	dnsServer, _ := parsed["dns_server"].(string)
	return dnsServer
}

func gatewayConfigSource(mode string) string {
	if gatewayConfigModeOrDefault(mode) == repository.GatewayConfigModeAuto {
		return "egress_binding"
	}
	return "host_custom_config"
}

func normalizeGatewayConfigMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "", repository.GatewayConfigModeAuto:
		return repository.GatewayConfigModeAuto, nil
	case repository.GatewayConfigModeCustom:
		return repository.GatewayConfigModeCustom, nil
	default:
		return "", fmt.Errorf("invalid gateway config mode %q", mode)
	}
}

func gatewayConfigModeOrDefault(mode string) string {
	normalized, err := normalizeGatewayConfigMode(mode)
	if err != nil {
		return repository.GatewayConfigModeAuto
	}
	return normalized
}

func gatewayConfigRawEmpty(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func nullableRaw(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func normalizeHostTimezone(value string) (string, error) {
	timezone := strings.TrimSpace(value)
	if timezone == "" {
		return "", fmt.Errorf("timezone is required")
	}
	if len(timezone) > 128 {
		return "", fmt.Errorf("timezone is too long")
	}
	for _, r := range timezone {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '/', '_', '-', '+':
			continue
		default:
			return "", fmt.Errorf("timezone contains invalid character %q", r)
		}
	}
	if strings.HasPrefix(timezone, "/") || strings.Contains(timezone, "..") || strings.Contains(timezone, "//") {
		return "", fmt.Errorf("timezone is invalid")
	}
	return timezone, nil
}

func hostTimezoneRequiresRebuild(status string) bool {
	switch status {
	case "running", "stopped", "failed":
		return true
	default:
		return false
	}
}

func mustGenerateMachineID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000000000000000000000000000"
	}
	return fmt.Sprintf("%x", buf)
}

func defaultWorkerIdentity(hostname, timezone, machineID string) repository.WorkerIdentity {
	return repository.WorkerIdentity{
		Hostname:  strings.TrimSpace(hostname),
		Timezone:  strings.TrimSpace(timezone),
		MachineID: strings.ToLower(strings.TrimSpace(machineID)),
		Locale: repository.WorkerIdentityLocale{
			Lang:     "en_US.UTF-8",
			Language: "en_US:en",
			LCAll:    "en_US.UTF-8",
		},
		VNCResolution:     "1920x1080",
		BrowserLanguage:   "en-US",
		BrowserWindowSize: "1920x1080",
	}
}

func workerIdentityWithDefaults(input, fallback repository.WorkerIdentity) repository.WorkerIdentity {
	out := fallback
	if input.Hostname != "" {
		out.Hostname = input.Hostname
	}
	if input.Timezone != "" {
		out.Timezone = input.Timezone
	}
	if input.MachineID != "" {
		out.MachineID = input.MachineID
	}
	if input.Locale.Lang != "" {
		out.Locale.Lang = input.Locale.Lang
	}
	if input.Locale.Language != "" {
		out.Locale.Language = input.Locale.Language
	}
	if input.Locale.LCAll != "" {
		out.Locale.LCAll = input.Locale.LCAll
	}
	if input.VNCResolution != "" {
		out.VNCResolution = input.VNCResolution
	}
	if input.BrowserLanguage != "" {
		out.BrowserLanguage = input.BrowserLanguage
	}
	if input.BrowserWindowSize != "" {
		out.BrowserWindowSize = input.BrowserWindowSize
	}
	return out
}

func workerIdentityIsZero(identity repository.WorkerIdentity) bool {
	return strings.TrimSpace(identity.Hostname) == "" &&
		strings.TrimSpace(identity.Timezone) == "" &&
		strings.TrimSpace(identity.MachineID) == "" &&
		strings.TrimSpace(identity.Locale.Lang) == "" &&
		strings.TrimSpace(identity.Locale.Language) == "" &&
		strings.TrimSpace(identity.Locale.LCAll) == "" &&
		strings.TrimSpace(identity.VNCResolution) == "" &&
		strings.TrimSpace(identity.BrowserLanguage) == "" &&
		strings.TrimSpace(identity.BrowserWindowSize) == ""
}

func normalizeWorkerIdentity(input, fallback repository.WorkerIdentity) (repository.WorkerIdentity, error) {
	identity := workerIdentityWithDefaults(input, fallback)

	hostname, err := normalizeWorkerHostname(identity.Hostname)
	if err != nil {
		return repository.WorkerIdentity{}, err
	}
	timezone, err := normalizeHostTimezone(identity.Timezone)
	if err != nil {
		return repository.WorkerIdentity{}, err
	}
	machineID, err := normalizeWorkerMachineID(identity.MachineID)
	if err != nil {
		return repository.WorkerIdentity{}, err
	}
	locale, err := normalizeWorkerLocale(identity.Locale)
	if err != nil {
		return repository.WorkerIdentity{}, err
	}
	vncResolution, err := normalizeResolution(identity.VNCResolution, "vnc_resolution")
	if err != nil {
		return repository.WorkerIdentity{}, err
	}
	browserWindowSize, err := normalizeResolution(identity.BrowserWindowSize, "browser_window_size")
	if err != nil {
		return repository.WorkerIdentity{}, err
	}
	browserLanguage, err := normalizeWorkerToken(identity.BrowserLanguage, "browser_language", 128, "-_,.;=")
	if err != nil {
		return repository.WorkerIdentity{}, err
	}

	return repository.WorkerIdentity{
		Hostname:          hostname,
		Timezone:          timezone,
		MachineID:         machineID,
		Locale:            locale,
		VNCResolution:     vncResolution,
		BrowserLanguage:   browserLanguage,
		BrowserWindowSize: browserWindowSize,
	}, nil
}

func normalizeWorkerHostname(value string) (string, error) {
	hostname := strings.TrimSpace(value)
	if hostname == "" {
		return "", fmt.Errorf("hostname is required")
	}
	if len(hostname) > 63 {
		return "", fmt.Errorf("hostname is too long")
	}
	if hostname[0] == '-' || hostname[len(hostname)-1] == '-' {
		return "", fmt.Errorf("hostname must not start or end with '-'")
	}
	for _, r := range hostname {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return "", fmt.Errorf("hostname contains invalid character %q", r)
	}
	return hostname, nil
}

func normalizeWorkerMachineID(value string) (string, error) {
	machineID := strings.ToLower(strings.TrimSpace(value))
	if machineID == "" {
		machineID = mustGenerateMachineID()
	}
	if len(machineID) != 32 {
		return "", fmt.Errorf("machine_id must be 32 hex characters")
	}
	for _, r := range machineID {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return "", fmt.Errorf("machine_id must be 32 hex characters")
	}
	return machineID, nil
}

func normalizeWorkerLocale(locale repository.WorkerIdentityLocale) (repository.WorkerIdentityLocale, error) {
	lang, err := normalizeWorkerToken(locale.Lang, "LANG", 64, "_.@-")
	if err != nil {
		return repository.WorkerIdentityLocale{}, err
	}
	language, err := normalizeWorkerToken(locale.Language, "LANGUAGE", 128, "_.@:-")
	if err != nil {
		return repository.WorkerIdentityLocale{}, err
	}
	lcAll, err := normalizeWorkerToken(locale.LCAll, "LC_ALL", 64, "_.@-")
	if err != nil {
		return repository.WorkerIdentityLocale{}, err
	}
	return repository.WorkerIdentityLocale{
		Lang:     lang,
		Language: language,
		LCAll:    lcAll,
	}, nil
}

func normalizeWorkerToken(value, field string, maxLen int, extraAllowed string) (string, error) {
	token := strings.TrimSpace(value)
	if token == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if len(token) > maxLen {
		return "", fmt.Errorf("%s is too long", field)
	}
	for _, r := range token {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		if strings.ContainsRune(extraAllowed, r) {
			continue
		}
		return "", fmt.Errorf("%s contains invalid character %q", field, r)
	}
	return token, nil
}

func normalizeResolution(value, field string) (string, error) {
	resolution := strings.ToLower(strings.TrimSpace(value))
	widthRaw, heightRaw, ok := strings.Cut(resolution, "x")
	if !ok || widthRaw == "" || heightRaw == "" {
		return "", fmt.Errorf("%s must use WIDTHxHEIGHT format", field)
	}
	width, err := strconv.Atoi(widthRaw)
	if err != nil {
		return "", fmt.Errorf("%s width is invalid", field)
	}
	height, err := strconv.Atoi(heightRaw)
	if err != nil {
		return "", fmt.Errorf("%s height is invalid", field)
	}
	if width < 640 || width > 7680 || height < 480 || height > 4320 {
		return "", fmt.Errorf("%s is out of supported range", field)
	}
	return fmt.Sprintf("%dx%d", width, height), nil
}

var applyRunningGatewayConfig = func(ctx context.Context, hostID string, config []byte) error {
	configDir := network.GatewayConfigDir(hostID)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("mkdir gateway config dir: %w", err)
	}
	configPath := network.GatewayConfigPath(hostID)
	previous, previousErr := os.ReadFile(configPath)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		return fmt.Errorf("write gateway config: %w", err)
	}

	gwName := "cloudproxy-gw-" + hostID
	restartAndCheck := func() error {
		cmd := exec.CommandContext(ctx, "docker", "restart", gwName)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker restart %s: %s: %w", gwName, strings.TrimSpace(string(out)), err)
		}
		time.Sleep(500 * time.Millisecond)

		inspect := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", gwName)
		running, inspectErr := inspect.Output()
		if inspectErr == nil && strings.TrimSpace(string(running)) == "true" {
			return nil
		}
		logs, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "120", gwName).CombinedOutput()
		if inspectErr != nil {
			return fmt.Errorf("inspect restarted gateway: %w; logs: %s", inspectErr, strings.TrimSpace(string(logs)))
		}
		return fmt.Errorf("gateway is not running after restart; logs: %s", strings.TrimSpace(string(logs)))
	}

	if err := restartAndCheck(); err != nil {
		if previousErr != nil {
			return err
		}
		if rollbackWriteErr := os.WriteFile(configPath, previous, 0o644); rollbackWriteErr != nil {
			return fmt.Errorf("%w; rollback write failed: %v", err, rollbackWriteErr)
		}
		if rollbackErr := restartAndCheck(); rollbackErr != nil {
			return fmt.Errorf("%w; rollback restart failed: %v", err, rollbackErr)
		}
		return fmt.Errorf("%w; previous gateway config restored", err)
	}
	return nil
}

// syncContainerPassword updates the Linux user password inside a running container via docker exec.
// Exposed as a package-level var so unit tests can inject a fake implementation (Phase 29.1).
var syncContainerPassword = func(containerName, user, password string) error {
	cmd := exec.CommandContext(context.Background(), "docker", "exec", "-i", containerName,
		"chpasswd")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("%s:%s\n", user, password))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker exec chpasswd: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func generateHostname() string {
	const alphaNum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	prefixes := []string{"DESKTOP-", "LAPTOP-"}

	pidx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(prefixes))))
	prefix := prefixes[pidx.Int64()]

	suffix := make([]byte, 7)
	for i := range suffix {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphaNum))))
		suffix[i] = alphaNum[n.Int64()]
	}
	return prefix + string(suffix)
}

func dockerRm(containerName string) error {
	cmd := exec.CommandContext(context.Background(), "docker", "rm", "-f", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("docker rm failed (may not exist)", "container", containerName, "output", string(output), "error", err)
	}
	return err
}

func dockerNetworkRm(networkName string) error {
	cmd := exec.CommandContext(context.Background(), "docker", "network", "rm", networkName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("docker network rm failed (may not exist)", "network", networkName, "output", string(output), "error", err)
	}
	return err
}

func (h *AdminHostsHandler) GetLogs() nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		hostID := r.PathValue("hostID")

		_, err := h.store.GetHost(r.Context(), hostID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "host not found"})
				return
			}
			h.logger.Error("get host for logs failed", "host_id", hostID, "error", err)
			writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "get host failed"})
			return
		}

		tail := r.URL.Query().Get("tail")
		if tail == "" {
			tail = "100"
		}
		tailN, err := strconv.Atoi(tail)
		if err != nil || tailN < 1 {
			tailN = 100
		}
		if tailN > 500 {
			tailN = 500
		}

		target := strings.TrimSpace(r.URL.Query().Get("target"))
		containerName, target, ok := hostLogsContainerName(hostID, target)
		if !ok {
			writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "target must be worker or gateway"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", strconv.Itoa(tailN), containerName)
		output, err := cmd.CombinedOutput()

		result := map[string]any{
			"host_id":        hostID,
			"target":         target,
			"container_name": containerName,
			"tail":           tailN,
			"logs":           string(output),
		}
		if err != nil {
			result["error"] = err.Error()
			result["logs"] = string(output)
		}

		writeJSON(w, nethttp.StatusOK, result)
	})
}

func hostLogsContainerName(hostID, target string) (string, string, bool) {
	if target == "" {
		target = "worker"
	}
	switch target {
	case "worker":
		return "cloudproxy-" + hostID, target, true
	case "gateway":
		return "cloudproxy-gw-" + hostID, target, true
	default:
		return "", target, false
	}
}
