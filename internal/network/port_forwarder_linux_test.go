//go:build linux

package network

import (
	"net"
	"testing"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
)

func TestPortForwardSpecsNormalizesProtocols(t *testing.T) {
	specs, err := portForwardSpecs([]agentapi.PortMapping{
		{HostPort: 8080, ContainerPort: 80},
		{HostPort: 8443, ContainerPort: 443, Protocol: "TCP"},
		{HostPort: 5353, ContainerPort: 5353, Protocol: "UDP"},
		{HostPort: 0, ContainerPort: 9000},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("unexpected spec count: got %d want 3", len(specs))
	}
	if specs[0].hostPort != 8080 || specs[0].workerPort != 80 || specs[0].protocol != "tcp" {
		t.Fatalf("unexpected first spec: %#v", specs[0])
	}
	if specs[1].hostPort != 8443 || specs[1].workerPort != 443 || specs[1].protocol != "tcp" {
		t.Fatalf("unexpected second spec: %#v", specs[1])
	}
	if specs[2].hostPort != 5353 || specs[2].workerPort != 5353 || specs[2].protocol != "udp" {
		t.Fatalf("unexpected third spec: %#v", specs[2])
	}
}

func TestPortForwardSpecsRejectsUnknownProtocol(t *testing.T) {
	_, err := portForwardSpecs([]agentapi.PortMapping{
		{HostPort: 5353, ContainerPort: 5353, Protocol: "sctp"},
	})
	if err == nil {
		t.Fatal("expected unknown protocol to be rejected")
	}
}

func TestPortForwardSpecsRejectsDuplicateHostProtocol(t *testing.T) {
	_, err := portForwardSpecs([]agentapi.PortMapping{
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostPort: 8080, ContainerPort: 8081, Protocol: "TCP"},
	})
	if err == nil {
		t.Fatal("expected duplicate host protocol mapping to be rejected")
	}
}

func TestStartPortForwardersWithNoPortsStopsExistingListeners(t *testing.T) {
	hostID := "host-empty-ports"
	ln := &fakeListener{}
	fwdMu.Lock()
	fwdByHost[hostID] = map[portForwardKey]activeForwarder{
		{hostPort: 8080, protocol: "tcp"}: {
			spec:     portForwardSpec{hostPort: 8080, workerPort: 80, protocol: "tcp"},
			workerIP: "10.99.1.3",
			closer:   ln,
		},
	}
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

func TestStartPortForwardersKeepsUnchangedListeners(t *testing.T) {
	hostID := "host-keep-listener"
	ln := &fakeListener{}
	fwdMu.Lock()
	fwdByHost[hostID] = map[portForwardKey]activeForwarder{
		{hostPort: 8080, protocol: "tcp"}: {
			spec:     portForwardSpec{hostPort: 8080, workerPort: 80, protocol: "tcp"},
			workerIP: "10.99.1.3",
			closer:   ln,
		},
	}
	fwdMu.Unlock()

	if err := startPortForwarders(hostID, "10.99.1.3", []agentapi.PortMapping{
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fwdMu.Lock()
	_, exists := fwdByHost[hostID][portForwardKey{hostPort: 8080, protocol: "tcp"}]
	fwdMu.Unlock()
	if !exists {
		t.Fatal("expected listener registry to retain unchanged listener")
	}
	if ln.closed {
		t.Fatal("expected unchanged listener to stay open")
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
