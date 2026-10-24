package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	connectip "github.com/Diniboy1123/connect-ip-go"
	"github.com/maisem/tailmix/usque/internal"
	"github.com/tailscale/wireguard-go/tun"
)

// NetBuffer is a pool of byte slices with a fixed capacity.
// Helps to reduce memory allocations and improve performance.
// It uses a sync.Pool to manage the byte slices.
// The capacity of the byte slices is set when the pool is created.
type NetBuffer struct {
	capacity int
	buf      sync.Pool
}

// Get returns a byte slice from the pool.
func (n *NetBuffer) Get() []byte {
	return *(n.buf.Get().(*[]byte))
}

// Put places a byte slice back into the pool.
// It checks if the capacity of the byte slice matches the pool's capacity.
// If it doesn't match, the byte slice is not returned to the pool.
func (n *NetBuffer) Put(buf []byte) {
	if cap(buf) != n.capacity {
		return
	}
	n.buf.Put(&buf)
}

// NewNetBuffer creates a new NetBuffer with the specified capacity.
// The capacity must be greater than 0.
func NewNetBuffer(capacity int) *NetBuffer {
	if capacity <= 0 {
		panic("capacity must be greater than 0")
	}
	return &NetBuffer{
		capacity: capacity,
		buf: sync.Pool{
			New: func() interface{} {
				b := make([]byte, capacity)
				return &b
			},
		},
	}
}

// TunnelDevice abstracts a TUN device so that we can use the same tunnel-maintenance code
// regardless of the underlying implementation.
type TunnelDevice interface {
	// ReadPacket reads a packet from the device (using the given mtu) and returns its contents.
	ReadPacket(buf []byte) (int, error)
	// WritePacket writes a packet to the device.
	WritePacket(pkt []byte) error
}

// DeviceAdapter wraps a tun.Device to satisfy TunnelDevice. It reads one
// packet per call through the batch Read shape the device implements.
type DeviceAdapter struct {
	dev   tun.Device
	slab  []byte
	batch []tun.ReadPacket
}

var _ TunnelDevice = (*DeviceAdapter)(nil)

// NewDeviceAdapter creates a new DeviceAdapter.
func NewDeviceAdapter(dev tun.Device, mtu int) *DeviceAdapter {
	return &DeviceAdapter{
		dev: dev,
		// The device writes each packet at its own offset and may prepend or
		// append the per-packet spacing, so one packet needs it on both ends.
		slab:  make([]byte, mtu+2*tun.ReadPacketSpacing),
		batch: make([]tun.ReadPacket, 1),
	}
}

func (a *DeviceAdapter) ReadPacket(buf []byte) (int, error) {
	for {
		n, err := a.dev.Read(a.slab, a.batch)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}

		pkt := a.batch[0]
		return copy(buf, a.slab[pkt.Offset:pkt.Offset+pkt.Size]), nil
	}
}

func (a *DeviceAdapter) WritePacket(pkt []byte) error {
	_, err := a.dev.Write([][]byte{pkt}, 0)
	return err
}

// pumpShutdownGrace bounds how long the supervisor waits for both forwarding
// pumps to exit after an error before spawning a fresh pair. A device-side
// pump may still be parked in a blocking TUN read during this window; the
// readMu serializes any overlap with the next cycle's device reader.
const pumpShutdownGrace = 2 * time.Second

// CONNECT-IP context ID 0 is encoded as a one-byte QUIC varint. Reserving this
// byte before packets lets connect-ip-go send outbound datagrams in place.
const datagramContextIDHeadroom = 1

// Reconnect waits double from one second to thirty, and jitter spreads the
// retries of many profiles.
const (
	reconnectMinDelay = time.Second
	reconnectMaxDelay = 30 * time.Second
	reconnectJitter   = 0.2
)

// backoffBase returns the un-jittered wait before the given zero-based attempt.
func backoffBase(attempt int) time.Duration {
	d := reconnectMinDelay
	for i := 0; i < attempt && d < reconnectMaxDelay; i++ {
		d *= 2
	}
	return min(d, reconnectMaxDelay)
}

