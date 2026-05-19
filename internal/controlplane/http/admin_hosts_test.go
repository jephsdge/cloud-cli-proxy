package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
	"github.com/zanel1u/cloud-cli-proxy/internal/store/repository"
)

type stubHostStore struct {
	hosts         []repository.HostWithUsername
	detail        repository.HostDetail
	host          repository.Host
	runningHosts  []repository.Host
	hostWithCA    repository.HostWithClaudeAccount
	hostWithCAErr error
	listErr       error
	detailErr     error
	hostErr       error
	runningErr    error
	timezone      string
	timezoneErr   error
	identity      repository.WorkerIdentity
	identityErr   error
	ports         repository.HostPorts
}

func (s *stubHostStore) ListHostsWithUsername(_ context.Context) ([]repository.HostWithUsername, error) {
	return s.hosts, s.listErr
}

func (s *stubHostStore) GetHostDetail(_ context.Context, _ string) (repository.HostDetail, error) {
	return s.detail, s.detailErr
}

func (s *stubHostStore) GetHost(_ context.Context, _ string) (repository.Host, error) {
	return s.host, s.hostErr
}

func (s *stubHostStore) UpsertHost(_ context.Context, _ repository.UpsertHostParams) (repository.Host, error) {
	return repository.Host{}, nil
}

func (s *stubHostStore) GetUser(_ context.Context, _ string) (repository.User, error) {
	return repository.User{}, nil
}

func (s *stubHostStore) BindEgressIPToHost(_ context.Context, _ string, _ string) (repository.HostBinding, error) {
	return repository.HostBinding{}, nil
}

func (s *stubHostStore) DeleteHost(_ context.Context, _ string) error {
	return nil
}

func (s *stubHostStore) ListRunningHosts(_ context.Context) ([]repository.Host, error) {
	return s.runningHosts, s.runningErr
}

func (s *stubHostStore) ListHostsByUserID(_ context.Context, _ string) ([]repository.Host, error) {
	return nil, nil
}

func (s *stubHostStore) GetHostWithClaudeAccount(_ context.Context, _ string) (repository.HostWithClaudeAccount, error) {
	return s.hostWithCA, s.hostWithCAErr
}

func (s *stubHostStore) UpdateHostMounts(_ context.Context, _ string, _ repository.HostMounts) error {
	return nil
}

func (s *stubHostStore) UpdateHostPorts(_ context.Context, _ string, ports repository.HostPorts) error {
	s.ports = ports
	return nil
}

func (s *stubHostStore) UpdateHostTimezone(_ context.Context, _ string, timezone string) error {
	s.timezone = timezone
	return s.timezoneErr
}

func (s *stubHostStore) UpdateHostIdentity(_ context.Context, _ string, identity repository.WorkerIdentity) error {
	s.identity = identity
	return s.identityErr
}

func (s *stubHostStore) UpdateHostGatewayConfig(_ context.Context, _, _ string, _ json.RawMessage) error {
	return nil
}

