package profile

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/maisem/tailmix/netns"
	"github.com/maisem/tailmix/usque/api"
	"github.com/maisem/tailmix/warpcfg"
	"github.com/tailscale/wireguard-go/tun"
	"tailscale.com/net/netmon"
	"tailscale.com/types/logger"
)

const (
	warpMTU             = 1280
	warpKeepalivePeriod = 30 * time.Second
	warpEndpointSNI     = "consumer-masque.cloudflareclient.com"
)

// WarpEngineConfig describes a WARP profile engine.
type WarpEngineConfig struct {
	ProfileID string
	Alias     string
	Config    warpcfg.Config
	Secrets   warpcfg.Secrets
	Tun       tun.Device
	Logf      logger.Logf
}

// WarpEngine runs a MASQUE tunnel for a WARP profile. The tunnel loop owns
// reconnection; the engine only projects its connection state.
type WarpEngine struct {
	cfg WarpEngineConfig

	// maintain is the tunnel loop, replaced in tests.
	maintain func(context.Context, api.MaintainTunnelConfig) error

	mu            sync.Mutex
	started       bool
	closed        bool
	connected     bool
	lastConnected time.Time
	cancel        context.CancelFunc
	done          chan struct{}
	netMon        *netmon.Monitor
	updateCh      chan struct{}
}

func NewWarpEngine(cfg WarpEngineConfig) *WarpEngine {
	if cfg.Logf == nil {
		cfg.Logf = logger.Discard
	}
	return &WarpEngine{
		cfg:      cfg,
		maintain: api.MaintainTunnel,
		updateCh: make(chan struct{}),
	}
}

func (e *WarpEngine) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("warp engine is closed")
	}
	if e.started {
		return errors.New("warp engine is already started")
	}
	if e.cfg.Tun == nil {
		return errors.New("warp engine requires a TUN device")
	}
	netMon := netmon.NewStatic()
	tunnelCfg, err := e.tunnelConfig(netMon)
	if err != nil {
		netMon.Close()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.done = make(chan struct{})
	e.netMon = netMon
	e.started = true
	e.connected = false
	e.notifyLocked()
	go e.run(runCtx, tunnelCfg)
	return nil
}

func (e *WarpEngine) run(ctx context.Context, cfg api.MaintainTunnelConfig) {
	defer close(e.done)
	_ = e.maintain(ctx, cfg)
}

func (e *WarpEngine) tunnelConfig(netMon *netmon.Monitor) (api.MaintainTunnelConfig, error) {
	privateKey, err := warpPrivateKey(e.cfg.Secrets.PrivateKey)
	if err != nil {
		return api.MaintainTunnelConfig{}, err
	}
	endpointKey, err := warpEndpointKey(e.cfg.Config.EndpointPublicKey)
	if err != nil {
		return api.MaintainTunnelConfig{}, err
	}
	endpoint, err := warpEndpoint(e.cfg.Config)
	if err != nil {
		return api.MaintainTunnelConfig{}, err
	}
	cert, err := warpClientCert(privateKey)
	if err != nil {
		return api.MaintainTunnelConfig{}, err
	}
	tlsConfig, err := api.PrepareTlsConfig(privateKey, endpointKey, cert, warpEndpointSNI, false)
	if err != nil {
		return api.MaintainTunnelConfig{}, err
	}
	listener := netns.Listener(e.cfg.Logf, netMon)
	dialer := netns.NewDialer(e.cfg.Logf, netMon)
	return api.MaintainTunnelConfig{
		TLSConfig:       tlsConfig,
		KeepalivePeriod: warpKeepalivePeriod,
		Endpoint:        endpoint,
		Device:          api.NewDeviceAdapter(e.cfg.Tun, warpMTU),
		MTU:             warpMTU,
		UseHTTP2:        e.cfg.Config.UseHTTP2,
		Transport: api.Transport{
			ListenPacket: listener.ListenPacket,
			DialContext:  dialer.DialContext,
		},
		Logf:          e.cfg.Logf,
		OnStateChange: e.onStateChange,
	}, nil
}

