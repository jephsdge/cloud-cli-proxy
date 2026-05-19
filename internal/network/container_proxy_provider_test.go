package network

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestNetworkName(t *testing.T) {
	tests := []struct {
		hostID string
		want   string
	}{
		{hostID: "abc123", want: "cloudproxy-net-abc123"},
		{hostID: "host-1", want: "cloudproxy-net-host-1"},
		{hostID: "", want: "cloudproxy-net-"},
	}
	for _, tt := range tests {
		t.Run(tt.hostID, func(t *testing.T) {
			got := networkName(tt.hostID)
			if got != tt.want {
				t.Errorf("networkName(%q) = %q, want %q", tt.hostID, got, tt.want)
			}
		})
	}
}

func TestGatewayContainerName(t *testing.T) {
	got := gatewayContainerName("my-host")
	want := "cloudproxy-gw-my-host"
	if got != want {
		t.Errorf("gatewayContainerName = %q, want %q", got, want)
	}
}

func TestWorkerContainerName(t *testing.T) {
	got := workerContainerName("my-host")
	want := "cloudproxy-my-host"
	if got != want {
		t.Errorf("workerContainerName = %q, want %q", got, want)
	}
}

func TestSubnetThirdOctet_Deterministic(t *testing.T) {
	// Same input always produces same output
	hostID := "test-host-id"
	first := subnetThirdOctet(hostID)
	for i := 0; i < 10; i++ {
		if got := subnetThirdOctet(hostID); got != first {
			t.Errorf("subnetThirdOctet not deterministic: got %d, want %d", got, first)
		}
	}
}

func TestSubnetThirdOctet_Range(t *testing.T) {
	// The third octet should be in range [20, 219]
	hostIDs := []string{"a", "abc", "host-1", "550e8400-e29b-41d4-a716-446655440000", "test", "very-long-host-id-that-goes-on-and-on"}
	for _, hid := range hostIDs {
		octet := subnetThirdOctet(hid)
		if octet < 20 || octet > 219 {
			t.Errorf("subnetThirdOctet(%q) = %d, want in range [20, 219]", hid, octet)
		}
	}
}

func TestSubnetThirdOctet_DifferentInputs(t *testing.T) {
	// Different inputs may produce different outputs (hash collisions possible but rare)
	results := make(map[string]int)
	for _, hid := range []string{"host-a", "host-b", "host-c"} {
		results[hid] = subnetThirdOctet(hid)
	}
	// This is a probabilistic test - FNV should distribute well
	unique := make(map[int]bool)
	for _, v := range results {
		unique[v] = true
	}
	if len(unique) < 2 {
		t.Log("all inputs produced same octet (possible FNV collision, not necessarily a bug)")
	}
}

func TestGatewayConfigDir_WithDataDir(t *testing.T) {
	os.Setenv("DATA_DIR", "/custom/data")
	defer os.Unsetenv("DATA_DIR")

	got := gatewayConfigDir("host-1")
	want := "/custom/data/gateway/host-1"
	if got != want {
		t.Errorf("gatewayConfigDir = %q, want %q", got, want)
	}
}

func TestGatewayConfigDir_Default(t *testing.T) {
	os.Unsetenv("DATA_DIR")

	got := gatewayConfigDir("host-1")
	want := "/var/lib/cloud-cli-proxy/gateway/host-1"
	if got != want {
		t.Errorf("gatewayConfigDir = %q, want %q", got, want)
	}
}

func TestGatewayImage_Custom(t *testing.T) {
	os.Setenv("CLOUD_CLI_PROXY_GATEWAY_IMAGE", "my-custom-image:v2")
	defer os.Unsetenv("CLOUD_CLI_PROXY_GATEWAY_IMAGE")

	got := GatewayImage()
	if got != "my-custom-image:v2" {
		t.Errorf("GatewayImage = %q, want %q", got, "my-custom-image:v2")
	}
}

func TestGatewayImage_Default(t *testing.T) {
	os.Unsetenv("CLOUD_CLI_PROXY_GATEWAY_IMAGE")

	got := GatewayImage()
	want := "cloud-cli-proxy-sing-gateway:local"
	if got != want {
		t.Errorf("GatewayImage = %q, want %q", got, want)
	}
}