func TestAdminHostsHandler(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	sampleHost := repository.HostWithUsername{
		Host:     repository.Host{ID: "h1", UserID: "u1", Status: "stopped", CreatedAt: now, UpdatedAt: now},
		Username: "testuser",
	}
	sampleDetail := repository.HostDetail{
		Host:     repository.Host{ID: "h1", UserID: "u1", Status: "stopped", CreatedAt: now, UpdatedAt: now},
		User:     repository.User{ID: "u1", Username: "testuser", Status: "active", CreatedAt: now, UpdatedAt: now},
		Bindings: []repository.BindingWithIP{},
	}

	tests := []struct {
		name       string
		method     string
		path       string
		hostStore  *stubHostStore
		queue      *stubQueuer
		wantStatus int
		wantField  string
		wantAction agentapi.HostAction
		wantQueued bool
	}{
		{
			name:       "List hosts 200",
			method:     "GET",
			path:       "/v1/admin/hosts",
			hostStore:  &stubHostStore{hosts: []repository.HostWithUsername{sampleHost}},
			queue:      &stubQueuer{},
			wantStatus: 200,
			wantField:  "hosts",
		},
		{
			name:       "List hosts store error 500",
			method:     "GET",
			path:       "/v1/admin/hosts",
			hostStore:  &stubHostStore{listErr: fmt.Errorf("db down")},
			queue:      &stubQueuer{},
			wantStatus: 500,
		},
		{
			name:       "Get host detail 200",
			method:     "GET",
			path:       "/v1/admin/hosts/h1",
			hostStore:  &stubHostStore{detail: sampleDetail},
			queue:      &stubQueuer{},
			wantStatus: 200,
			wantField:  "host",
		},
		{
			name:       "Get host 404",
			method:     "GET",
			path:       "/v1/admin/hosts/missing",
			hostStore:  &stubHostStore{detailErr: pgx.ErrNoRows},
			queue:      &stubQueuer{},
			wantStatus: 404,
		},
		{
			name:       "Start host 202",
			method:     "POST",
			path:       "/v1/admin/hosts/h1/start",
			hostStore:  &stubHostStore{},
			queue:      &stubQueuer{task: repository.Task{ID: "t1"}},
			wantStatus: 202,
			wantQueued: true,
			wantAction: agentapi.ActionStartHost,
		},
		{
			name:       "Start host 404",
			method:     "POST",
			path:       "/v1/admin/hosts/missing/start",
			hostStore:  &stubHostStore{},
			queue:      &stubQueuer{err: pgx.ErrNoRows},
			wantStatus: 404,
		},
		{
			name:       "Stop host 202",
			method:     "POST",
			path:       "/v1/admin/hosts/h1/stop",
			hostStore:  &stubHostStore{},
			queue:      &stubQueuer{task: repository.Task{ID: "t2"}},
			wantStatus: 202,
			wantQueued: true,
			wantAction: agentapi.ActionStopHost,
		},
		{
			name:       "Rebuild host 202",
			method:     "POST",
			path:       "/v1/admin/hosts/h1/rebuild",
			hostStore:  &stubHostStore{},
			queue:      &stubQueuer{task: repository.Task{ID: "t3"}},
			wantStatus: 202,
			wantQueued: true,
			wantAction: agentapi.ActionRebuildHost,
		},
		{
			name:       "Restart VNC host 409 when not running",
			method:     "POST",
			path:       "/v1/admin/hosts/h1/vnc/restart",
			hostStore:  &stubHostStore{host: repository.Host{ID: "h1", Status: "stopped"}},
			queue:      &stubQueuer{},
			wantStatus: 409,
		},
		{
			name:       "Restart VNC host 404",
			method:     "POST",
			path:       "/v1/admin/hosts/missing/vnc/restart",
			hostStore:  &stubHostStore{hostErr: pgx.ErrNoRows},
			queue:      &stubQueuer{},
			wantStatus: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := &stubEventRecorder{}
			router := adminTestRouter(t, Dependencies{
				Logger:        slog.Default(),
				AdminHosts:    tt.hostStore,
				HostActions:   tt.queue,
				EventRecorder: events,
			})
			srv := httptest.NewServer(router)
			defer srv.Close()

			req, _ := nethttp.NewRequest(tt.method, srv.URL+tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+validAdminToken(t))

			resp, err := nethttp.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				var respBody map[string]any
				json.NewDecoder(resp.Body).Decode(&respBody)
				t.Errorf("status = %d, want %d; body = %v", resp.StatusCode, tt.wantStatus, respBody)
				return
			}

			if tt.wantQueued && !tt.queue.called {
				t.Error("expected queue to be called")
			}
			if tt.wantQueued && tt.queue.calledAction != tt.wantAction {
				t.Errorf("queue action = %v, want %v", tt.queue.calledAction, tt.wantAction)
			}

			if tt.wantField != "" {
				var respBody map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if _, ok := respBody[tt.wantField]; !ok {
					t.Errorf("response missing field %q: %v", tt.wantField, respBody)
				}
			}
		})
	}
}

func TestAdminHostsUpdatePortsQueuesRefreshForRunningHost(t *testing.T) {
	store := &stubHostStore{
		host: repository.Host{ID: "h1", Status: "running"},
	}
	queue := &stubQueuer{task: repository.Task{ID: "refresh-task"}}
	handler := NewAdminHostsHandler(slog.Default(), store, queue, &stubEventRecorder{}, "").UpdatePorts()

	body := `{"ports":[{"host_port":5353,"container_port":5353,"protocol":"UDP"}]}`
	req := httptest.NewRequest("PUT", "/v1/admin/hosts/h1/ports", strings.NewReader(body))
	req.SetPathValue("hostID", "h1")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != nethttp.StatusOK {
		var respBody map[string]any
		json.NewDecoder(rec.Body).Decode(&respBody)
		t.Fatalf("status = %d, want 200; body = %v", rec.Code, respBody)
	}
	if len(store.ports) != 1 || store.ports[0].Protocol != "udp" {
		t.Fatalf("stored ports = %#v, want normalized udp", store.ports)
	}
	if !queue.called || queue.calledAction != agentapi.ActionPrepareHost || queue.calledRequestedBy != "admin" {
		t.Fatalf("queue = called:%v action:%v requested_by:%q, want prepare_host by admin",
			queue.called, queue.calledAction, queue.calledRequestedBy)
	}

	var respBody map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&respBody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if respBody["refresh_status"] != "queued" {
		t.Fatalf("refresh_status = %v, want queued", respBody["refresh_status"])
	}
}

