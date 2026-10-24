package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Diniboy1123/connect-ip-go"
	"github.com/maisem/tailmix/tunmux"
)

// testEndpointIP is from the documentation range. No test lets it reach a log.
const testEndpointIP = "198.51.100.7"

func testEndpoint() *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(testEndpointIP), Port: 443}
}

func testConfig(dev TunnelDevice, logf func(format string, args ...any), onState func(TunnelState)) MaintainTunnelConfig {
	return MaintainTunnelConfig{
		Endpoint:      testEndpoint(),
		Device:        dev,
		MTU:           1280,
		TLSConfig:     &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}},
		Logf:          logf,
		OnStateChange: onState,
	}
}

// stubDial replaces the dial seam for one test.
func stubDial(t *testing.T, dial func(ctx context.Context, cfg MaintainTunnelConfig) (*connection, error)) {
	t.Helper()
	orig := dialTunnel
	t.Cleanup(func() { dialTunnel = orig })
	dialTunnel = dial
}

// fakeDevice blocks in ReadPacket until the test releases it.
type fakeDevice struct {
	readCalls   atomic.Int32
	block       chan struct{}
	releaseOnce sync.Once
	mu          sync.Mutex
	wrote       [][]byte
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{block: make(chan struct{})}
}

func (d *fakeDevice) ReadPacket(buf []byte) (int, error) {
	d.readCalls.Add(1)
	<-d.block
	return 0, errors.New("device closed")
}

func (d *fakeDevice) WritePacket(pkt []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wrote = append(d.wrote, append([]byte(nil), pkt...))
	return nil
}

func (d *fakeDevice) release() {
	d.releaseOnce.Do(func() { close(d.block) })
}

// fakeIPConn stands in for the Connect-IP connection.
type fakeIPConn struct {
	read func() ([]byte, error)

	closeOnce sync.Once
	closed    chan struct{}
}

func newFakeIPConn() *fakeIPConn {
	return &fakeIPConn{closed: make(chan struct{})}
}

func (c *fakeIPConn) WritePacketBuffer(buf []byte, packetOffset, packetLen int) ([]byte, error) {
	return nil, nil
}

func (c *fakeIPConn) ReadPacketZeroCopy(allowAny bool) ([]byte, error) {
	if c.read != nil {
		return c.read()
	}
	<-c.closed
	return nil, errors.New("connection closed")
}

func (c *fakeIPConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func fakeConnection(conn ipConn) *connection {
	return &connection{
		conn:     conn,
		response: &http.Response{StatusCode: http.StatusOK, Status: "200 OK"},
		endpoint: testEndpoint(),
	}
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRecorder) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprint(l.lines)
}

type stateRecorder struct {
	mu     sync.Mutex
	states []TunnelState
}

func (r *stateRecorder) record(state TunnelState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state)
}

func (r *stateRecorder) snapshot() []TunnelState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TunnelState(nil), r.states...)
}

// A cancelled context must end the tunnel loop instead of leaving the process
// running a dead tunnel.
func TestMaintainTunnelReturnsOnCancel(t *testing.T) {
	ipConn := newFakeIPConn()
	stubDial(t, func(ctx context.Context, cfg MaintainTunnelConfig) (*connection, error) {
		return fakeConnection(ipConn), nil
	})

	dev := newFakeDevice()
	defer dev.release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- MaintainTunnel(ctx, testConfig(dev, nil, nil)) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("MaintainTunnel returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MaintainTunnel did not return after the context was cancelled")
	}
}

