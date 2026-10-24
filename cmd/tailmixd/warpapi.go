package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/maisem/tailmix/controlapi"
	"github.com/maisem/tailmix/state"
	"github.com/maisem/tailmix/usque/api"
	"github.com/maisem/tailmix/usque/models"
	"github.com/maisem/tailmix/warpcfg"
)

// warpEnroll is the registration seam. It is a variable so tests can replace
// the Cloudflare calls.
var warpEnroll = enrollWarpDevice

// ApplyWarp registers or imports a WARP profile, writes its owner-only
// credentials, saves the desired state, and starts the profile. The platform
// check runs before any file or state write.
func (s *supervisor) ApplyWarp(_ context.Context, request controlapi.ApplyWarpRequest) (controlapi.WarpProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireDaemonUpLocked(); err != nil {
		return controlapi.WarpProfile{}, err
	}
	if !warpPlatformSupported {
		return controlapi.WarpProfile{}, controlapi.NewError("warp_unsupported_platform", "%v", errWarpPlatform)
	}
	if s.cfg.Mode != "tun" {
		return controlapi.WarpProfile{}, controlapi.NewError("unsupported_mode", "WARP profiles require TUN mode")
	}
	name := strings.TrimSpace(request.Name)
	if !validProfileName(name) {
		return controlapi.WarpProfile{}, controlapi.NewError("invalid_request", "invalid profile name %q", request.Name)
	}

	var config warpcfg.Config
	var secrets warpcfg.Secrets
	switch {
	case request.Register != nil && request.Config != nil:
		return controlapi.WarpProfile{}, controlapi.NewError("invalid_request", "apply either a registration or an imported configuration, not both")
	case request.Register != nil:
		registered, credentials, err := warpEnroll(name, request.Register.JWT)
		if err != nil {
			return controlapi.WarpProfile{}, controlapi.NewError("warp_credentials", "WARP registration failed: %v", err)
		}
		config, secrets = registered, credentials
	case request.Config != nil && request.Secrets != nil:
		imported, err := normalizeWarpImport(*request.Config, *request.Secrets)
		if err != nil {
			return controlapi.WarpProfile{}, controlapi.NewError("invalid_request", "invalid WARP configuration: %v", err)
		}
		config, secrets = imported, *request.Secrets
	default:
		return controlapi.WarpProfile{}, controlapi.NewError("invalid_request", "apply requires a registration or an imported configuration")
	}

	if raw := strings.TrimSpace(request.DNS); raw != "" {
		resolver, err := netip.ParseAddr(raw)
		if err != nil {
			return controlapi.WarpProfile{}, controlapi.NewError("invalid_request", "invalid DNS address")
		}
		config.DNS = resolver
	} else if !config.DNS.IsValid() {
		config.DNS = netip.MustParseAddr("1.1.1.1")
	}
	config.UseHTTP2 = request.UseHTTP2
	if config.UseHTTP2 && config.EndpointH2V4 == "" && config.EndpointH2V6 == "" {
		return controlapi.WarpProfile{}, controlapi.NewError("invalid_request", "HTTP/2 transport requires a configuration-provided HTTP/2 endpoint")
	}
	generation, err := randomOpaque("g_", 8)
	if err != nil {
		return controlapi.WarpProfile{}, err
	}
	config.CredentialsGeneration = generation

	before := cloneState(s.st)
	next := cloneState(s.st)
	configured, findErr := profileByName(&next, name)
	create := findErr != nil
	if create {
		id, idErr := randomOpaque("p_", 8)
		if idErr != nil {
			return controlapi.WarpProfile{}, idErr
		}
		stateDir, pathErr := s.validateNewStateDirLocked(defaultProfileStateDir(s.store.Path(), id))
		if pathErr != nil {
			return controlapi.WarpProfile{}, pathErr
		}
		next.Profiles = append(next.Profiles, state.Profile{
			ID:       id,
			Name:     name,
			Kind:     state.ProfileKindWarp,
			StateDir: stateDir,
		})
		configured, _ = profileByName(&next, name)
	} else if configured.Kind != state.ProfileKindWarp {
		return controlapi.WarpProfile{}, controlapi.NewError("profile_exists", "profile %q is not a WARP profile", name)
	}
	profileID := configured.ID

	// The credentials file is replaced only after registration or import
	// succeeded, so a failed attempt leaves the previous file intact.
	if err := warpcfg.SaveSecrets(configured.StateDir, secrets); err != nil {
		return controlapi.WarpProfile{}, fmt.Errorf("write WARP credentials: %w", err)
	}

	configured.Kind = state.ProfileKindWarp
	configured.Removed = false
	if create || beforeProfileRemoved(before, configured.ID) {
		configured.Disabled = false
	}
	configured.Hostname = ""
	configured.Warp = &config
	configured, err = normalizeProfileByID(&next, profileID)
	if err != nil {
		return controlapi.WarpProfile{}, controlapi.NewError("invalid_state", "%v", err)
	}

	if err := s.store.Save(next); err != nil {
		return controlapi.WarpProfile{}, err
	}
	s.st = next

	if !configured.Disabled && s.runtimes[profileID] == nil {
		if err := s.startProfileLockedWithWireGuardTransition(*configured, "", "", true); err != nil {
			s.lastErrors[profileID] = fmt.Sprintf("WARP apply left the profile fail-closed; runtime may differ from saved desired state: %v; reapply or restart to retry", err)
			return controlapi.WarpProfile{}, controlapi.NewError("runtime_start_failed", "start WARP profile: %v", err)
		}
	}
	if !configured.Disabled {
		if err := s.reconcileLocked(); err != nil {
			s.st = next
			s.lastErrors[profileID] = fmt.Sprintf("WARP apply left the profile fail-closed; runtime may differ from saved desired state: %v; reapply or restart to retry", err)
			return controlapi.WarpProfile{}, controlapi.NewError("reconcile_failed", "%v", err)
		}
	}
	delete(s.lastErrors, profileID)
	return warpProfileView(*configured)
}