func TestAdminHostsUpdateTimezone(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	store := &stubHostStore{
		detail: repository.HostDetail{
			Host: repository.Host{
				ID:        "h1",
				UserID:    "u1",
				Status:    "running",
				Timezone:  "America/Los_Angeles",
				CreatedAt: now,
				UpdatedAt: now,
			},
			User: repository.User{ID: "u1", Username: "testuser", Status: "active", CreatedAt: now, UpdatedAt: now},
		},
	}
	router := adminTestRouter(t, Dependencies{
		Logger:        slog.Default(),
		AdminHosts:    store,
		HostActions:   &stubQueuer{},
		EventRecorder: &stubEventRecorder{},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, _ := nethttp.NewRequest(
		"PUT",
		srv.URL+"/v1/admin/hosts/h1/timezone",
		strings.NewReader(`{"timezone":"America/New_York"}`),
	)
	req.Header.Set("Authorization", "Bearer "+validAdminToken(t))
	req.Header.Set("Content-Type", "application/json")

	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != nethttp.StatusOK {
		var respBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&respBody)
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, respBody)
	}
	if store.timezone != "America/New_York" {
		t.Fatalf("timezone = %q, want America/New_York", store.timezone)
	}

	var body updateHostTimezoneResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.RequiresRebuild {
		t.Fatal("running host timezone update should require rebuild")
	}
}

func TestAdminHostsUpdateIdentity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	store := &stubHostStore{
		detail: repository.HostDetail{
			Host: repository.Host{
				ID:        "h1",
				UserID:    "u1",
				Status:    "stopped",
				Hostname:  "old-host",
				Timezone:  "America/Los_Angeles",
				CreatedAt: now,
				UpdatedAt: now,
			},
			User: repository.User{ID: "u1", Username: "testuser", Status: "active", CreatedAt: now, UpdatedAt: now},
		},
	}
	router := adminTestRouter(t, Dependencies{
		Logger:        slog.Default(),
		AdminHosts:    store,
		HostActions:   &stubQueuer{},
		EventRecorder: &stubEventRecorder{},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := `{"identity":{"hostname":"fixed-host","timezone":"America/New_York","machine_id":"0123456789abcdef0123456789abcdef","locale":{"LANG":"en_US.UTF-8","LANGUAGE":"en_US:en","LC_ALL":"en_US.UTF-8"},"vnc_resolution":"1920x1080","browser_language":"en-US","browser_window_size":"1365x768"}}`
	req, _ := nethttp.NewRequest("PUT", srv.URL+"/v1/admin/hosts/h1/identity", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+validAdminToken(t))
	req.Header.Set("Content-Type", "application/json")

	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != nethttp.StatusOK {
		var respBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&respBody)
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, respBody)
	}
	if store.identity.Hostname != "fixed-host" {
		t.Fatalf("hostname = %q, want fixed-host", store.identity.Hostname)
	}
	if store.identity.Timezone != "America/New_York" {
		t.Fatalf("timezone = %q, want America/New_York", store.identity.Timezone)
	}
	if store.identity.MachineID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("machine_id = %q", store.identity.MachineID)
	}
}

func TestNormalizeHostTimezoneRejectsInvalidValue(t *testing.T) {
	if _, err := normalizeHostTimezone("../etc/passwd"); err == nil {
		t.Fatal("expected invalid timezone error")
	}
	if got, err := normalizeHostTimezone(" America/New_York "); err != nil || got != "America/New_York" {
		t.Fatalf("normalizeHostTimezone = %q, %v; want America/New_York, nil", got, err)
	}
}

func TestNormalizeWorkerIdentityRejectsInvalidMachineID(t *testing.T) {
	fallback := defaultWorkerIdentity("host-1", "America/New_York", "")
	_, err := normalizeWorkerIdentity(repository.WorkerIdentity{MachineID: "not-hex"}, fallback)
	if err == nil {
		t.Fatal("expected invalid machine_id error")
	}
}

