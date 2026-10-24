package profile

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/maisem/tailmix/tunmux"
	"github.com/maisem/tailmix/usque/api"
	"github.com/maisem/tailmix/warpcfg"
)

func testWarpConfig(t *testing.T) (warpcfg.Config, warpcfg.Secrets) {
	t.Helper()
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	endpointKey := testWarpKey(t)
	pubDER, err := x509.MarshalPKIXPublicKey(&endpointKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := warpcfg.Config{
		EndpointV4:        "192.0.2.10:4443",
		EndpointH2V4:      "192.0.2.10:443",
		EndpointPublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		IPv4:              netip.MustParseAddr("172.16.0.2"),
		IPv6:              netip.MustParseAddr("2606:4700:110:8a1e::2"),
		DNS:               netip.MustParseAddr("1.1.1.1"),
	}
	secrets := warpcfg.Secrets{
		PrivateKey:     base64.StdEncoding.EncodeToString(keyDER),
		RegistrationID: "registration-123",
	}
	return cfg, secrets
}

func testWarpKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testWarpCert(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject:      pkix.Name{CommonName: "warp-test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func testWarpEngine(t *testing.T) *WarpEngine {
	t.Helper()
	cfg, secrets := testWarpConfig(t)
	return NewWarpEngine(WarpEngineConfig{
		ProfileID: "p_warp",
		Alias:     "warp",
		Config:    cfg,
		Secrets:   secrets,
		Tun:       tunmux.NewChanTUN("warp-test"),
	})
}

func mustWarpStatus(t *testing.T, engine *WarpEngine) Status {
	t.Helper()
	status, err := engine.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestWarpEngineReportsTunnelAddresses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		engine.maintain = func(ctx context.Context, _ api.MaintainTunnelConfig) error {
			<-ctx.Done()
			return ctx.Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := engine.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		synctest.Wait()

		status := mustWarpStatus(t, engine)
		if status.Kind != "warp" {
			t.Fatalf("kind = %q, want warp", status.Kind)
		}
		wantIPs := []netip.Addr{engine.cfg.Config.IPv4, engine.cfg.Config.IPv6}
		if !slices.Equal(status.SelfIPs, wantIPs) {
			t.Fatalf("self addresses = %v, want %v", status.SelfIPs, wantIPs)
		}
		if status.ExitNodeID != engine.cfg.Secrets.RegistrationID {
			t.Fatalf("exit node = %q, want registration ID %q", status.ExitNodeID, engine.cfg.Secrets.RegistrationID)
		}
		if !status.RouteAll {
			t.Fatal("warp profile does not accept all routes")
		}
		wantRoutes := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
		if len(status.AvailableRoutes) != len(wantRoutes) {
			t.Fatalf("routes = %v, want %v", status.AvailableRoutes, wantRoutes)
		}
		for i, route := range status.AvailableRoutes {
			if route.Prefix != wantRoutes[i] {
				t.Fatalf("routes = %v, want %v", status.AvailableRoutes, wantRoutes)
			}
		}
		if len(status.Peers) != 0 {
			t.Fatalf("peers = %v, want none", status.Peers)
		}
	})
}

func TestWarpEngineReportsConnectionTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		steps := make(chan api.TunnelState, 1)
		var starts atomic.Int32
		engine.maintain = func(ctx context.Context, cfg api.MaintainTunnelConfig) error {
			starts.Add(1)
			for {
				select {
				case state := <-steps:
					cfg.OnStateChange(state)
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := engine.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer engine.Close()

		steps <- api.TunnelConnected
		synctest.Wait()
		connected := mustWarpStatus(t, engine)
		if connected.BackendState != "Running" {
			t.Fatalf("backend state = %q, want Running", connected.BackendState)
		}
		if connected.LastConnected.IsZero() {
			t.Fatal("running status has no connection time")
		}
		firstConnection := connected.LastConnected

		time.Sleep(time.Minute)
		steps <- api.TunnelDisconnected
		synctest.Wait()
		dropped := mustWarpStatus(t, engine)
		if dropped.BackendState != "Unavailable" {
			t.Fatalf("backend state = %q, want Unavailable", dropped.BackendState)
		}
		if len(dropped.SelfIPs) != 2 {
			t.Fatalf("self addresses went empty while the tunnel was down: %v", dropped.SelfIPs)
		}

		time.Sleep(time.Minute)
		steps <- api.TunnelConnected
		synctest.Wait()
		reconnected := mustWarpStatus(t, engine)
		if reconnected.BackendState != "Running" {
			t.Fatalf("backend state = %q, want Running", reconnected.BackendState)
		}
		if !reconnected.LastConnected.After(firstConnection) {
			t.Fatalf("connection time = %v, want after %v", reconnected.LastConnected, firstConnection)
		}
		if got := starts.Load(); got != 1 {
			t.Fatalf("tunnel loop started %d times, want 1", got)
		}
	})
}

func TestWarpEngineRejectsDifferentEndpointKey(t *testing.T) {
	engine := testWarpEngine(t)
	configs := make(chan api.MaintainTunnelConfig, 1)
	engine.maintain = func(ctx context.Context, cfg api.MaintainTunnelConfig) error {
		configs <- cfg
		cfg.OnStateChange(api.TunnelDisconnected)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	tunnelCfg := <-configs

	wrongKey := testWarpKey(t)
	serverConn, clientConn := net.Pipe()
	server := tls.Server(serverConn, &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{testWarpCert(t, wrongKey)},
			PrivateKey:  wrongKey,
		}},
	})
	go func() { _ = server.Handshake(); server.Close() }()
	client := tls.Client(clientConn, tunnelCfg.TLSConfig)
	err := client.Handshake()
	client.Close()
	if err == nil {
		t.Fatal("handshake with a different endpoint key succeeded")
	}
	if !strings.Contains(err.Error(), "different public key") {
		t.Fatalf("handshake error = %v, want a public key mismatch", err)
	}
	if status := mustWarpStatus(t, engine); status.BackendState != "Unavailable" {
		t.Fatalf("backend state = %q, want Unavailable", status.BackendState)
	}
}

func TestWarpEngineCloseStopsTunnel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		returned := make(chan struct{})
		engine.maintain = func(ctx context.Context, _ api.MaintainTunnelConfig) error {
			<-ctx.Done()
			close(returned)
			return ctx.Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := engine.Start(ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		cancel()
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Fatal("tunnel loop survived a cancelled context")
		}
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		if err := engine.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})
}

func TestWarpEngineWatchUpdates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := make(chan struct{}, 8)
		done := make(chan error, 1)
		go func() {
			done <- engine.WatchUpdates(ctx, func() {
				select {
				case calls <- struct{}{}:
				default:
				}
			})
		}()
		synctest.Wait()

		engine.onStateChange(api.TunnelConnected)
		synctest.Wait()
		select {
		case <-calls:
		default:
			t.Fatal("connection state change did not notify watchers")
		}

		cancel()
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatalf("WatchUpdates: %v", err)
		}
	})
}