func warpProfileView(configured state.Profile) (controlapi.WarpProfile, error) {
	if configured.Warp == nil {
		return controlapi.WarpProfile{}, controlapi.NewError("invalid_state", "WARP profile has no configuration")
	}
	config := *configured.Warp
	result := controlapi.WarpProfile{
		Name:      profileName(configured),
		Kind:      state.ProfileKindWarp,
		Transport: "quic",
		Resolver:  config.DNS.String(),
	}
	if config.UseHTTP2 {
		result.Transport = "http2"
	}
	for _, addr := range []netip.Addr{config.IPv4, config.IPv6} {
		if addr.IsValid() {
			result.Addresses = append(result.Addresses, addr.String())
		}
	}
	return result, nil
}

func enrollWarpDevice(deviceName, teamToken string) (warpcfg.Config, warpcfg.Secrets, error) {
	account, err := api.Register("PC", "en_US", teamToken, true)
	if err != nil {
		return warpcfg.Config{}, warpcfg.Secrets{}, err
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return warpcfg.Config{}, warpcfg.Secrets{}, err
	}
	privateDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return warpcfg.Config{}, warpcfg.Secrets{}, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return warpcfg.Config{}, warpcfg.Secrets{}, err
	}
	enrolled, err := api.EnrollKey(account.ID, account.Token, publicDER, deviceName)
	if err != nil {
		return warpcfg.Config{}, warpcfg.Secrets{}, err
	}
	config, err := warpConfigFromRegistration(enrolled)
	if err != nil {
		return warpcfg.Config{}, warpcfg.Secrets{}, err
	}
	return config, warpcfg.Secrets{
		PrivateKey:     base64.StdEncoding.EncodeToString(privateDER),
		RegistrationID: enrolled.ID,
		AccessToken:    account.Token,
		License:        enrolled.Account.License,
	}, nil
}

