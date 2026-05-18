//go:build linux

package network

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"

	"github.com/vishvananda/netns"
	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
)

type portForwardSpec struct {
	hostPort   int
	workerPort int
}

var (
	fwdMu     sync.Mutex
	fwdByHost = map[string][]net.Listener{}
)

// startPortForwarders binds each host port in the host network namespace and
// proxies incoming TCP connections to the worker's isolated network IP.
//
// The socket keeps the network namespace it was created in, so the accept loop
// can run after the setup goroutine returns to the control-plane namespace.
func startPortForwarders(hostID, workerIP string, ports []agentapi.PortMapping) error {
	specs, err := tcpPortForwardSpecs(ports)
	if err != nil {
		return err
	}

	if len(specs) == 0 {
		stopPortForwarders(hostID)
		return nil
	}

	hostNS, err := netns.GetFromPath("/proc/1/ns/net")
	if err != nil {
		return fmt.Errorf("open host netns: %w", err)
	}
	defer hostNS.Close()

	stopPortForwarders(hostID)

	var listeners []net.Listener
	for _, spec := range specs {
		ln, err := listenInHostNS(hostNS, spec.hostPort)
		if err != nil {
			closeListeners(listeners)
			return fmt.Errorf("listen on host :%d: %w", spec.hostPort, err)
		}
		listeners = append(listeners, ln)
		go fwdAcceptLoop(ln, workerIP, spec.workerPort)
	}

	fwdMu.Lock()
	fwdByHost[hostID] = listeners
	fwdMu.Unlock()
	return nil
}

func stopPortForwarders(hostID string) {
	fwdMu.Lock()
	listeners := fwdByHost[hostID]
	delete(fwdByHost, hostID)
	fwdMu.Unlock()
	closeListeners(listeners)
}

func closeListeners(listeners []net.Listener) {
	for _, l := range listeners {
		_ = l.Close()
	}
}

func tcpPortForwardSpecs(ports []agentapi.PortMapping) ([]portForwardSpec, error) {
	specs := make([]portForwardSpec, 0, len(ports))
	for _, pm := range ports {
		if pm.HostPort <= 0 || pm.ContainerPort <= 0 {
			continue
		}
		proto := strings.ToLower(pm.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" {
			return nil, fmt.Errorf("linux userland port forwarding only supports tcp, got %s for %d:%d",
				proto, pm.HostPort, pm.ContainerPort)
		}
		specs = append(specs, portForwardSpec{
			hostPort:   pm.HostPort,
			workerPort: pm.ContainerPort,
		})
	}
	return specs, nil
}

func listenInHostNS(ns netns.NsHandle, port int) (net.Listener, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cur, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("get current netns: %w", err)
	}
	defer cur.Close()
	if err := netns.Set(ns); err != nil {
		return nil, fmt.Errorf("set host netns: %w", err)
	}
	defer func() { _ = netns.Set(cur) }()

	return net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
}

func fwdAcceptLoop(ln net.Listener, workerIP string, workerPort int) {
	target := fmt.Sprintf("%s:%d", workerIP, workerPort)
	for {
		src, err := ln.Accept()
		if err != nil {
			return
		}
		go fwdProxyConn(src, target)
	}
}

func fwdProxyConn(src net.Conn, target string) {
	defer src.Close()

	dst, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer dst.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(src, dst)
		done <- struct{}{}
	}()
	<-done
}