// backoffDelay returns the wait before the given zero-based attempt, jittered
// by ±reconnectJitter.
func backoffDelay(attempt int) time.Duration {
	d := backoffBase(attempt)
	spread := float64(d) * reconnectJitter
	return d + time.Duration((rand.Float64()*2-1)*spread)
}

// MaintainTunnelConfig contains runtime settings for tunnel maintenance.
type MaintainTunnelConfig struct {
	TLSConfig         *tls.Config
	KeepalivePeriod   time.Duration
	InitialPacketSize uint16
	Endpoint          net.Addr
	Device            TunnelDevice
	MTU               int
	UseHTTP2          bool
	// Transport supplies the sockets the tunnel dials through. Both the QUIC
	// socket and the HTTP/2 socket come from it.
	Transport Transport
	// Logf receives progress lines. It defaults to the standard logger. No
	// line carries the endpoint address (R40).
	Logf func(format string, args ...any)
	// OnStateChange reports every connect attempt outcome.
	OnStateChange func(TunnelState)
}

func (c MaintainTunnelConfig) validate() error {
	if c.Device == nil {
		return errors.New("MaintainTunnel: no device")
	}
	if c.MTU <= 0 {
		return errors.New("MaintainTunnel: MTU must be positive")
	}
	if c.TLSConfig == nil {
		return errors.New("MaintainTunnel: no TLS config")
	}
	if c.UseHTTP2 {
		if _, ok := c.Endpoint.(*net.TCPAddr); !ok {
			return fmt.Errorf("MaintainTunnel: HTTP/2 mode requires a *net.TCPAddr endpoint, got %T", c.Endpoint)
		}
	} else if _, ok := c.Endpoint.(*net.UDPAddr); !ok {
		return fmt.Errorf("MaintainTunnel: HTTP/3 mode requires a *net.UDPAddr endpoint, got %T", c.Endpoint)
	}
	return nil
}

func (c MaintainTunnelConfig) logf(format string, args ...any) {
	if c.Logf == nil {
		log.Printf(format, args...)
		return
	}
	c.Logf(format, args...)
}

// redactErrorText removes the addresses an error message carries, because no
// log line may name a packet endpoint or a DNS name (R40).
func redactErrorText(err error, endpoint net.Addr) string {
	if err == nil {
		return ""
	}

	var oldNew []string
	redact := func(sensitive string) {
		if sensitive != "" {
			oldNew = append(oldNew, sensitive, "<endpoint>")
		}
	}
	if endpoint != nil {
		redact(endpoint.String())
		if host, _, splitErr := net.SplitHostPort(endpoint.String()); splitErr == nil {
			redact(host)
		}
	}
	if u, parseErr := url.Parse(internal.ConnectURI); parseErr == nil {
		redact(u.Host)
	}
	redact(internal.ConnectSNI)

	return strings.NewReplacer(oldNew...).Replace(err.Error())
}

// dialTunnel is a variable so tests can drive the loop without a peer.
var dialTunnel = func(ctx context.Context, cfg MaintainTunnelConfig) (*connection, error) {
	return connectTunnel(
		ctx,
		cfg.Transport,
		cfg.TLSConfig,
		internal.DefaultQuicConfig(cfg.KeepalivePeriod, cfg.InitialPacketSize),
		internal.ConnectURI,
		cfg.Endpoint,
		cfg.UseHTTP2,
	)
}