// Neither a failed connect nor a lost connection may put the endpoint address
// or a DNS name into a log line.
func TestMaintainTunnelLogsNoEndpoint(t *testing.T) {
	endpoint := testEndpoint()
	leakyError := fmt.Errorf("dial udp %s: read tcp %s: no route to %s", endpoint, endpoint, "cloudflareaccess.com")

	tests := []struct {
		name  string
		dial  func(ctx context.Context, cfg MaintainTunnelConfig) (*connection, error)
		state []TunnelState
	}{
		{
			name: "connect failure",
			dial: func(ctx context.Context, cfg MaintainTunnelConfig) (*connection, error) {
				return nil, leakyError
			},
			state: []TunnelState{TunnelConnecting, TunnelDisconnected},
		},
		{
			name: "connection loss",
			dial: func(ctx context.Context, cfg MaintainTunnelConfig) (*connection, error) {
				ipConn := newFakeIPConn()
				ipConn.read = func() ([]byte, error) {
					return nil, fmt.Errorf("read from %s: %w", endpoint, &connectip.CloseError{Remote: true})
				}
				return fakeConnection(ipConn), nil
			},
			state: []TunnelState{TunnelConnecting, TunnelConnected, TunnelDisconnected},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stubDial(t, test.dial)

			dev := newFakeDevice()
			defer dev.release()

			recorder := &logRecorder{}
			states := &stateRecorder{}

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_ = MaintainTunnel(ctx, testConfig(dev, recorder.Logf, states.record))

			logs := recorder.text()
			if logs == "" {
				t.Fatal("no log line was produced")
			}
			for _, sensitive := range []string{testEndpointIP, "cloudflareaccess.com", endpoint.String()} {
				if bytes.Contains([]byte(logs), []byte(sensitive)) {
					t.Fatalf("log line carries %q:\n%s", sensitive, logs)
				}
			}

			got := states.snapshot()
			if len(got) < len(test.state) {
				t.Fatalf("reported states %v, want at least %v", got, test.state)
			}
			for i := range test.state {
				if got[i] != test.state[i] {
					t.Fatalf("reported states %v, want them to start with %v", got, test.state)
				}
			}
		})
	}
}

// QUIC must dial through the packet socket the caller supplies.
func TestMaintainTunnelUsesCallerPacketSocket(t *testing.T) {
	var listenCalls atomic.Int32
	var writes atomic.Int32

	transport := Transport{
		ListenPacket: func(ctx context.Context, network, address string) (net.PacketConn, error) {
			listenCalls.Add(1)
			if network != "udp4" {
				t.Errorf("ListenPacket network = %q, want udp4", network)
			}
			packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			return &countingPacketConn{PacketConn: packet, writes: &writes}, nil
		},
	}

	dev := newFakeDevice()
	defer dev.release()

	cfg := testConfig(dev, nil, nil)
	cfg.Transport = transport

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = MaintainTunnel(ctx, cfg)

	if listenCalls.Load() == 0 {
		t.Fatal("the tunnel did not call the caller's ListenPacket")
	}
	if writes.Load() == 0 {
		t.Fatal("no QUIC packet left through the caller's socket")
	}
	if got := dev.readCalls.Load(); got != 0 {
		t.Fatalf("the device was read %d times before any tunnel connected", got)
	}
}

// HTTP/2 must dial through the caller's dialer and address.
func TestMaintainTunnelUsesCallerDialer(t *testing.T) {
	var mu sync.Mutex
	var dialed []string

	transport := Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			dialed = append(dialed, address)
			return nil, errors.New("no route to the endpoint")
		},
	}

	dev := newFakeDevice()
	defer dev.release()

	cfg := testConfig(dev, nil, nil)
	cfg.Transport = transport
	cfg.UseHTTP2 = true
	cfg.Endpoint = &net.TCPAddr{IP: net.ParseIP(testEndpointIP), Port: 443}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = MaintainTunnel(ctx, cfg)

	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 {
		t.Fatalf("the tunnel dialed %v, want exactly one call through the caller's dialer", dialed)
	}
	if want := cfg.Endpoint.String(); dialed[0] != want {
		t.Fatalf("dialed %q, want %q", dialed[0], want)
	}
}

