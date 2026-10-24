package profile

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/maisem/tailmix/dns"
	"github.com/maisem/tailmix/tunmux"
	"github.com/maisem/tailmix/usque/api"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/net/packet"
)

var (
	_ dns.ProfileDNSQueryer        = (*WarpEngine)(nil)
	_ dns.ProfileDNSContextQueryer = (*WarpEngine)(nil)
)

// A "." ProfileDNS route is published by pointing the forwarder at the
// selected engine, so the forwarder must accept it without error.
func TestWarpEngineStartsProfileDNSForwarder(t *testing.T) {
	engine := testWarpEngine(t)
	forwarder, err := dns.StartProfileDNSForwarder(t.Context(), engine)
	if err != nil {
		t.Fatalf("start forwarder: %v", err)
	}
	t.Cleanup(func() { _ = forwarder.Close() })
}

func startWarpEngine(t *testing.T, engine *WarpEngine, state api.TunnelState) {
	t.Helper()
	engine.maintain = func(ctx context.Context, cfg api.MaintainTunnelConfig) error {
		cfg.OnStateChange(state)
		<-ctx.Done()
		return ctx.Err()
	}
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	synctest.Wait()
}

func readWarpPacket(t *testing.T, dev tun.Device) []byte {
	t.Helper()
	slab := make([]byte, warpMTU+2*tun.ReadPacketSpacing)
	packets := make([]tun.ReadPacket, 1)
	n, err := dev.Read(slab, packets)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("read %d packets, want 1", n)
	}
	pkt := packets[0]
	return append([]byte(nil), slab[pkt.Offset:pkt.Offset+pkt.Size]...)
}

// warpDNSReply builds the tunnel-side answer to query, sent from resolver.
func warpDNSReply(t *testing.T, query []byte, resolver netip.AddrPort, answer netip.Addr) []byte {
	t.Helper()
	var parsed packet.Parsed
	parsed.Decode(query)
	var msg dnsmessage.Message
	if err := msg.Unpack(parsed.Payload()); err != nil {
		t.Fatal(err)
	}
	wire, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: msg.Header.ID, Response: true, RecursionAvailable: true},
		Questions: msg.Questions,
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name:  msg.Questions[0].Name,
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			},
			Body: &dnsmessage.AResource{A: answer.As4()},
		}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Src.Addr().Is4() {
		return packet.Generate(packet.UDP4Header{
			IP4Header: packet.IP4Header{Src: resolver.Addr(), Dst: parsed.Src.Addr()},
			SrcPort:   resolver.Port(),
			DstPort:   parsed.Src.Port(),
		}, wire)
	}
	return packet.Generate(packet.UDP6Header{
		IP6Header: packet.IP6Header{Src: resolver.Addr(), Dst: parsed.Src.Addr()},
		SrcPort:   resolver.Port(),
		DstPort:   parsed.Src.Port(),
	}, wire)
}

func warpDNSAnswer(t *testing.T, response []byte) netip.Addr {
	t.Helper()
	var msg dnsmessage.Message
	if err := msg.Unpack(response); err != nil {
		t.Fatal(err)
	}
	if len(msg.Answers) != 1 {
		t.Fatalf("answers = %v, want one", msg.Answers)
	}
	a, ok := msg.Answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("answer body = %T, want A", msg.Answers[0].Body)
	}
	return netip.AddrFrom4(a.A)
}

func TestWarpDNSResolvesThroughTunnel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		startWarpEngine(t, engine, api.TunnelConnected)

		results := make(chan []byte, 1)
		go func() {
			response, err := engine.QueryDNS("example.com.", dnsmessage.TypeA)
			if err != nil {
				t.Errorf("QueryDNS: %v", err)
			}
			results <- response
		}()

		query := readWarpPacket(t, engine.dev)
		var parsed packet.Parsed
		parsed.Decode(query)
		if parsed.Dst != netip.MustParseAddrPort("1.1.1.1:53") {
			t.Fatalf("query destination = %v, want stored resolver 1.1.1.1:53", parsed.Dst)
		}
		if parsed.Src.Addr() != engine.cfg.Config.IPv4 {
			t.Fatalf("query source = %v, want tunnel address %v", parsed.Src.Addr(), engine.cfg.Config.IPv4)
		}
		answer := netip.MustParseAddr("93.184.216.34")
		reply := warpDNSReply(t, query, netip.MustParseAddrPort("1.1.1.1:53"), answer)
		if n, err := engine.dev.Write([][]byte{reply}, 0); err != nil || n != 1 {
			t.Fatalf("write reply = %d, %v", n, err)
		}

		synctest.Wait()
		if got := warpDNSAnswer(t, <-results); got != answer {
			t.Fatalf("answer = %v, want %v", got, answer)
		}
	})
}

