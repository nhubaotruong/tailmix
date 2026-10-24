package profile

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"sync"
	"time"

	"github.com/maisem/tailmix/warpcfg"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/net/packet"
	"tailscale.com/types/ipproto"
)

const (
	// warpDNSTimeout bounds a tunnel-side exchange for callers that do not
	// supply a context. It matches the forwarder's own query timeout.
	warpDNSTimeout = 3 * time.Second
	warpDNSQueue   = 16
	warpDNSInbound = 64
)

var errWarpDNSClosed = errors.New("warp DNS device is closed")

// QueryDNS resolves a name through the tunnel using the profile's stored
// resolver.
func (e *WarpEngine) QueryDNS(name string, queryType dnsmessage.Type) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), warpDNSTimeout)
	defer cancel()
	return e.QueryDNSContext(ctx, name, queryType)
}

// QueryDNSContext resolves a name through the tunnel until ctx ends.
func (e *WarpEngine) QueryDNSContext(ctx context.Context, name string, queryType dnsmessage.Type) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	ready := e.started && e.connected && e.dev != nil
	e.mu.Unlock()
	if !ready {
		return warpDNSServfail(name, queryType)
	}
	return e.dev.query(ctx, name, queryType)
}

// warpDNSExchange is one query in flight through the tunnel.
type warpDNSExchange struct {
	resolver netip.AddrPort
	self     netip.Addr
	port     uint16
	id       uint16
	packet   []byte
	done     chan []byte
}

// warpDNSDevice sits between the tunnel loop and the profile's TUN. Egress
// packets come from queued queries. Ingress packets that answer a pending
// query complete it instead of reaching the TUN.
type warpDNSDevice struct {
	tun.Device

	resolver     netip.Addr
	self4, self6 netip.Addr

	egress  chan *warpDNSExchange
	inbound chan []byte
	closed  chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	pumpWG    sync.WaitGroup

	mu      sync.Mutex
	pending map[uint16]*warpDNSExchange
}

func newWarpDNSDevice(dev tun.Device, cfg warpcfg.Config) *warpDNSDevice {
	return &warpDNSDevice{
		Device:   dev,
		resolver: cfg.DNS,
		self4:    cfg.IPv4,
		self6:    cfg.IPv6,
		egress:   make(chan *warpDNSExchange, warpDNSQueue),
		inbound:  make(chan []byte, warpDNSInbound),
		closed:   make(chan struct{}),
		pending:  make(map[uint16]*warpDNSExchange),
	}
}

// start begins reading the tunnel so queued queries and inbound packets can
// wake a blocked adapter read.
func (d *warpDNSDevice) start() {
	d.startOnce.Do(func() {
		d.pumpWG.Add(1)
		go d.pump()
	})
}

// stop ends the read pump. The caller closes the underlying device first so a
// blocked read returns.
func (d *warpDNSDevice) stop() {
	d.stopOnce.Do(func() { close(d.closed) })
	d.pumpWG.Wait()
}