// A packet written to the adapter must reach the ChanTUN channel, and a packet
// injected into the ChanTUN must come back out of the adapter.
func TestDeviceAdapterRoundTrip(t *testing.T) {
	chanTUN := tunmux.NewChanTUN("warp-test")
	defer chanTUN.Close()

	adapter := NewDeviceAdapter(chanTUN, 1280)

	outbound := []byte{0x45, 0x00, 0x00, 0x14, 0x2a}
	if err := adapter.WritePacket(outbound); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	select {
	case packet := <-chanTUN.Inbound():
		got := append([]byte(nil), packet.Bytes()...)
		packet.Release()
		if !bytes.Equal(got, outbound) {
			t.Fatalf("ChanTUN inbound carried %x, want %x", got, outbound)
		}
	case <-time.After(time.Second):
		t.Fatal("the packet did not reach the ChanTUN inbound channel")
	}

	inbound := []byte{0x60, 0x00, 0x00, 0x00, 0x07}
	if !chanTUN.TryInjectOutbound(inbound) {
		t.Fatal("ChanTUN rejected the injected packet")
	}
	buf := make([]byte, 1280)
	n, err := adapter.ReadPacket(buf)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(buf[:n], inbound) {
		t.Fatalf("the adapter read %x, want %x", buf[:n], inbound)
	}
}

func TestBackoffBase(t *testing.T) {
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for attempt, expected := range want {
		if got := backoffBase(attempt); got != expected {
			t.Fatalf("backoffBase(%d) = %s, want %s", attempt, got, expected)
		}
	}

	for attempt := range want {
		base := backoffBase(attempt)
		spread := time.Duration(float64(base) * reconnectJitter)
		for i := 0; i < 100; i++ {
			delay := backoffDelay(attempt)
			if delay < base-spread || delay > base+spread {
				t.Fatalf("backoffDelay(%d) = %s, want within %s of %s", attempt, delay, spread, base)
			}
		}
	}
}

// After a connection loss the loop must connect again, and the wait between
// attempts must grow.
func TestMaintainTunnelRetriesAfterConnectionLoss(t *testing.T) {
	var mu sync.Mutex
	var attempts []time.Time

	stubDial(t, func(ctx context.Context, cfg MaintainTunnelConfig) (*connection, error) {
		mu.Lock()
		attempts = append(attempts, time.Now())
		mu.Unlock()

		ipConn := newFakeIPConn()
		ipConn.read = func() ([]byte, error) {
			return nil, &connectip.CloseError{Remote: true}
		}
		return fakeConnection(ipConn), nil
	})

	dev := newFakeDevice()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	go func() {
		// Let the pumps of the first connection finish, so the next attempt
		// starts straight away instead of after the shutdown grace.
		time.Sleep(100 * time.Millisecond)
		dev.release()
	}()
	_ = MaintainTunnel(ctx, testConfig(dev, nil, nil))

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) < 3 {
		t.Fatalf("the loop made %d attempts, want at least 3", len(attempts))
	}
	firstGap := attempts[1].Sub(attempts[0])
	secondGap := attempts[2].Sub(attempts[1])
	if secondGap <= firstGap {
		t.Fatalf("waits did not grow: first %s, second %s", firstGap, secondGap)
	}
}

// A bad configuration must come back as an error. Upstream exited the process.
func TestMaintainTunnelRejectsInvalidConfig(t *testing.T) {
	dev := newFakeDevice()
	defer dev.release()

	tests := []struct {
		name string
		cfg  MaintainTunnelConfig
	}{
		{"no device", MaintainTunnelConfig{Endpoint: testEndpoint(), MTU: 1280, TLSConfig: testConfig(dev, nil, nil).TLSConfig}},
		{"no MTU", MaintainTunnelConfig{Endpoint: testEndpoint(), Device: dev, TLSConfig: testConfig(dev, nil, nil).TLSConfig}},
		{"no TLS config", MaintainTunnelConfig{Endpoint: testEndpoint(), Device: dev, MTU: 1280}},
		{"endpoint type mismatch", MaintainTunnelConfig{Endpoint: &net.TCPAddr{IP: net.ParseIP(testEndpointIP), Port: 443}, Device: dev, MTU: 1280, TLSConfig: testConfig(dev, nil, nil).TLSConfig}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := MaintainTunnel(context.Background(), test.cfg)
			if err == nil {
				t.Fatal("MaintainTunnel accepted an invalid configuration")
			}
		})
	}
}

type countingPacketConn struct {
	net.PacketConn
	writes *atomic.Int32
}

func (c *countingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.writes.Add(1)
	return c.PacketConn.WriteTo(p, addr)
}
