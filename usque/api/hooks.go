package api

// TunnelState is the connection state a tunnel reports to its caller. Every
// connect attempt reports one of these transitions (R15).
type TunnelState int

const (
	// TunnelConnecting means a connect attempt is in progress.
	TunnelConnecting TunnelState = iota
	// TunnelConnected means the tunnel carries packets.
	TunnelConnected
	// TunnelDisconnected means no tunnel is up. Another attempt may follow.
	TunnelDisconnected
)