func TestGatewayNetworkMTU_Default(t *testing.T) {
	t.Setenv(gatewayNetworkMTUEnv, "")

	got, err := gatewayNetworkMTU()
	if err != nil {
		t.Fatalf("gatewayNetworkMTU returned error: %v", err)
	}
	if got != 0 {
		t.Errorf("gatewayNetworkMTU = %d, want 0", got)
	}
}

func TestGatewayNetworkMTU_Custom(t *testing.T) {
	t.Setenv(gatewayNetworkMTUEnv, "1400")

	got, err := gatewayNetworkMTU()
	if err != nil {
		t.Fatalf("gatewayNetworkMTU returned error: %v", err)
	}
	if got != 1400 {
		t.Errorf("gatewayNetworkMTU = %d, want 1400", got)
	}
}

func TestGatewayNetworkMTU_Invalid(t *testing.T) {
	t.Setenv(gatewayNetworkMTUEnv, "abc")

	if _, err := gatewayNetworkMTU(); err == nil {
		t.Fatal("gatewayNetworkMTU expected error for invalid value")
	}
}

func TestGatewayEgressNetworkSubnet_Default(t *testing.T) {
	hostID := "test-host-id"
	third := subnetThirdOctet(hostID)

	subnet, gateway, err := gatewayEgressNetworkSubnet(hostID)
	if err != nil {
		t.Fatalf("gatewayEgressNetworkSubnet returned error: %v", err)
	}

	if want := "172.30." + strconv.Itoa(third) + ".0/24"; subnet != want {
		t.Errorf("egress subnet = %q, want %q", subnet, want)
	}
	if want := "172.30." + strconv.Itoa(third) + ".1"; gateway != want {
		t.Errorf("egress gateway = %q, want %q", gateway, want)
	}
}

func TestGatewayEgressNetworkSubnet_CustomBase(t *testing.T) {
	t.Setenv(gatewayEgressNetworkBaseEnv, "172.31.0.0/16")
	hostID := "test-host-id"
	third := subnetThirdOctet(hostID)

	subnet, gateway, err := gatewayEgressNetworkSubnet(hostID)
	if err != nil {
		t.Fatalf("gatewayEgressNetworkSubnet returned error: %v", err)
	}

	if want := "172.31." + strconv.Itoa(third) + ".0/24"; subnet != want {
		t.Errorf("egress subnet = %q, want %q", subnet, want)
	}
	if want := "172.31." + strconv.Itoa(third) + ".1"; gateway != want {
		t.Errorf("egress gateway = %q, want %q", gateway, want)
	}
}

func TestGatewayEgressNetworkSubnet_InvalidBase(t *testing.T) {
	t.Setenv(gatewayEgressNetworkBaseEnv, "172.31.20.0/24")

	if _, _, err := gatewayEgressNetworkSubnet("test-host-id"); err == nil {
		t.Fatal("gatewayEgressNetworkSubnet expected error for non-/16 base")
	}
}

func TestDockerNetworkCreateArgs_WithMTU(t *testing.T) {
	got := dockerNetworkCreateArgs("cloudproxy-net-host", "10.99.20.0/24", "10.99.20.1", 1400)
	want := []string{
		"network",
		"create",
		"--driver", "bridge",
		"--opt", "com.docker.network.driver.mtu=1400",
		"--subnet", "10.99.20.0/24",
		"--gateway", "10.99.20.1",
		"cloudproxy-net-host",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dockerNetworkCreateArgs = %#v, want %#v", got, want)
	}
}

func TestDockerNetworkCreateArgs_DefaultMTU(t *testing.T) {
	got := dockerNetworkCreateArgs("cloudproxy-net-host", "10.99.20.0/24", "10.99.20.1", 0)
	for i := 0; i < len(got)-1; i++ {
		if got[i] == "--opt" && strings.Contains(got[i+1], "mtu") {
			t.Fatalf("dockerNetworkCreateArgs must not set MTU when value is 0: %#v", got)
		}
	}
}

func TestDockerEgressNetworkCreateArgs_WithMTU(t *testing.T) {
	got := dockerEgressNetworkCreateArgs("cloudproxy-egress-host", "172.30.20.0/24", "172.30.20.1", 1400)
	want := []string{
		"network",
		"create",
		"--driver", "bridge",
		"--opt", "com.docker.network.driver.mtu=1400",
		"--subnet", "172.30.20.0/24",
		"--gateway", "172.30.20.1",
		"cloudproxy-egress-host",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dockerEgressNetworkCreateArgs = %#v, want %#v", got, want)
	}
}

