package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maisem/tailmix/controlapi"
	"github.com/maisem/tailmix/state"
	"github.com/maisem/tailmix/tunmux"
	"github.com/maisem/tailmix/warpcfg"
)

func stubWarpEnroll(t *testing.T, fn func(deviceName, teamToken string) (warpcfg.Config, warpcfg.Secrets, error)) {
	t.Helper()
	previous := warpEnroll
	warpEnroll = fn
	t.Cleanup(func() { warpEnroll = previous })
}

func warpApplyTestState() state.State {
	return state.State{
		SyntheticPool:   "100.127.0.0/24",
		SyntheticPoolV6: "fd6d:6e65:7400::/56",
	}
}

func warpApplyExistingProfile(t *testing.T, dir string, disabled bool) state.State {
	t.Helper()
	config, _ := warpApplyTestConfig(t)
	config.CredentialsGeneration = "g_previous"
	st := warpApplyTestState()
	st.Profiles = []state.Profile{{
		ID: "p_warp", Name: "cloudflare", Kind: state.ProfileKindWarp,
		StateDir: filepath.Join(dir, "profiles", "p_warp"), Warp: &config, Disabled: disabled,
	}}
	return st
}

func newWarpApplySupervisor(t *testing.T, disabled bool) (*supervisor, state.State) {
	t.Helper()
	s := newLifecycleTestSupervisor(t, warpApplyTestState())
	st := warpApplyExistingProfile(t, s.cfg.SocketDir, disabled)
	s.st = st
	if err := s.store.Save(st); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func warpApplyTestConfig(t *testing.T) (warpcfg.Config, warpcfg.Secrets) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	config := warpcfg.Config{
		EndpointV4:        "192.0.2.1:443",
		EndpointV6:        "[2001:db8::1]:443",
		EndpointH2V4:      "192.0.2.2:443",
		EndpointPublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})),
		IPv4:              netip.MustParseAddr("172.16.0.2"),
		IPv6:              netip.MustParseAddr("2606:4700:110:8a37::1"),
		DNS:               netip.MustParseAddr("1.1.1.1"),
	}
	secrets := warpcfg.Secrets{
		PrivateKey:     base64.StdEncoding.EncodeToString(privateDER),
		RegistrationID: "device-1",
		AccessToken:    "access-token-1",
		License:        "license-1",
	}
	return config, secrets
}

func TestWarpApplyRejectedRegistrationLeavesNoProfile(t *testing.T) {
	s := newLifecycleTestSupervisor(t, warpApplyTestState())
	s.cfg.Mode = "tun"
	stubWarpEnroll(t, func(string, string) (warpcfg.Config, warpcfg.Secrets, error) {
		return warpcfg.Config{}, warpcfg.Secrets{}, errors.New("Cloudflare rejected the registration")
	})

	_, err := s.ApplyWarp(context.Background(), controlapi.ApplyWarpRequest{
		Name: "cloudflare", Register: &controlapi.WarpRegisterRequest{},
	})
	var apiErr *controlapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "warp_credentials" {
		t.Fatalf("ApplyWarp error = %v, want warp_credentials", err)
	}
	persisted, loadErr := s.store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(persisted.Profiles) != 0 {
		t.Fatalf("rejected registration stored profiles: %+v", persisted.Profiles)
	}
	if _, statErr := os.Stat(filepath.Join(s.cfg.SocketDir, "profiles")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected registration created a profile directory: %v", statErr)
	}
}