// sleepCtx sleeps for d or until ctx is cancelled, whichever comes first.
// Returns ctx.Err() on cancellation and nil on normal completion.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// MaintainTunnel continuously connects to the MASQUE server, then starts two
// forwarding goroutines: one forwarding from the device to the IP connection (and handling
// any ICMP reply), and the other forwarding from the IP connection to the device.
// If an error occurs in either loop, the connection is closed and another
// attempt follows after a bounded exponential backoff. Every attempt outcome
// is reported to OnStateChange (R15). It returns when ctx is cancelled.
//
// Parameters:
//   - ctx: context.Context - The context for the connection.
//   - cfg: MaintainTunnelConfig - Tunnel maintenance runtime configuration.
func MaintainTunnel(ctx context.Context, cfg MaintainTunnelConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	packetBufferPool := NewNetBuffer(cfg.MTU + datagramContextIDHeadroom)

	report := func(state TunnelState) {
		if cfg.OnStateChange != nil {
			cfg.OnStateChange(state)
		}
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		report(TunnelConnecting)
		conn, err := dialTunnel(ctx, cfg)
		if err != nil {
			report(TunnelDisconnected)
			cfg.logf("MASQUE connect attempt failed: %s", redactErrorText(err, cfg.Endpoint))
			if sleepErr := sleepCtx(ctx, backoffDelay(attempt)); sleepErr != nil {
				return sleepErr
			}
			continue
		}

		if conn.response.StatusCode != http.StatusOK {
			status := conn.response.Status
			_ = conn.Close()
			report(TunnelDisconnected)
			cfg.logf("MASQUE connect attempt was rejected: %s", status)
			if sleepErr := sleepCtx(ctx, backoffDelay(attempt)); sleepErr != nil {
				return sleepErr
			}
			continue
		}

		conn.logf = cfg.logf
		report(TunnelConnected)

		err = conn.run(ctx, cfg.Device, packetBufferPool)
		report(TunnelDisconnected)
		cfg.logf("MASQUE connection lost: %s", redactErrorText(err, cfg.Endpoint))
		_ = conn.Close()

		if sleepErr := sleepCtx(ctx, backoffDelay(attempt)); sleepErr != nil {
			return sleepErr
		}
	}
}

// run forwards packets both ways until either side fails.
func (c *connection) run(ctx context.Context, dev TunnelDevice, packetBufferPool *NetBuffer) error {
	errChan := make(chan error, 2)
	pumpCtx, cancelPumps := context.WithCancel(ctx)

	var wg sync.WaitGroup
	var readMu sync.Mutex

	wg.Add(2)

	go func() {
		defer wg.Done()
		for {
			if pumpCtx.Err() != nil {
				return
			}
			buf := packetBufferPool.Get()
			readMu.Lock()
			n, err := dev.ReadPacket(buf[datagramContextIDHeadroom:])
			readMu.Unlock()
			if err != nil {
				packetBufferPool.Put(buf)
				errChan <- fmt.Errorf("failed to read from TUN device: %w", err)
				return
			}
			if pumpCtx.Err() != nil {
				packetBufferPool.Put(buf)
				return
			}
			icmp, err := c.conn.WritePacketBuffer(buf, datagramContextIDHeadroom, n)
			if err != nil {
				packetBufferPool.Put(buf)
				if errors.As(err, new(*connectip.CloseError)) {
					errChan <- fmt.Errorf("connection closed while writing to IP connection: %w", err)
					return
				}
				c.logf("error writing to IP connection: %s, continuing", redactErrorText(err, c.endpoint))
				continue
			}
			packetBufferPool.Put(buf)

			if len(icmp) > 0 {
				if err := dev.WritePacket(icmp); err != nil {
					if errors.As(err, new(*connectip.CloseError)) {
						errChan <- fmt.Errorf("connection closed while writing ICMP to TUN device: %w", err)
						return
					}
					c.logf("error writing ICMP to TUN device: %s, continuing", redactErrorText(err, c.endpoint))
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		for {
			packet, err := c.conn.ReadPacketZeroCopy(true)
			if err != nil {
				if c.useHTTP2 {
					errChan <- fmt.Errorf("connection closed while reading from IP connection: %w", err)
					return
				}
				if errors.As(err, new(*connectip.CloseError)) {
					errChan <- fmt.Errorf("connection closed while reading from IP connection: %w", err)
					return
				}
				c.logf("error reading from IP connection: %s, continuing", redactErrorText(err, c.endpoint))
				continue
			}
			if err := dev.WritePacket(packet); err != nil {
				errChan <- fmt.Errorf("failed to write to TUN device: %w", err)
				return
			}
		}
	}()

	err := error(nil)
	cancelled := false
	select {
	case err = <-errChan:
	case <-ctx.Done():
		// Closing the connection is the caller's job, so pumps parked in a
		// peer read end there.
		err = ctx.Err()
		cancelled = true
	}

	cancelPumps()

	if cancelled {
		return err
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(pumpShutdownGrace):
		c.logf("pump shutdown grace of %s expired; a stale TUN reader may still be parked", pumpShutdownGrace)
	}

	return err
}