func TestWarpDNSUnrelatedReplyReachesDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		startWarpEngine(t, engine, api.TunnelConnected)
		chanTUN := engine.cfg.Tun.(*tunmux.ChanTUN)

		results := make(chan []byte, 1)
		go func() {
			response, err := engine.QueryDNS("example.com.", dnsmessage.TypeA)
			if err != nil {
				t.Errorf("QueryDNS: %v", err)
			}
			results <- response
		}()
		query := readWarpPacket(t, engine.dev)

		// Same port and transaction ID, but not from the stored resolver.
		stray := warpDNSReply(t, query, netip.MustParseAddrPort("198.51.100.9:53"), netip.MustParseAddr("93.184.216.7"))
		if n, err := engine.dev.Write([][]byte{stray}, 0); err != nil || n != 1 {
			t.Fatalf("write stray reply = %d, %v", n, err)
		}
		select {
		case pkt := <-chanTUN.Inbound():
			if !bytes.Equal(pkt.Bytes(), stray) {
				t.Fatal("device received a different packet than the stray reply")
			}
		default:
			t.Fatal("stray reply did not reach the device")
		}
		select {
		case <-results:
			t.Fatal("stray reply completed the pending exchange")
		default:
		}

		answer := netip.MustParseAddr("93.184.216.34")
		reply := warpDNSReply(t, query, netip.MustParseAddrPort("1.1.1.1:53"), answer)
		if _, err := engine.dev.Write([][]byte{reply}, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := warpDNSAnswer(t, <-results); got != answer {
			t.Fatalf("answer = %v, want %v", got, answer)
		}
	})
}

func TestWarpDNSConcurrentQueriesAndTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		startWarpEngine(t, engine, api.TunnelConnected)

		// The forwarder passes only the name and type; each tunnel-side
		// exchange gets its own source port and transaction ID.
		results := make(chan string, 2)
		for _, name := range []string{"one.example.", "two.example."} {
			go func() {
				response, err := engine.QueryDNS(name, dnsmessage.TypeA)
				if err != nil {
					t.Errorf("QueryDNS(%s): %v", name, err)
				}
				results <- warpDNSAnswer(t, response).String()
			}()
		}

		queries := make(map[string][]byte, 2)
		ports := make(map[uint16]bool, 2)
		ids := make(map[uint16]bool, 2)
		for range 2 {
			query := readWarpPacket(t, engine.dev)
			var parsed packet.Parsed
			parsed.Decode(query)
			var msg dnsmessage.Message
			if err := msg.Unpack(parsed.Payload()); err != nil {
				t.Fatal(err)
			}
			name := msg.Questions[0].Name.String()
			queries[name] = query
			if ports[parsed.Src.Port()] {
				t.Fatalf("two exchanges share source port %d", parsed.Src.Port())
			}
			ports[parsed.Src.Port()] = true
			if ids[msg.Header.ID] {
				t.Fatalf("two exchanges share transaction ID %d", msg.Header.ID)
			}
			ids[msg.Header.ID] = true
		}

		answers := map[string]netip.Addr{
			"one.example.": netip.MustParseAddr("93.184.216.34"),
			"two.example.": netip.MustParseAddr("93.184.216.35"),
		}
		// Answer in the opposite order of the queries.
		for _, name := range []string{"two.example.", "one.example."} {
			reply := warpDNSReply(t, queries[name], netip.MustParseAddrPort("1.1.1.1:53"), answers[name])
			if _, err := engine.dev.Write([][]byte{reply}, 0); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		got := map[string]bool{}
		for range 2 {
			got[<-results] = true
		}
		for name, answer := range answers {
			if !got[answer.String()] {
				t.Fatalf("query %s did not get answer %v: %v", name, answer, got)
			}
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := engine.QueryDNSContext(ctx, "timeout.example.", dnsmessage.TypeA); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timed out query error = %v, want deadline exceeded", err)
		}
		engine.dev.mu.Lock()
		pending := len(engine.dev.pending)
		engine.dev.mu.Unlock()
		if pending != 0 {
			t.Fatalf("%d exchanges still pending after timeout", pending)
		}
	})
}

func TestWarpDNSWithoutTunnelAnswersServfail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := testWarpEngine(t)
		startWarpEngine(t, engine, api.TunnelDisconnected)

		response, err := engine.QueryDNS("example.com.", dnsmessage.TypeA)
		if err != nil {
			t.Fatal(err)
		}
		var msg dnsmessage.Message
		if err := msg.Unpack(response); err != nil {
			t.Fatal(err)
		}
		if !msg.Header.Response || msg.Header.RCode != dnsmessage.RCodeServerFailure {
			t.Fatalf("response = %+v, want SERVFAIL", msg.Header)
		}
		if len(msg.Questions) != 1 || msg.Questions[0].Name.String() != "example.com." {
			t.Fatalf("questions = %v, want the queried name", msg.Questions)
		}
		engine.dev.mu.Lock()
		pending := len(engine.dev.pending)
		engine.dev.mu.Unlock()
		if pending != 0 || len(engine.dev.egress) != 0 {
			t.Fatalf("tunnel down left %d pending exchanges and %d queued packets", pending, len(engine.dev.egress))
		}
	})
}