func (e *WarpEngine) onStateChange(state api.TunnelState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state == api.TunnelConnected {
		e.connected = true
		e.lastConnected = time.Now()
	} else {
		e.connected = false
	}
	e.notifyLocked()
}

func (e *WarpEngine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	started := e.started
	e.started = false
	e.connected = false
	cancel, done := e.cancel, e.done
	e.cancel, e.done = nil, nil
	netMon := e.netMon
	e.netMon = nil
	e.notifyLocked()
	e.mu.Unlock()

	if started {
		cancel()
		<-done
	}
	if netMon != nil {
		netMon.Close()
	}
	if e.cfg.Tun != nil {
		return e.cfg.Tun.Close()
	}
	return nil
}

func (e *WarpEngine) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("dial is not supported by warp profiles")
}

func (e *WarpEngine) Status(ctx context.Context) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	status := Status{
		ProfileID:  e.cfg.ProfileID,
		Alias:      e.cfg.Alias,
		Kind:       "warp",
		RouteAll:   true,
		ExitNodeID: e.cfg.Secrets.RegistrationID,
		SelfIPs:    warpSelfAddrs(e.cfg.Config),
		AvailableRoutes: []RouteStatus{
			{Prefix: netip.MustParsePrefix("0.0.0.0/0")},
			{Prefix: netip.MustParsePrefix("::/0")},
		},
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return status, nil
	}
	status.LastConnected = e.lastConnected
	if e.connected {
		status.BackendState = "Running"
	} else {
		status.BackendState = "Unavailable"
	}
	return status, nil
}

func (e *WarpEngine) WatchUpdates(ctx context.Context, notify func()) error {
	if notify == nil {
		return fmt.Errorf("nil update callback")
	}
	for {
		e.mu.Lock()
		ch := e.updateCh
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			notify()
		}
	}
}

func (e *WarpEngine) notifyLocked() {
	close(e.updateCh)
	e.updateCh = make(chan struct{})
}

func warpPrivateKey(encoded string) (*ecdsa.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("warp profile private key is not valid base64")
	}
	key, err := x509.ParseECPrivateKey(der)
	if err != nil {
		return nil, errors.New("warp profile private key is not a valid EC key")
	}
	return key, nil
}

func warpEndpointKey(encoded string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, errors.New("warp profile endpoint public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("warp profile endpoint public key is not a valid PKIX key")
	}
	ecKey, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("warp profile endpoint public key is not an EC key")
	}
	return ecKey, nil
}

func warpEndpoint(cfg warpcfg.Config) (net.Addr, error) {
	if cfg.UseHTTP2 {
		addr, err := warpAddrPort(cfg.EndpointH2V4, cfg.EndpointH2V6)
		if err != nil {
			return nil, errors.New("warp profile has no usable HTTP/2 endpoint")
		}
		return net.TCPAddrFromAddrPort(addr), nil
	}
	addr, err := warpAddrPort(cfg.EndpointV4, cfg.EndpointV6)
	if err != nil {
		return nil, errors.New("warp profile has no usable MASQUE endpoint")
	}
	return net.UDPAddrFromAddrPort(addr), nil
}

func warpAddrPort(v4, v6 string) (netip.AddrPort, error) {
	for _, encoded := range []string{v4, v6} {
		if encoded == "" {
			continue
		}
		addr, err := netip.ParseAddrPort(encoded)
		if err != nil {
			return netip.AddrPort{}, err
		}
		return addr, nil
	}
	return netip.AddrPort{}, errors.New("no endpoint")
}

func warpClientCert(privateKey *ecdsa.PrivateKey) ([][]byte, error) {
	template := &x509.Certificate{
		SerialNumber: big.NewInt(0),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, err
	}
	return [][]byte{der}, nil
}

func warpSelfAddrs(cfg warpcfg.Config) []netip.Addr {
	var addrs []netip.Addr
	for _, addr := range []netip.Addr{cfg.IPv4, cfg.IPv6} {
		if addr.IsValid() {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

var _ Engine = (*WarpEngine)(nil)
