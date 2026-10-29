//go:build linux

package dns

import tailscaledns "tailscale.com/net/dns"

const platformGOOS = "linux"

// systemd-resolved needs every split-DNS suffix as a link domain, which
// tailscale only installs when the default route is a route rather than
// DefaultResolvers.
const defaultRouteIsMatchDomain = true

func platformOSConfigurator(configurator tailscaledns.OSConfigurator) tailscaledns.OSConfigurator {
	return configurator
}
