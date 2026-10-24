package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maisem/tailmix/controlapi"
	tailmixdns "github.com/maisem/tailmix/dns"
	"github.com/maisem/tailmix/hosttun"
	tailmixprofile "github.com/maisem/tailmix/profile"
	"github.com/maisem/tailmix/profilesocket"
	"github.com/maisem/tailmix/socksproxy"
	"github.com/maisem/tailmix/state"
	"github.com/maisem/tailmix/warpcfg"
	"github.com/maisem/tailmix/wireguardcfg"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/net/dns/dnsmessage"
)

type lifecycleEngine struct {
	status     tailmixprofile.Status
	closeErr   error
	closeCalls int
}

func (e *lifecycleEngine) Start(context.Context) error { return nil }
func (e *lifecycleEngine) Close() error {
	e.closeCalls++
	return e.closeErr
}
func (e *lifecycleEngine) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, net.ErrClosed
}
func (e *lifecycleEngine) Status(context.Context) (tailmixprofile.Status, error) {
	return e.status, nil
}
func (e *lifecycleEngine) WatchUpdates(context.Context, func()) error { return nil }
func (e *lifecycleEngine) SetRouteAll(_ context.Context, enabled bool) error {
	e.status.RouteAll = enabled
	return nil
}

func newLifecycleTestSupervisor(t *testing.T, st state.State) *supervisor {
	t.Helper()
	dir := t.TempDir()
	store := state.NewJSONStore(filepath.Join(dir, "state.json"))
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	router, err := socksproxy.NewRouterWithRoutes(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newSupervisor(store, st, nil, daemonConfig{
		Mode: "socks", SocketDir: dir, Stderr: io.Discard,
	})
	s.ctx = ctx
	s.cancel = cancel
	s.profileAPIs = newProfileAPIGroup(ctx, dir, io.Discard)
	s.socksRouter = socksproxy.NewDynamicRouter(router)
	return s
}

func lifecycleTestState() state.State {
	return state.State{
		SyntheticPool:   "100.127.0.0/24",
		SyntheticPoolV6: "fd6d:6e65:7400::/56",
		Profiles: []state.Profile{
			{ID: "p_work", Name: "work", StateDir: "profiles/work"},
			{ID: "p_home", Name: "home", StateDir: "profiles/home", Disabled: true},
			{ID: "p_old", Name: "old", StateDir: "profiles/old", Removed: true},
		},
	}
}

func TestSupervisorRunHonorsPersistedDownState(t *testing.T) {
	st := lifecycleTestState()
	st.Down = true
	dir := t.TempDir()
	store := state.NewJSONStore(filepath.Join(dir, "state.json"))
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	s := newSupervisor(store, st, nil, daemonConfig{
		Mode: "socks", SOCKSAddr: "127.0.0.1:0", SocketDir: dir, Stderr: io.Discard,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			cancel()
			<-done
		}
	})

	controlPath := profilesocket.ControlPath(dir)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(controlPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket %s was not created", controlPath)
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, err := controlapi.NewClient(dir).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "down" || len(status.Profiles) != 2 {
		t.Fatalf("status = %+v", status)
	}
	for _, profile := range status.Profiles {
		want := "down"
		if profile.Name == "home" {
			want = "disabled"
		}
		if profile.RuntimeState != want {
			t.Fatalf("profile %q runtime = %q, want %q", profile.Name, profile.RuntimeState, want)
		}
	}

	cancel()
	runErr := <-done
	finished = true
	if runErr != nil {
		t.Fatal(runErr)
	}
}

func TestDaemonDownStopsRuntimesAndPersists(t *testing.T) {
	s := newLifecycleTestSupervisor(t, lifecycleTestState())
	work := &lifecycleEngine{status: tailmixprofile.Status{ProfileID: "p_work", BackendState: "Running"}}
	home := &lifecycleEngine{status: tailmixprofile.Status{ProfileID: "p_home", BackendState: "Running"}}
	s.runtimes["p_work"] = &managedProfile{runtime: runtimeProfile{Engine: work}, cancel: func() {}}
	s.runtimes["p_home"] = &managedProfile{runtime: runtimeProfile{Engine: home}, cancel: func() {}}

	got, err := s.SetDaemonUp(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "down" || !s.st.Down || len(s.runtimes) != 0 {
		t.Fatalf("state = %+v, persisted down = %v, runtimes = %d", got, s.st.Down, len(s.runtimes))
	}
	if work.closeCalls != 1 || home.closeCalls != 1 {
		t.Fatalf("close calls: work=%d home=%d", work.closeCalls, home.closeCalls)
	}
	persisted, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Down || profileByID(&persisted, "p_work").Disabled || !profileByID(&persisted, "p_home").Disabled {
		t.Fatalf("persisted state = %+v", persisted)
	}

	status, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "down" {
		t.Fatalf("status state = %q", status.State)
	}
	states := map[string]string{}
	for _, profile := range status.Profiles {
		states[profile.Name] = profile.RuntimeState
	}
	removed, err := s.GetProfile(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	states[removed.Name] = removed.RuntimeState
	if states["work"] != "down" || states["home"] != "disabled" || states["old"] != "removed" {
		t.Fatalf("runtime states = %v", states)
	}

	got, err = s.SetDaemonUp(context.Background(), false)
	if err != nil || got.State != "down" {
		t.Fatalf("idempotent down = %+v, %v", got, err)
	}
	if work.closeCalls != 1 || home.closeCalls != 1 {
		t.Fatalf("idempotent down closed runtimes again: work=%d home=%d", work.closeCalls, home.closeCalls)
	}
}

func TestDaemonUpRestoresEnabledSet(t *testing.T) {
	st := lifecycleTestState()
	st.Down = true
	s := newLifecycleTestSupervisor(t, st)
	work := &lifecycleEngine{status: tailmixprofile.Status{ProfileID: "p_work", BackendState: "Running"}}
	home := &lifecycleEngine{status: tailmixprofile.Status{ProfileID: "p_home", BackendState: "Running"}}
	s.runtimes["p_work"] = &managedProfile{runtime: runtimeProfile{Engine: work}, cancel: func() {}}
	s.runtimes["p_home"] = &managedProfile{runtime: runtimeProfile{Engine: home}, cancel: func() {}}

	got, err := s.SetDaemonUp(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "up" || s.st.Down {
		t.Fatalf("state = %+v, persisted down = %v", got, s.st.Down)
	}
	if _, ok := s.runtimes["p_work"]; !ok {
		t.Fatal("enabled profile runtime was not retained")
	}
	if _, ok := s.runtimes["p_home"]; ok || home.closeCalls != 1 {
		t.Fatalf("disabled profile runtime was retained: present=%v closes=%d", ok, home.closeCalls)
	}
	persisted, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Down || profileByID(&persisted, "p_work").Disabled || !profileByID(&persisted, "p_home").Disabled {
		t.Fatalf("persisted state = %+v", persisted)
	}

	got, err = s.SetDaemonUp(context.Background(), true)
	if err != nil || got.State != "up" {
		t.Fatalf("idempotent up = %+v, %v", got, err)
	}
	if home.closeCalls != 1 {
		t.Fatalf("idempotent up closed disabled runtime again: %d", home.closeCalls)
	}
}

func TestDaemonDownPersistsAfterRuntimeStopFailure(t *testing.T) {
	s := newLifecycleTestSupervisor(t, lifecycleTestState())
	engine := &lifecycleEngine{
		status:   tailmixprofile.Status{ProfileID: "p_work", BackendState: "Running"},
		closeErr: errors.New("close failed"),
	}
	s.runtimes["p_work"] = &managedProfile{runtime: runtimeProfile{Engine: engine}, cancel: func() {}}

	got, err := s.SetDaemonUp(context.Background(), false)
	var apiErr *controlapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "runtime_stop_failed" {
		t.Fatalf("error = %v, want runtime_stop_failed", err)
	}
	if got.State != "down" || !s.st.Down || len(s.runtimes) != 0 {
		t.Fatalf("state = %+v, persisted down = %v, runtimes = %d", got, s.st.Down, len(s.runtimes))
	}
	persisted, loadErr := s.store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !persisted.Down {
		t.Fatal("runtime stop failure did not persist down state")
	}
}

func TestDaemonDownRejectsProfileMutations(t *testing.T) {
	s := &supervisor{st: state.State{Down: true}}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "add", run: func() error {
			_, err := s.AddProfile(context.Background(), controlapi.AddProfileRequest{Name: "work"})
			return err
		}},
		{name: "patch", run: func() error {
			_, err := s.PatchProfile(context.Background(), "work", controlapi.PatchProfileRequest{})
			return err
		}},
		{name: "enable", run: func() error { _, err := s.SetProfileEnabled(context.Background(), "work", true); return err }},
		{name: "restart", run: func() error { _, err := s.RestartProfile(context.Background(), "work"); return err }},
		{name: "remove", run: func() error { _, err := s.RemoveProfile(context.Background(), "work", false); return err }},
		{name: "wireguard apply", run: func() error {
			_, err := s.ApplyWireGuard(context.Background(), wireguardcfg.Config{}, wireguardcfg.Secrets{})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			var apiErr *controlapi.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "daemon_down" || apiErr.Message != "tailmix is down; run \"tailmix up\" first" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

type closeErrorHost struct {
	err error
}

func (h closeErrorHost) Device() tun.Device             { return nil }
func (h closeErrorHost) Name() string                   { return "utun42" }
func (h closeErrorHost) Configure(hosttun.Config) error { return nil }
func (h closeErrorHost) Close() error                   { return h.err }

func TestSupervisorCloseReportsHostCleanupError(t *testing.T) {
	var stderr bytes.Buffer
	s := &supervisor{
		cfg:  daemonConfig{Stderr: &stderr},
		host: closeErrorHost{err: errors.New("route delete failed")},
	}

	err := s.close()
	if err == nil || !strings.Contains(err.Error(), "close host TUN: route delete failed") {
		t.Fatalf("close error = %v, want host cleanup failure", err)
	}
	if !strings.Contains(stderr.String(), "close host TUN: route delete failed") {
		t.Fatalf("stderr = %q, want host cleanup failure", stderr.String())
	}
}

type lifecycleDNSService struct {
	tailmixdns.Service
	err error
}

func (s *lifecycleDNSService) ConfigureFull(tailmixdns.LiveConfig) error { return s.err }

type lifecycleDNSEngine struct {
	lifecycleEngine
	query func() ([]byte, error)
}

func (e *lifecycleDNSEngine) QueryDNS(string, dnsmessage.Type) ([]byte, error) {
	return e.query()
}

func TestSupervisorDNSReplacementDrainsAcrossMultipleUpdates(t *testing.T) {
	s := newLifecycleTestSupervisor(t, lifecycleTestState())
	service := &lifecycleDNSService{}
	s.dnsService = service
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	question := dnsmessage.Question{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	response, err := (&dnsmessage.Message{Header: dnsmessage.Header{Response: true}, Questions: []dnsmessage.Question{question}}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	engine := &lifecycleDNSEngine{query: func() ([]byte, error) {
		close(entered)
		<-release
		return response, nil
	}}
	s.runtimes["p_work"] = &managedProfile{runtime: runtimeProfile{Engine: engine}}
	configure := func() error {
		return s.configureDNSLocked(tailmixdns.LiveConfig{Routes: []tailmixdns.Route{{Suffix: ".", ProfileID: "p_work", ProfileDNS: true}}})
	}
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	old := s.dnsForwarders[0]
	defer old.Close()
	conn, err := net.Dial("udp", old.Resolver().Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	query, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{question}}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upstream query did not start")
	}
	// A failed configuration must leave the currently installed generation alone.
	service.err = errors.New("configure failed")
	if err := configure(); err == nil {
		t.Fatal("configuration unexpectedly succeeded")
	}
	if s.dnsForwarders[0] != old {
		t.Fatal("failed configure replaced the active forwarder")
	}
	service.err = nil
	for i := 0; i < 2; i++ {
		if err := configure(); err != nil {
			t.Fatal(err)
		}
		defer s.dnsForwarders[0].Close()
	}
	close(release)
	wire := make([]byte, 65535)
	n, err := conn.Read(wire)
	if err != nil {
		t.Fatalf("two replacements lost an in-flight query: %v", err)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(wire[:n]); err != nil {
		t.Fatal(err)
	}
	if msg.Header.ID != 42 || msg.Header.RCode != dnsmessage.RCodeSuccess || !msg.Header.Response {
		t.Fatalf("drained reply = %+v", msg)
	}
}

func newWarpTestSupervisor(t *testing.T, st state.State) *supervisor {
	t.Helper()
	dir := t.TempDir()
	store := state.NewJSONStore(filepath.Join(dir, "state.json"))
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newSupervisor(store, st, nil, daemonConfig{Mode: "tun", SocketDir: dir, Stderr: io.Discard})
	s.ctx = ctx
	s.cancel = cancel
	return s
}

func warpTestState(dir string) state.State {
	return state.State{
		SyntheticPool: "100.127.0.0/24",
		Profiles: []state.Profile{
			{ID: "p_work", Name: "work", StateDir: filepath.Join(dir, "work")},
			{
				ID: "p_warp", Name: "warp", Kind: state.ProfileKindWarp, StateDir: filepath.Join(dir, "warp"),
				Warp: &warpcfg.Config{
					EndpointV4: "162.159.198.2:4443",
					IPv4:       netip.MustParseAddr("172.16.0.2"),
					DNS:        netip.MustParseAddr("1.1.1.1"),
				},
			},
		},
	}
}

func warpTestProfile(t *testing.T, st state.State) state.Profile {
	t.Helper()
	for _, configured := range st.Profiles {
		if configured.Kind == state.ProfileKindWarp {
			return configured
		}
	}
	t.Fatal("test state has no WARP profile")
	return state.Profile{}
}

func startRunningTestProfile(t *testing.T, s *supervisor, profileID string) *lifecycleEngine {
	t.Helper()
	for _, configured := range s.st.Profiles {
		if configured.ID != profileID {
			continue
		}
		engine := &lifecycleEngine{status: tailmixprofile.Status{ProfileID: profileID, BackendState: "Running"}}
		s.runtimes[profileID] = &managedProfile{
			runtime: runtimeProfile{State: configured, Engine: engine},
			cancel:  func() {},
			status:  engine.status,
		}
		return engine
	}
	t.Fatalf("test state has no profile %q", profileID)
	return nil
}

func TestWarpProfileWithoutCredentialsReportsReRegistration(t *testing.T) {
	dir := t.TempDir()
	st := warpTestState(dir)
	s := newWarpTestSupervisor(t, st)
	startRunningTestProfile(t, s, "p_work")
	warp := warpTestProfile(t, st)

	err := s.startProfileLocked(warp, "", "")
	if !errors.Is(err, warpcfg.ErrCredentialsMissing) {
		t.Fatalf("start error = %v, want ErrCredentialsMissing", err)
	}
	s.lastErrors[warp.ID] = err.Error()

	projected := s.projectProfileLocked(warp)
	if projected.RuntimeState != "error" {
		t.Fatalf("warp runtime state = %q, want error", projected.RuntimeState)
	}
	if projected.BackendState != "NeedsReRegistration" {
		t.Fatalf("warp backend state = %q, want NeedsReRegistration", projected.BackendState)
	}
	if !strings.Contains(projected.LastError, "re-register") {
		t.Fatalf("warp last error = %q, want re-registration", projected.LastError)
	}
	if _, ok := s.runtimes["p_warp"]; ok {
		t.Fatal("warp profile opened a runtime without credentials")
	}
	for _, configured := range st.Profiles {
		if configured.ID == "p_work" && s.projectProfileLocked(configured).RuntimeState != "running" {
			t.Fatal("tailscale profile stopped when the WARP profile failed")
		}
	}
}

func TestWarpProfileReportsPlatformLimitOnDarwin(t *testing.T) {
	dir := t.TempDir()
	st := warpTestState(dir)
	s := newWarpTestSupervisor(t, st)
	startRunningTestProfile(t, s, "p_work")
	warp := warpTestProfile(t, st)
	if err := warpcfg.SaveSecrets(warp.StateDir, warpcfg.Secrets{PrivateKey: "key", RegistrationID: "id"}); err != nil {
		t.Fatal(err)
	}
	previous := warpPlatformSupported
	warpPlatformSupported = false
	t.Cleanup(func() { warpPlatformSupported = previous })

	err := s.startProfileLocked(warp, "", "")
	if err == nil || !strings.Contains(err.Error(), "Linux only") {
		t.Fatalf("start error = %v, want a platform limit", err)
	}
	s.lastErrors[warp.ID] = err.Error()

	projected := s.projectProfileLocked(warp)
	if projected.RuntimeState != "error" || !strings.Contains(projected.LastError, "Linux only") {
		t.Fatalf("warp projection = %+v", projected)
	}
	if _, ok := s.runtimes["p_warp"]; ok {
		t.Fatal("warp profile opened a runtime on an unsupported platform")
	}
	for _, configured := range st.Profiles {
		if configured.ID == "p_work" && s.projectProfileLocked(configured).RuntimeState != "running" {
			t.Fatal("tailscale profile stopped when the WARP profile failed")
		}
	}
}