func (d *warpDNSDevice) pump() {
	defer d.pumpWG.Done()
	slab := make([]byte, warpMTU+2*tun.ReadPacketSpacing)
	packets := make([]tun.ReadPacket, 1)
	for {
		n, err := d.Device.Read(slab, packets)
		for i := 0; i < n; i++ {
			pkt := packets[i]
			data := append([]byte(nil), slab[pkt.Offset:pkt.Offset+pkt.Size]...)
			select {
			case d.inbound <- data:
			case <-d.closed:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (d *warpDNSDevice) Read(slab []byte, packets []tun.ReadPacket) (int, error) {
	for {
		// A queued query goes out before tunnel traffic.
		select {
		case ex := <-d.egress:
			if !d.stillPending(ex) {
				continue
			}
			return readPacket(slab, packets, ex.packet)
		default:
		}
		select {
		case ex := <-d.egress:
			if !d.stillPending(ex) {
				continue
			}
			return readPacket(slab, packets, ex.packet)
		case pkt := <-d.inbound:
			return readPacket(slab, packets, pkt)
		case <-d.closed:
			return 0, io.EOF
		}
	}
}

func (d *warpDNSDevice) Write(bufs [][]byte, offset int) (int, error) {
	rest := make([][]byte, 0, len(bufs))
	for _, buf := range bufs {
		if d.complete(buf[offset:]) {
			continue
		}
		rest = append(rest, buf)
	}
	if len(rest) == 0 {
		return len(bufs), nil
	}
	if _, err := d.Device.Write(rest, offset); err != nil {
		return 0, err
	}
	return len(bufs), nil
}

func readPacket(slab []byte, packets []tun.ReadPacket, pkt []byte) (int, error) {
	offset := tun.ReadPacketSpacing
	if len(pkt) > len(slab)-offset-tun.ReadPacketSpacing {
		return 0, tun.ErrTooManySegments
	}
	copy(slab[offset:], pkt)
	packets[0] = tun.ReadPacket{Offset: offset, Size: len(pkt)}
	return 1, nil
}

func (d *warpDNSDevice) stillPending(ex *warpDNSExchange) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending[ex.port] == ex
}

// complete hands the payload of a matching reply to its exchange and reports
// whether the packet was taken.
func (d *warpDNSDevice) complete(pkt []byte) bool {
	var parsed packet.Parsed
	parsed.Decode(pkt)
	if parsed.IPProto != ipproto.UDP {
		return false
	}
	payload := parsed.Payload()
	if len(payload) < 2 {
		return false
	}
	d.mu.Lock()
	ex := d.pending[parsed.Dst.Port()]
	d.mu.Unlock()
	if ex == nil || ex.resolver != parsed.Src || ex.self != parsed.Dst.Addr() {
		return false
	}
	if binary.BigEndian.Uint16(payload[:2]) != ex.id {
		return false
	}
	d.mu.Lock()
	delete(d.pending, ex.port)
	d.mu.Unlock()
	select {
	case ex.done <- append([]byte(nil), payload...):
	default:
	}
	return true
}

func (d *warpDNSDevice) query(ctx context.Context, name string, queryType dnsmessage.Type) ([]byte, error) {
	question, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	resolver := d.resolver
	self := d.self4
	if resolver.Is6() {
		self = d.self6
	}
	if !resolver.IsValid() || !self.IsValid() {
		return nil, errors.New("warp profile has no tunnel to answer DNS")
	}

	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	ex := &warpDNSExchange{
		resolver: netip.AddrPortFrom(resolver, 53),
		self:     self,
		id:       binary.BigEndian.Uint16(seed[:2]),
		done:     make(chan []byte, 1),
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{ID: ex.id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  question,
			Type:  queryType,
			Class: dnsmessage.ClassINET,
		}},
	}
	wire, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	if err := d.reserve(ex); err != nil {
		return nil, err
	}
	ex.packet = warpDNSQueryPacket(ex, wire)
	select {
	case d.egress <- ex:
	case <-ctx.Done():
		d.remove(ex)
		return nil, ctx.Err()
	case <-d.closed:
		d.remove(ex)
		return nil, errWarpDNSClosed
	}
	select {
	case response := <-ex.done:
		return response, nil
	case <-ctx.Done():
		d.remove(ex)
		return nil, ctx.Err()
	}
}

// reserve allocates a source port that no pending exchange holds.
func (d *warpDNSDevice) reserve(ex *warpDNSExchange) error {
	var seed [2]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return err
	}
	port := 1024 + binary.BigEndian.Uint16(seed[:])
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := 0; i < 8 && d.pending[port] != nil; i++ {
		port++
		if port < 1024 {
			port = 1024
		}
	}
	if d.pending[port] != nil {
		return errors.New("warp DNS has no free source port")
	}
	ex.port = port
	d.pending[port] = ex
	return nil
}

func (d *warpDNSDevice) remove(ex *warpDNSExchange) {
	d.mu.Lock()
	if d.pending[ex.port] == ex {
		delete(d.pending, ex.port)
	}
	d.mu.Unlock()
}

func warpDNSQueryPacket(ex *warpDNSExchange, payload []byte) []byte {
	if ex.self.Is4() {
		return packet.Generate(packet.UDP4Header{
			IP4Header: packet.IP4Header{Src: ex.self, Dst: ex.resolver.Addr()},
			SrcPort:   ex.port,
			DstPort:   ex.resolver.Port(),
		}, payload)
	}
	return packet.Generate(packet.UDP6Header{
		IP6Header: packet.IP6Header{Src: ex.self, Dst: ex.resolver.Addr()},
		SrcPort:   ex.port,
		DstPort:   ex.resolver.Port(),
	}, payload)
}

func warpDNSServfail(name string, queryType dnsmessage.Type) ([]byte, error) {
	question, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	return (&dnsmessage.Message{
		Header: dnsmessage.Header{
			Response:           true,
			RecursionAvailable: true,
			RCode:              dnsmessage.RCodeServerFailure,
		},
		Questions: []dnsmessage.Question{{
			Name:  question,
			Type:  queryType,
			Class: dnsmessage.ClassINET,
		}},
	}).Pack()
}