func TestNormalizeWorkerIdentityUsesFallbacks(t *testing.T) {
	fallback := defaultWorkerIdentity("host-1", "America/New_York", "0123456789abcdef0123456789abcdef")
	identity, err := normalizeWorkerIdentity(repository.WorkerIdentity{BrowserWindowSize: "1280X720"}, fallback)
	if err != nil {
		t.Fatalf("normalize worker identity: %v", err)
	}
	if identity.Hostname != "host-1" || identity.Timezone != "America/New_York" {
		t.Fatalf("identity fallback mismatch: %+v", identity)
	}
	if identity.BrowserWindowSize != "1280x720" {
		t.Fatalf("browser_window_size = %q, want 1280x720", identity.BrowserWindowSize)
	}
}

func TestHostTimezoneRequiresRebuild(t *testing.T) {
	for _, status := range []string{"running", "stopped", "failed"} {
		if !hostTimezoneRequiresRebuild(status) {
			t.Fatalf("status %q should require rebuild", status)
		}
	}
	if hostTimezoneRequiresRebuild("pending") {
		t.Fatal("pending host should not require rebuild")
	}
}

func TestHostLogsContainerName(t *testing.T) {
	tests := []struct {
		name          string
		target        string
		wantName      string
		wantTarget    string
		wantSupported bool
	}{
		{
			name:          "default worker",
			target:        "",
			wantName:      "cloudproxy-h1",
			wantTarget:    "worker",
			wantSupported: true,
		},
		{
			name:          "worker",
			target:        "worker",
			wantName:      "cloudproxy-h1",
			wantTarget:    "worker",
			wantSupported: true,
		},
		{
			name:          "gateway",
			target:        "gateway",
			wantName:      "cloudproxy-gw-h1",
			wantTarget:    "gateway",
			wantSupported: true,
		},
		{
			name:          "unsupported",
			target:        "postgres",
			wantTarget:    "postgres",
			wantSupported: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotTarget, gotSupported := hostLogsContainerName("h1", tt.target)
			if gotSupported != tt.wantSupported {
				t.Fatalf("supported = %v, want %v", gotSupported, tt.wantSupported)
			}
			if gotName != tt.wantName {
				t.Fatalf("container = %q, want %q", gotName, tt.wantName)
			}
			if gotTarget != tt.wantTarget {
				t.Fatalf("target = %q, want %q", gotTarget, tt.wantTarget)
			}
		})
	}
}

func TestBuildEffectiveGatewayConfig_AutoIgnoresHostGatewayConfig(t *testing.T) {
	detail := repository.HostDetail{
		Host: repository.Host{ID: "h1"},
		Bindings: []repository.BindingWithIP{{
			EgressIP: repository.EgressIP{
				ProxyConfig: json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080,"dns_server":"1.1.1.1"}`),
			},
		}},
	}

	effective, err := buildEffectiveGatewayConfig(detail, repository.GatewayConfigModeAuto, json.RawMessage(`{"type":"trojan","server":"5.6.7.8","server_port":443,"password":"secret","dns_server":"9.9.9.9"}`))
	if err != nil {
		t.Fatalf("build effective config: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(effective, &cfg); err != nil {
		t.Fatalf("decode effective config: %v", err)
	}

	route := cfg["route"].(map[string]any)
	if got := route["final"]; got != "proxy-out" {
		t.Fatalf("route.final=%v, want proxy-out", got)
	}

	outbounds := cfg["outbounds"].([]any)
	first := outbounds[0].(map[string]any)
	if got := first["type"]; got != "socks" {
		t.Fatalf("first outbound type=%v, want socks", got)
	}
	if got := first["tag"]; got != "proxy-out" {
		t.Fatalf("first outbound tag=%v, want proxy-out", got)
	}

	dns := cfg["dns"].(map[string]any)
	servers := dns["servers"].([]any)
	if got := servers[0].(map[string]any)["server"]; got != "1.1.1.1" {
		t.Fatalf("dns server=%v, want 1.1.1.1", got)
	}
}

func TestBuildEffectiveGatewayConfig_FullConfigPreservesCustomFinal(t *testing.T) {
	detail := repository.HostDetail{
		Host: repository.Host{ID: "h1"},
		Bindings: []repository.BindingWithIP{{
			EgressIP: repository.EgressIP{
				ProxyConfig: json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080,"dns_server":"1.1.1.1"}`),
			},
		}},
	}

	full := json.RawMessage(`{
		"inbounds": [{"type":"tun","tag":"tun-in","address":["172.19.0.1/30"],"auto_route":true}],
		"outbounds": [{"type":"socks","tag":"custom-out","server":"5.6.7.8","server_port":1080}],
		"route": {"final":"custom-out","rules":[]}
	}`)
	effective, err := buildEffectiveGatewayConfig(detail, repository.GatewayConfigModeCustom, full)
	if err != nil {
		t.Fatalf("build effective config: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(effective, &cfg); err != nil {
		t.Fatalf("decode effective config: %v", err)
	}

	route := cfg["route"].(map[string]any)
	if got := route["final"]; got != "custom-out" {
		t.Fatalf("route.final=%v, want custom-out", got)
	}
	outbounds := cfg["outbounds"].([]any)
	for _, item := range outbounds {
		m := item.(map[string]any)
		if m["tag"] == "proxy-out" {
			t.Fatal("full gateway config must not inject proxy-out")
		}
	}
}