func TestWarpApplyRefusesUnsupportedPlatformBeforeWriting(t *testing.T) {
	previous := warpPlatformSupported
	warpPlatformSupported = false
	t.Cleanup(func() { warpPlatformSupported = previous })

	for _, test := range []struct {
		name    string
		request controlapi.ApplyWarpRequest
	}{
		{name: "register", request: controlapi.ApplyWarpRequest{Name: "cloudflare", Register: &controlapi.WarpRegisterRequest{}}},
		{name: "import", request: controlapi.ApplyWarpRequest{Name: "cloudflare", Config: &warpcfg.Config{}, Secrets: &warpcfg.Secrets{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newLifecycleTestSupervisor(t, warpApplyTestState())
			s.cfg.Mode = "socks"
			stubWarpEnroll(t, func(string, string) (warpcfg.Config, warpcfg.Secrets, error) {
				t.Fatal("unsupported platform reached Cloudflare")
				return warpcfg.Config{}, warpcfg.Secrets{}, nil
			})

			_, err := s.ApplyWarp(context.Background(), test.request)
			if err == nil || !strings.Contains(err.Error(), errWarpPlatform.Error()) {
				t.Fatalf("ApplyWarp error = %v, want %v", err, errWarpPlatform)
			}
			persisted, loadErr := s.store.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if len(persisted.Profiles) != 0 {
				t.Fatalf("unsupported platform stored profiles: %+v", persisted.Profiles)
			}
		})
	}
}

func TestWarpApplyRegistersNewProfileAndStartsIt(t *testing.T) {
	config, secrets := warpApplyTestConfig(t)
	s := newLifecycleTestSupervisor(t, warpApplyTestState())
	s.cfg.Mode = "tun"
	s.dnsService = &lifecycleDNSService{}
	hostDevice := tunmux.NewChanTUN("warp-apply-host")
	s.host = &wireGuardApplyFailHost{device: hostDevice}
	s.mux = tunmux.NewMux(hostDevice, nil, nil, nil)
	stubWarpEnroll(t, func(deviceName, teamToken string) (warpcfg.Config, warpcfg.Secrets, error) {
		return config, secrets, nil
	})

	got, err := s.ApplyWarp(context.Background(), controlapi.ApplyWarpRequest{
		Name: "cloudflare", Register: &controlapi.WarpRegisterRequest{JWT: "team-token"},
		DNS: "9.9.9.9", UseHTTP2: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "cloudflare" || got.Kind != state.ProfileKindWarp || got.Transport != "http2" {
		t.Fatalf("profile view = %+v", got)
	}
	persisted, loadErr := s.store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(persisted.Profiles) != 1 {
		t.Fatalf("profiles = %+v", persisted.Profiles)
	}
	stored := persisted.Profiles[0]
	if stored.Warp == nil || stored.Warp.DNS != netip.MustParseAddr("9.9.9.9") || !stored.Warp.UseHTTP2 {
		t.Fatalf("stored WARP config = %+v", stored.Warp)
	}
	if stored.Warp.CredentialsGeneration == "" {
		t.Fatal("stored WARP config has no credential generation")
	}
	info, statErr := os.Stat(warpcfg.SecretPath(stored.StateDir))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode = %v, want 0600", info.Mode().Perm())
	}
	loaded, loadErr := warpcfg.LoadSecrets(stored.StateDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.RegistrationID != "device-1" {
		t.Fatalf("stored credentials = %+v", loaded)
	}
}

func TestWarpApplyImportStoresBothAddressesWithoutRegistration(t *testing.T) {
	config := warpcfg.Config{
		EndpointV4:        "192.0.2.1",
		EndpointV6:        "2001:db8::1",
		EndpointH2V4:      "192.0.2.2",
		EndpointPublicKey: "endpoint-key",
		IPv4:              netip.MustParseAddr("172.16.0.2"),
		IPv6:              netip.MustParseAddr("2606:4700:110:8a37::1"),
	}
	secrets := warpcfg.Secrets{PrivateKey: "private-key", RegistrationID: "device-2", AccessToken: "access-token-2"}

	s, _ := newWarpApplySupervisor(t, true)
	s.cfg.Mode = "tun"
	stubWarpEnroll(t, func(string, string) (warpcfg.Config, warpcfg.Secrets, error) {
		t.Fatal("import contacted Cloudflare")
		return warpcfg.Config{}, warpcfg.Secrets{}, nil
	})

	got, err := s.ApplyWarp(context.Background(), controlapi.ApplyWarpRequest{
		Name: "cloudflare", Config: &config, Secrets: &secrets, UseHTTP2: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Resolver != "1.1.1.1" {
		t.Fatalf("resolver = %q, want default 1.1.1.1", got.Resolver)
	}
	persisted, loadErr := s.store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(persisted.Profiles) != 1 {
		t.Fatalf("profiles = %+v", persisted.Profiles)
	}
	stored := persisted.Profiles[0]
	if stored.Warp == nil {
		t.Fatal("import stored no WARP config")
	}
	if stored.Warp.EndpointV4 != "192.0.2.1:443" || stored.Warp.EndpointV6 != "[2001:db8::1]:443" || stored.Warp.EndpointH2V4 != "192.0.2.2:443" {
		t.Fatalf("imported endpoints = %+v", stored.Warp)
	}
	if stored.Warp.IPv4 != config.IPv4 || stored.Warp.IPv6 != config.IPv6 {
		t.Fatalf("imported tunnel addresses = %+v", stored.Warp)
	}
	if stored.Warp.CredentialsGeneration == "g_previous" {
		t.Fatal("import did not replace the credential generation")
	}
}

func TestWarpApplyInterruptedReRegistrationKeepsPreviousCredentials(t *testing.T) {
	s, st := newWarpApplySupervisor(t, true)
	s.cfg.Mode = "tun"
	previousSecrets := warpcfg.Secrets{
		PrivateKey: "previous-private-key", RegistrationID: "previous-device",
		AccessToken: "previous-access-token",
	}
	if err := warpcfg.SaveSecrets(st.Profiles[0].StateDir, previousSecrets); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(warpcfg.SecretPath(st.Profiles[0].StateDir))
	if err != nil {
		t.Fatal(err)
	}

	stubWarpEnroll(t, func(string, string) (warpcfg.Config, warpcfg.Secrets, error) {
		return warpcfg.Config{}, warpcfg.Secrets{}, errors.New("network unreachable")
	})

	_, err = s.ApplyWarp(context.Background(), controlapi.ApplyWarpRequest{
		Name: "cloudflare", Register: &controlapi.WarpRegisterRequest{},
	})
	var apiErr *controlapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "warp_credentials" {
		t.Fatalf("ApplyWarp error = %v, want warp_credentials", err)
	}
	after, readErr := os.ReadFile(warpcfg.SecretPath(st.Profiles[0].StateDir))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Fatal("failed re-registration replaced the previous credential file")
	}
	persisted, loadErr := s.store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if persisted.Profiles[0].Warp.CredentialsGeneration != "g_previous" {
		t.Fatal("failed re-registration replaced the previous state entry")
	}
}

func TestWarpApplyPlainRemovalKeepsCredentialsAndPurgeDeletesThem(t *testing.T) {
	_, secrets := warpApplyTestConfig(t)
	s, st := newWarpApplySupervisor(t, true)
	s.cfg.Mode = "socks"
	if err := warpcfg.SaveSecrets(st.Profiles[0].StateDir, secrets); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RemoveProfile(context.Background(), "cloudflare", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(warpcfg.SecretPath(st.Profiles[0].StateDir)); err != nil {
		t.Fatalf("plain removal deleted credentials: %v", err)
	}
	if _, err := s.RemoveProfile(context.Background(), "cloudflare", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(warpcfg.SecretPath(st.Profiles[0].StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purge kept credentials: %v", err)
	}
}