func warpConfigFromRegistration(account *models.AccountData) (warpcfg.Config, error) {
	if account == nil || len(account.Config.Peers) == 0 {
		return warpcfg.Config{}, errors.New("registration response has no peer")
	}
	peer := account.Config.Peers[0]
	endpointV4, err := warpEndpointAddr(peer.Endpoint.V4)
	if err != nil {
		return warpcfg.Config{}, fmt.Errorf("registration response has no IPv4 endpoint: %w", err)
	}
	endpointV6, err := warpEndpointAddr(peer.Endpoint.V6)
	if err != nil {
		return warpcfg.Config{}, fmt.Errorf("registration response has no IPv6 endpoint: %w", err)
	}
	ipv4, err := netip.ParseAddr(account.Config.Interface.Addresses.V4)
	if err != nil {
		return warpcfg.Config{}, errors.New("registration response has no IPv4 tunnel address")
	}
	ipv6, err := netip.ParseAddr(account.Config.Interface.Addresses.V6)
	if err != nil {
		return warpcfg.Config{}, errors.New("registration response has no IPv6 tunnel address")
	}
	endpointH2V4, err := warpEndpointAddr("162.159.198.2")
	if err != nil {
		return warpcfg.Config{}, err
	}
	return warpcfg.Config{
		EndpointV4:        endpointV4,
		EndpointV6:        endpointV6,
		EndpointH2V4:      endpointH2V4,
		EndpointPublicKey: peer.PublicKey,
		IPv4:              ipv4,
		IPv6:              ipv6,
		DNS:               netip.MustParseAddr("1.1.1.1"),
	}, nil
}

func normalizeWarpImport(requested warpcfg.Config, supplied warpcfg.Secrets) (warpcfg.Config, error) {
	endpointV4, err := warpEndpointAddr(requested.EndpointV4)
	if err != nil {
		return warpcfg.Config{}, fmt.Errorf("endpoint_v4: %w", err)
	}
	endpointV6, err := warpEndpointAddr(requested.EndpointV6)
	if err != nil {
		return warpcfg.Config{}, fmt.Errorf("endpoint_v6: %w", err)
	}
	endpointH2V4, endpointH2V6 := "", ""
	if strings.TrimSpace(requested.EndpointH2V4) != "" {
		if endpointH2V4, err = warpEndpointAddr(requested.EndpointH2V4); err != nil {
			return warpcfg.Config{}, fmt.Errorf("endpoint_h2_v4: %w", err)
		}
	}
	if strings.TrimSpace(requested.EndpointH2V6) != "" {
		if endpointH2V6, err = warpEndpointAddr(requested.EndpointH2V6); err != nil {
			return warpcfg.Config{}, fmt.Errorf("endpoint_h2_v6: %w", err)
		}
	}
	if strings.TrimSpace(requested.EndpointPublicKey) == "" {
		return warpcfg.Config{}, errors.New("endpoint_pub_key is missing")
	}
	if !requested.IPv4.IsValid() {
		return warpcfg.Config{}, errors.New("ipv4 tunnel address is missing")
	}
	if !requested.IPv6.IsValid() {
		return warpcfg.Config{}, errors.New("ipv6 tunnel address is missing")
	}
	if strings.TrimSpace(supplied.PrivateKey) == "" {
		return warpcfg.Config{}, errors.New("private_key is missing")
	}
	if strings.TrimSpace(supplied.RegistrationID) == "" {
		return warpcfg.Config{}, errors.New("id is missing")
	}
	return warpcfg.Config{
		EndpointV4:        endpointV4,
		EndpointV6:        endpointV6,
		EndpointH2V4:      endpointH2V4,
		EndpointH2V6:      endpointH2V6,
		EndpointPublicKey: requested.EndpointPublicKey,
		IPv4:              requested.IPv4,
		IPv6:              requested.IPv6,
		DNS:               requested.DNS,
		UseHTTP2:          requested.UseHTTP2,
	}, nil
}

// warpEndpointAddr accepts a host-only endpoint or an addr:port endpoint and
// returns it with a port. The stored form always carries a port.
func warpEndpointAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("endpoint is empty")
	}
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		if addrPort.Port() == 0 {
			addrPort = netip.AddrPortFrom(addrPort.Addr(), 443)
		}
		return addrPort.String(), nil
	}
	host := strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "", errors.New("endpoint is not an IP address")
	}
	return netip.AddrPortFrom(addr, 443).String(), nil
}