func TestAdminHostDetail_IncludesPersistentVolumeName_WhenAvailable(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	store := &stubHostStore{
		detail: repository.HostDetail{
			Host:     repository.Host{ID: "h-1", UserID: "u-1", Status: "running", CreatedAt: now, UpdatedAt: now},
			User:     repository.User{ID: "u-1", Username: "u", Status: "active", CreatedAt: now, UpdatedAt: now},
			Bindings: []repository.BindingWithIP{},
		},
		hostWithCA: repository.HostWithClaudeAccount{
			Host:                 repository.Host{ID: "h-1"},
			PersistentVolumeName: "claude-state-acct-1",
		},
	}
	router := adminTestRouter(t, Dependencies{
		Logger:        slog.Default(),
		AdminHosts:    store,
		HostActions:   &stubQueuer{},
		EventRecorder: &stubEventRecorder{},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, _ := nethttp.NewRequest("GET", srv.URL+"/v1/admin/hosts/h-1", nil)
	req.Header.Set("Authorization", "Bearer "+validAdminToken(t))
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, _ := body["persistent_volume_name"].(string); v != "claude-state-acct-1" {
		t.Errorf("persistent_volume_name=%q, want claude-state-acct-1; full=%v", v, body)
	}
}

func TestAdminHostDetail_OmitsPersistentVolumeName_WhenEmpty(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	store := &stubHostStore{
		detail: repository.HostDetail{
			Host:     repository.Host{ID: "h-1", UserID: "u-1", Status: "running", CreatedAt: now, UpdatedAt: now},
			User:     repository.User{ID: "u-1", Username: "u", Status: "active", CreatedAt: now, UpdatedAt: now},
			Bindings: []repository.BindingWithIP{},
		},
		hostWithCA: repository.HostWithClaudeAccount{
			Host:                 repository.Host{ID: "h-1"},
			PersistentVolumeName: "",
		},
	}
	router := adminTestRouter(t, Dependencies{
		Logger:        slog.Default(),
		AdminHosts:    store,
		HostActions:   &stubQueuer{},
		EventRecorder: &stubEventRecorder{},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, _ := nethttp.NewRequest("GET", srv.URL+"/v1/admin/hosts/h-1", nil)
	req.Header.Set("Authorization", "Bearer "+validAdminToken(t))
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["persistent_volume_name"]; ok {
		t.Errorf("persistent_volume_name key must be omitted when empty (omitempty); body=%v", body)
	}
}

func TestAdminHostList_DoesNotIncludePersistentVolumeName(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	store := &stubHostStore{
		hosts: []repository.HostWithUsername{
			{
				Host:     repository.Host{ID: "h-1", UserID: "u-1", Status: "running", CreatedAt: now, UpdatedAt: now},
				Username: "u",
			},
		},
	}
	router := adminTestRouter(t, Dependencies{
		Logger:        slog.Default(),
		AdminHosts:    store,
		HostActions:   &stubQueuer{},
		EventRecorder: &stubEventRecorder{},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, _ := nethttp.NewRequest("GET", srv.URL+"/v1/admin/hosts", nil)
	req.Header.Set("Authorization", "Bearer "+validAdminToken(t))
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hosts, ok := body["hosts"].([]any)
	if !ok || len(hosts) == 0 {
		t.Fatalf("expected hosts[] non-empty; body=%v", body)
	}
	first, _ := hosts[0].(map[string]any)
	if _, has := first["persistent_volume_name"]; has {
		t.Errorf("OOS-A19: list endpoint must NOT include persistent_volume_name; got %v", first)
	}
}
