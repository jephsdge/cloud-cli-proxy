package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
	"github.com/zanel1u/cloud-cli-proxy/internal/broadcast"
	"github.com/zanel1u/cloud-cli-proxy/internal/store/repository"
)

type ReconcileStore interface {
	ListRunningHosts(context.Context) ([]repository.Host, error)
	ListFailedHosts(context.Context) ([]repository.Host, error)
	UpdateHostStatus(context.Context, string, string) error
	MarkStaleTasks(context.Context, time.Duration) ([]repository.Task, error)
	RecordEvent(context.Context, repository.RecordEventParams) (repository.Event, error)
}

type ContainerInspector interface {
	InspectContainer(context.Context, string) (agentapi.ContainerStatusResponse, error)
}

const (
	StartupNetworkRefreshAttempts = 3
	StartupNetworkRefreshDelay    = 30 * time.Second
)

type Reconciler struct {
	logger         *slog.Logger
	store          ReconcileStore
	inspector      ContainerInspector
	queuer         HostActionQueuer
	staleThreshold time.Duration
}

func NewReconciler(logger *slog.Logger, store ReconcileStore, inspector ContainerInspector, queuer HostActionQueuer, staleThreshold time.Duration) *Reconciler {
	if staleThreshold == 0 {
		staleThreshold = 10 * time.Minute
	}
	return &Reconciler{
		logger:         logger,
		store:          store,
		inspector:      inspector,
		queuer:         queuer,
		staleThreshold: staleThreshold,
	}
}

func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.reconcileHosts(ctx); err != nil {
		r.logger.Error("host reconciliation failed", "error", err)
	}

	if err := r.reconcileFailedHosts(ctx); err != nil {
		r.logger.Error("failed host recovery failed", "error", err)
	}

	if err := r.reconcileStaleTasks(ctx); err != nil {
		r.logger.Error("stale task reconciliation failed", "error", err)
	}

	return nil
}

func (r *Reconciler) reconcileHosts(ctx context.Context) error {
	hosts, err := r.store.ListRunningHosts(ctx)
	if err != nil {
		return fmt.Errorf("list running hosts: %w", err)
	}

	for _, host := range hosts {
		containerName := fmt.Sprintf("cloudproxy-%s", host.ID)

		status, err := r.inspector.InspectContainer(ctx, containerName)
		if err != nil {
			r.logger.Warn("inspect container communication failed, skipping host",
				"host_id", host.ID, "container", containerName, "error", err)
			continue
		}

		if status.Exists && status.Running {
			continue
		}

		actualStatus := "stopped"
		if !status.Exists {
			actualStatus = "not_found"
		}

		if r.queuer != nil {
			if _, err := r.queuer.QueueHostAction(ctx, host.ID, agentapi.ActionStartHost, "system"); err != nil {
				r.logger.Error("auto-recover host failed, falling back to drift",
					"host_id", host.ID, "error", err)
				// 自动恢复失败时回退到原有 drift 行为
				r.recordHostDrift(ctx, host.ID, actualStatus)
				continue
			}

			r.store.RecordEvent(ctx, repository.RecordEventParams{
				HostID:  &host.ID,
				Level:   "info",
				Type:    "reconcile.host.auto_recover",
				Message: "对账自动恢复主机",
				Metadata: map[string]any{
					"operator":               "system",
					"db_status":              "running",
					"previous_actual_status": actualStatus,
					"host_id":                host.ID,
				},
			})

			r.logger.Info("reconciled auto-recovered host",
				"host_id", host.ID, "db_status", "running", "previous_actual_status", actualStatus)
			continue
		}
		broadcast.Broadcast("hosts", "update", host.ID)

		// queuer == nil: 向后兼容，保持原有 drift 行为
		r.recordHostDrift(ctx, host.ID, actualStatus)
	}

	return nil
}

// RefreshRunningHostNetworks restores volatile host-side networking for running
// containers after the control-plane process restarts. It performs a single
// attempt and is kept for tests/manual callers; production startup uses the
// retrying wrapper below.
func (r *Reconciler) RefreshRunningHostNetworks(ctx context.Context) error {
	_, err := r.refreshRunningHostNetworks(ctx, nil, 1)
	return err
}

// RefreshRunningHostNetworksWithRetry runs a bounded startup repair. Only hosts
// that failed inspection or queueing are retried; healthy successes are not
// touched again, so this does not become periodic network churn.
func (r *Reconciler) RefreshRunningHostNetworksWithRetry(ctx context.Context, attempts int, delay time.Duration) error {
	if attempts <= 0 {
		attempts = 1
	}
	if delay < 0 {
		delay = 0
	}

	var retryOnly map[string]struct{}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		failed, err := r.refreshRunningHostNetworks(ctx, retryOnly, attempt)
		if err != nil {
			lastErr = err
		} else if len(failed) == 0 {
			return nil
		} else {
			lastErr = fmt.Errorf("startup network refresh failed for %d host(s)", len(failed))
			retryOnly = hostIDSet(failed)
		}

		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}

