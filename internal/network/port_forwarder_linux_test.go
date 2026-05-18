//go:build linux

package network

import (
	"net"
	"testing"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
)

func TestTCPPortForwardSpecsNormalizesTCP(t *testing.T) {
	specs, err := tcpPortForwardSpecs([]agentapi.PortMapping{
		{HostPort: 8080, ContainerPort: 80},
		{HostPort: 8443, ContainerPort: 443, Protocol: "TCP"},
		{HostPort: 0, ContainerPort: 9000},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: got %d want 2", len(specs))
	}
	if specs[0].hostPort != 8080 || specs[0].workerPort != 80 {
		t.Fatalf("unexpected first spec: %#v", specs[0])
	}
	if specs[1].hostPort != 8443 || specs[1].workerPort != 443 {
		t.Fatalf("unexpected second spec: %#v", specs[1])
	}
}

func TestTCPPortForwardSpecsRejectsUDP(t *testing.T) {
	_, err := tcpPortForwardSpecs([]agentapi.PortMapping{
		{HostPort: 5353, ContainerPort: 5353, Protocol: "udp"},
	})
	if err == nil {
		t.Fatal("expected udp to be rejected")
	}
}

func TestStartPortForwardersWithNoPortsStopsExistingListeners(t *testing.T) {
	hostID := "host-empty-ports"
	ln := &fakeListener{}
	fwdMu.Lock()
	fwdByHost[hostID] = []net.Listener{ln}
	fwdMu.Unlock()

	if err := startPortForwarders(hostID, "10.99.1.3", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fwdMu.Lock()
	_, exists := fwdByHost[hostID]
	fwdMu.Unlock()
	if exists {
		t.Fatal("expected listener registry to be cleared")
	}
	if !ln.closed {
		t.Fatal("expected listener to be closed")
	}
}

type fakeListener struct {
	closed bool
}

func (f *fakeListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (f *fakeListener) Close() error {
	f.closed = true
	return nil
}

func (f *fakeListener) Addr() net.Addr {
	return fakeAddr("fake")
}

type fakeAddr string

func (a fakeAddr) Network() string {
	return string(a)
}

func (a fakeAddr) String() string {
	return string(a)
}
