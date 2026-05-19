package tasks

import (
	"context"
	"errors"
	"testing"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
	"github.com/zanel1u/cloud-cli-proxy/internal/store/repository"
)

func TestExecutePrepareHostSuccessDoesNotUpdateHostStatus(t *testing.T) {
	repo := &fakeWorkerRepo{egressIP: testEgressIP()}
	provider := &fakeNetworkProvider{}
	w := NewWorker(repo, provider)

	update := w.Execute(context.Background(), agentapi.HostActionRequest{
		TaskID: "task-prepare",
		HostID: "host-prepare",
		Action: agentapi.ActionPrepareHost,
	})

	if update.Status != taskStateSucceeded {
		t.Fatalf("status = %q, want %q", update.Status, taskStateSucceeded)
	}
	if len(repo.hostStatuses) != 0 {
		t.Fatalf("prepare_host must not update host status, got %#v", repo.hostStatuses)
	}
	if len(provider.refreshed) != 1 {
		t.Fatalf("expected RefreshHost once, got %d", len(provider.refreshed))
	}
}

func TestExecutePrepareHostFailureDoesNotStopOrFailHost(t *testing.T) {
	repo := &fakeWorkerRepo{egressIP: testEgressIP()}
	provider := &fakeNetworkProvider{refreshErr: errors.New("refresh failed")}
	w := NewWorker(repo, provider)

	update := w.Execute(context.Background(), agentapi.HostActionRequest{
		TaskID: "task-prepare",
		HostID: "host-prepare",
		Action: agentapi.ActionPrepareHost,
	})

	if update.Status != taskStateFailed {
		t.Fatalf("status = %q, want %q", update.Status, taskStateFailed)
	}
	if len(repo.hostStatuses) != 0 {
		t.Fatalf("prepare_host failure must not update host status, got %#v", repo.hostStatuses)
	}
}

func testEgressIP() repository.EgressIP {
	return repository.EgressIP{
		ID:        "egress-1",
		IPAddress: "203.0.113.10",
		ProxyConfig: []byte(`{
			"type":"vless",
			"server":"proxy.example.com",
			"server_port":443
		}`),
	}
}
