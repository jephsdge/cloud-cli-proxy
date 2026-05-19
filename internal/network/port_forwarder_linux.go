//go:build linux

package network

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netns"
	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
)

type portForwardSpec struct {
	hostPort   int
	workerPort int
	protocol   string
}

type portForwardKey struct {
	hostPort int
	protocol string
}

type activeForwarder struct {
	spec     portForwardSpec
	workerIP string
	closer   forwardCloser
}

var (
	fwdMu     sync.Mutex
	fwdByHost = map[string]map[portForwardKey]activeForwarder{}
)

type forwardCloser interface {
	Close() error
}

// startPortForwarders binds each host port in the host network namespace and
// proxies incoming connections to the worker's isolated network IP.
//
// The socket keeps the network namespace it was created in, so the accept loop
// can run after the setup goroutine returns to the control-plane namespace.
func startPortForwarders(hostID, workerIP string, ports []agentapi.PortMapping) error {
	specs, err := portForwardSpecs(ports)
	if err != nil {
		return err
	}

	desired := make(map[portForwardKey]portForwardSpec, len(specs))
	for _, spec := range specs {
		desired[spec.key()] = spec
	}

	if len(specs) == 0 {
		stopPortForwarders(hostID)
		return nil
	}

	fwdMu.Lock()
	defer fwdMu.Unlock()

	current := fwdByHost[hostID]
	if current == nil {
		current = make(map[portForwardKey]activeForwarder)
		fwdByHost[hostID] = current
	}

	for key, active := range current {
		want, ok := desired[key]
		if !ok || !active.matches(workerIP, want) {
			_ = active.closer.Close()
			delete(current, key)
		}
	}

	var hostNS netns.NsHandle
	hostNSOpen := false
	defer func() {
		if hostNSOpen {
			_ = hostNS.Close()
		}
	}()

	newForwarders := make(map[portForwardKey]activeForwarder)
	for _, spec := range specs {
		key := spec.key()
		if _, ok := current[key]; ok {
			continue
		}
		if !hostNSOpen {
			hostNS, err = netns.GetFromPath("/proc/1/ns/net")
			if err != nil {
				rollbackNewForwarders(current, newForwarders)
				return fmt.Errorf("open host netns: %w", err)
			}
			hostNSOpen = true
		}

		fwd, err := listenInHostNS(hostNS, spec)
		if err != nil {
			rollbackNewForwarders(current, newForwarders)
			return fmt.Errorf("listen on host %s/%d: %w", spec.protocol, spec.hostPort, err)
		}
		active := activeForwarder{spec: spec, workerIP: workerIP, closer: fwd}
		current[key] = active
		newForwarders[key] = active
		switch spec.protocol {
		case "tcp":
			ln, ok := fwd.(net.Listener)
			if !ok {
				rollbackNewForwarders(current, newForwarders)
				return fmt.Errorf("listen on host tcp/%d returned %T", spec.hostPort, fwd)
			}
			go fwdTCPAcceptLoop(ln, workerIP, spec.workerPort)
		case "udp":
			conn, ok := fwd.(*net.UDPConn)
			if !ok {
				rollbackNewForwarders(current, newForwarders)
				return fmt.Errorf("listen on host udp/%d returned %T", spec.hostPort, fwd)
			}
			go fwdUDPReadLoop(conn, workerIP, spec.workerPort)
		}
	}
	return nil
}

func stopPortForwarders(hostID string) {
	fwdMu.Lock()
	forwarders := fwdByHost[hostID]
	delete(fwdByHost, hostID)
	fwdMu.Unlock()
	closeActiveForwarders(forwarders)
}

func closeActiveForwarders(forwarders map[portForwardKey]activeForwarder) {
	for _, f := range forwarders {
		_ = f.closer.Close()
	}
}

func rollbackNewForwarders(current, created map[portForwardKey]activeForwarder) {
	for key, f := range created {
		_ = f.closer.Close()
		delete(current, key)
	}
}

func (s portForwardSpec) key() portForwardKey {
	return portForwardKey{hostPort: s.hostPort, protocol: s.protocol}
}

func (f activeForwarder) matches(workerIP string, spec portForwardSpec) bool {
	return f.workerIP == workerIP && f.spec.workerPort == spec.workerPort && f.spec.protocol == spec.protocol
}

func portForwardSpecs(ports []agentapi.PortMapping) ([]portForwardSpec, error) {
	specs := make([]portForwardSpec, 0, len(ports))
	seen := make(map[portForwardKey]struct{}, len(ports))
	for _, pm := range ports {
		if pm.HostPort <= 0 || pm.ContainerPort <= 0 {
			continue
		}
		proto := strings.ToLower(pm.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			return nil, fmt.Errorf("linux userland port forwarding only supports tcp/udp, got %s for %d:%d",
				proto, pm.HostPort, pm.ContainerPort)
		}
		spec := portForwardSpec{
			hostPort:   pm.HostPort,
			workerPort: pm.ContainerPort,
			protocol:   proto,
		}
		key := spec.key()
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate host port mapping for %s/%d", proto, pm.HostPort)
		}
		seen[key] = struct{}{}
		specs = append(specs, spec)
	}
	return specs, nil
}

func listenInHostNS(ns netns.NsHandle, spec portForwardSpec) (forwardCloser, error) {
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

	addr := fmt.Sprintf("0.0.0.0:%d", spec.hostPort)
	if spec.protocol == "udp" {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return nil, err
		}
		return net.ListenUDP("udp", udpAddr)
	}
	return net.Listen("tcp", addr)
}

func fwdTCPAcceptLoop(ln net.Listener, workerIP string, workerPort int) {
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

func fwdUDPReadLoop(src *net.UDPConn, workerIP string, workerPort int) {
	target := fmt.Sprintf("%s:%d", workerIP, workerPort)
	buf := make([]byte, 64*1024)
	for {
		n, clientAddr, err := src.ReadFromUDP(buf)
		if err != nil {
			return
		}
		payload := append([]byte(nil), buf[:n]...)
		go fwdUDPDatagram(src, clientAddr, target, payload)
	}
}

func fwdUDPDatagram(src *net.UDPConn, clientAddr *net.UDPAddr, target string, payload []byte) {
	dst, err := net.Dial("udp", target)
	if err != nil {
		return
	}
	defer dst.Close()
	if _, err := dst.Write(payload); err != nil {
		return
	}
	_ = dst.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply := make([]byte, 64*1024)
	n, err := dst.Read(reply)
	if err != nil {
		return
	}
	_, _ = src.WriteToUDP(reply[:n], clientAddr)
}