func TestNewContainerProxyProvider(t *testing.T) {
	p := NewContainerProxyProvider(nil)
	if p == nil {
		t.Fatal("NewContainerProxyProvider returned nil")
	}
	if p.logger != nil {
		t.Error("expected nil logger when passed nil")
	}
}

func TestPrepareHost_ConnectsGatewayNetworksBeforeStart(t *testing.T) {
	source, err := os.ReadFile("container_proxy_provider.go")
	if err != nil {
		t.Fatalf("read provider source: %v", err)
	}

	body := string(source)
	createEgressIdx := strings.Index(body, "dockerEgressNetworkCreate(ctx, egressNetName, egressSubnet, egressGateway, networkMTU)")
	createGatewayIdx := strings.Index(body, "dockerCreateGateway(ctx, gwName, egressNetName")
	connectIsolatedIdx := strings.Index(body, "dockerNetworkConnect(ctx, netName, gwName, gwIP)")
	startIdx := strings.Index(body, "dockerStartGateway(ctx")
	if createEgressIdx < 0 || createGatewayIdx < 0 || connectIsolatedIdx < 0 || startIdx < 0 {
		t.Fatalf("expected create/connect/start calls to exist, got egress=%d create=%d connect=%d start=%d", createEgressIdx, createGatewayIdx, connectIsolatedIdx, startIdx)
	}
	if !(createEgressIdx < createGatewayIdx && createGatewayIdx < connectIsolatedIdx && connectIsolatedIdx < startIdx) {
		t.Fatalf("gateway call order must be egress network -> create gateway -> connect isolated network -> start, got egress=%d create=%d connect=%d start=%d", createEgressIdx, createGatewayIdx, connectIsolatedIdx, startIdx)
	}
	if strings.Contains(body, `dockerNetworkConnect(ctx, "bridge", gwName, "")`) {
		t.Fatal("gateway must not use Docker default bridge as its managed egress network")
	}
}

func TestPrepareHost_AlwaysInstallsWorkerRouting(t *testing.T) {
	source, err := os.ReadFile("container_proxy_provider.go")
	if err != nil {
		t.Fatalf("read provider source: %v", err)
	}

	body := string(source)
	routingIdx := strings.Index(body, "宿主机 iptables / policy routing 规则")
	if routingIdx < 0 {
		t.Fatal("expected host-side routing setup comment to exist")
	}
	routingBody := body[routingIdx:]
	setupIdx := strings.Index(routingBody, "setupPortForwarding(ctx, hostID, bridgeGW, gwIP, spec.PortMappings)")
	if setupIdx < 0 {
		t.Fatal("expected setupPortForwarding call to exist")
	}
	guardIdx := strings.LastIndex(routingBody[:setupIdx], "if len(spec.PortMappings) > 0")
	if guardIdx >= 0 {
		t.Fatalf("setupPortForwarding must not be guarded by PortMappings length; found guard before call at %d", guardIdx)
	}
}

func TestRefreshHost_DoesNotRecreateGateway(t *testing.T) {
	source, err := os.ReadFile("container_proxy_provider.go")
	if err != nil {
		t.Fatalf("read provider source: %v", err)
	}

	body := string(source)
	startIdx := strings.Index(body, "func (p *ContainerProxyProvider) RefreshHost")
	if startIdx < 0 {
		t.Fatal("expected RefreshHost function to exist")
	}
	endIdx := strings.Index(body[startIdx:], "func (p *ContainerProxyProvider) CleanupHost")
	if endIdx < 0 {
		t.Fatal("expected CleanupHost function to exist after RefreshHost")
	}
	refreshBody := body[startIdx : startIdx+endIdx]
	for _, forbidden := range []string{"teardownGateway", "dockerCreateGateway", "dockerStartGateway", "dockerNetworkCreate("} {
		if strings.Contains(refreshBody, forbidden) {
			t.Fatalf("RefreshHost must not recreate gateway/network; found %q", forbidden)
		}
	}
	if !strings.Contains(refreshBody, "setupPortForwarding(ctx, hostID, bridgeGW, gwIP, spec.PortMappings)") {
		t.Fatal("RefreshHost must refresh host-side port forwarding")
	}
}
