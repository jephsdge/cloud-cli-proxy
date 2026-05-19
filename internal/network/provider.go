package network

import "context"

// Provider defines the contract for setting up and tearing down per-host
// network isolation. Implementations must be safe for concurrent use.
type Provider interface {
	PrepareHost(context.Context, HostNetworkSpec) error
	// RefreshHost restores volatile host-side networking state for an already
	// running host without recreating containers or Docker networks.
	RefreshHost(context.Context, HostNetworkSpec) error
	CleanupHost(context.Context, HostNetworkSpec) error
}