func (r *Reconciler) refreshRunningHostNetworks(ctx context.Context, retryOnly map[string]struct{}, attempt int) ([]string, error) {
	if r.queuer == nil {
		return nil, nil
	}
	hosts, err := r.store.ListRunningHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running hosts for network refresh: %w", err)
	}

	failed := make([]string, 0)
	for _, host := range hosts {
		if len(retryOnly) > 0 {
			if _, ok := retryOnly[host.ID]; !ok {
				continue
			}
		}
		containerName := fmt.Sprintf("cloudproxy-%s", host.ID)
		status, err := r.inspector.InspectContainer(ctx, containerName)
		if err != nil {
			r.logger.Warn("startup network refresh inspect failed, skipping host",
				"host_id", host.ID, "container", containerName, "attempt", attempt, "error", err)
			failed = append(failed, host.ID)
			continue
		}
		if !status.Exists || !status.Running {
			continue
		}
		if _, err := r.queuer.QueueHostAction(ctx, host.ID, agentapi.ActionPrepareHost, "system:startup"); err != nil {
			r.logger.Warn("startup network refresh queue failed", "host_id", host.ID, "attempt", attempt, "error", err)
			failed = append(failed, host.ID)
			continue
		}
		r.store.RecordEvent(ctx, repository.RecordEventParams{
			HostID:  &host.ID,
			Level:   "info",
			Type:    "reconcile.host.network_refresh",
			Message: "控制面启动恢复运行中主机网络",
			Metadata: map[string]any{
				"operator":  "system:startup",
				"db_status": "running",
				"host_id":   host.ID,
				"attempt":   attempt,
			},
		})
	}
	return failed, nil
}

func hostIDSet(hostIDs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(hostIDs))
	for _, id := range hostIDs {
		out[id] = struct{}{}
	}
	return out
}

func (r *Reconciler) recordHostDrift(ctx context.Context, hostID, actualStatus string) {
	if err := r.store.UpdateHostStatus(ctx, hostID, "stopped"); err != nil {
		r.logger.Error("update drifted host status failed", "host_id", hostID, "error", err)
		return
	}

	r.store.RecordEvent(ctx, repository.RecordEventParams{
		HostID:  &hostID,
		Level:   "warn",
		Type:    "reconcile.host.drift",
		Message: "对账发现主机状态漂移",
		Metadata: map[string]any{
			"operator":      "system",
			"db_status":     "running",
			"actual_status": actualStatus,
			"host_id":       hostID,
		},
	})

	r.logger.Info("reconciled drifted host",
		"host_id", hostID, "db_status", "running", "actual_status", actualStatus)
}

// reconcileFailedHosts 尝试自动恢复 status='failed' 的主机。
// 适用于系统重启后或之前任务失败的主机，由 reconciler 周期性重试。
func (r *Reconciler) reconcileFailedHosts(ctx context.Context) error {
	hosts, err := r.store.ListFailedHosts(ctx)
	if err != nil {
		return fmt.Errorf("list failed hosts: %w", err)
	}

	for _, host := range hosts {
		if r.queuer == nil {
			continue
		}

		if _, err := r.queuer.QueueHostAction(ctx, host.ID, agentapi.ActionStartHost, "system"); err != nil {
			r.logger.Warn("auto-recover failed host skipped",
				"host_id", host.ID, "error", err)
			continue
		}

		r.store.RecordEvent(ctx, repository.RecordEventParams{
			HostID:  &host.ID,
			Level:   "info",
			Type:    "reconcile.host.recover_from_failed",
			Message: "对账自动恢复失败主机",
			Metadata: map[string]any{
				"operator":  "system",
				"db_status": "failed",
				"host_id":   host.ID,
			},
		})

		r.logger.Info("reconciled failed host recovery queued",
			"host_id", host.ID, "db_status", "failed")
	}

	return nil
}

func (r *Reconciler) reconcileStaleTasks(ctx context.Context) error {
	staleTasks, err := r.store.MarkStaleTasks(ctx, r.staleThreshold)
	if err != nil {
		return fmt.Errorf("mark stale tasks: %w", err)
	}

	for _, task := range staleTasks {
		r.store.RecordEvent(ctx, repository.RecordEventParams{
			HostID:  task.HostID,
			Level:   "warn",
			Type:    "reconcile.task.stale",
			Message: "对账发现陈旧任务",
			Metadata: map[string]any{
				"operator":   "system",
				"task_id":    task.ID,
				"kind":       task.Kind,
				"old_status": "pending/running",
				"threshold":  r.staleThreshold.String(),
			},
		})

		r.logger.Info("marked stale task", "task_id", task.ID, "kind", task.Kind)
	}

	if len(staleTasks) > 0 {
		r.logger.Info("stale task reconciliation completed", "stale_count", len(staleTasks))
	}

	return nil
}
