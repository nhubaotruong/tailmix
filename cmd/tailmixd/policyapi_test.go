package main

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/maisem/tailmix/controlapi"
	tailmixprofile "github.com/maisem/tailmix/profile"
	"github.com/maisem/tailmix/state"
	"github.com/maisem/tailmix/warpcfg"
)

type exitPreferenceEngine struct {
	status       tailmixprofile.Status
	changes      []netip.Addr
	routeChanges []bool
}

func (e *exitPreferenceEngine) Start(context.Context) error { return nil }
func (e *exitPreferenceEngine) Close() error                { return nil }
func (e *exitPreferenceEngine) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, net.ErrClosed
}
func (e *exitPreferenceEngine) Status(context.Context) (tailmixprofile.Status, error) {
	return e.status, nil
}
func (e *exitPreferenceEngine) WatchUpdates(context.Context, func()) error { return nil }
func (e *exitPreferenceEngine) SetExitNodeIP(_ context.Context, peerIP netip.Addr) error {
	e.changes = append(e.changes, peerIP)
	e.status.ExitNodeID = ""
	for _, peer := range e.status.Peers {
		if slices.Contains(peer.TailscaleIPs, peerIP) {
			e.status.ExitNodeID = peer.NodeID
			break
		}
	}
	return nil
}
func (e *exitPreferenceEngine) SetRouteAll(_ context.Context, enabled bool) error {
	e.routeChanges = append(e.routeChanges, enabled)
	e.status.RouteAll = enabled
	return nil
}

func testPolicyState() state.State {
	return state.State{
		SyntheticPool:   "100.127.0.0/24",
		SyntheticPoolV6: "fd6d:6e65:7400::/56",
		Profiles: []state.Profile{
			{ID: "p_work", Name: "work"},
			{ID: "p_lab", Name: "lab"},
		},
	}
}

func TestApplyIPPatchPersistsStableProfileIDAndRequiresReplace(t *testing.T) {
	supervisor := &supervisor{}
	st := testPolicyState()
	prefix := netip.MustParsePrefix("10.20.3.4/16")
	err := supervisor.applyIPPatchLocked(&st, controlapi.PatchIPRoutesRequest{
		Bind: []controlapi.IPRouteMutation{{Prefix: prefix, ProfileName: "work"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.IPRouteBindings) != 1 ||
		st.IPRouteBindings[0].Prefix != netip.MustParsePrefix("10.20.0.0/16") ||
		st.IPRouteBindings[0].ProfileID != "p_work" {
		t.Fatalf("bindings = %+v", st.IPRouteBindings)
	}
	err = supervisor.applyIPPatchLocked(&st, controlapi.PatchIPRoutesRequest{
		Bind: []controlapi.IPRouteMutation{{Prefix: prefix, ProfileName: "lab"}},
	})
	if apiErr, ok := err.(*controlapi.Error); !ok || apiErr.Code != "route_binding_conflict" {
		t.Fatalf("conflict error = %#v", err)
	}
	err = supervisor.applyIPPatchLocked(&st, controlapi.PatchIPRoutesRequest{
		Replace: true,
		Bind:    []controlapi.IPRouteMutation{{Prefix: prefix, ProfileName: "lab"}},
	})
	if err != nil || st.IPRouteBindings[0].ProfileID != "p_lab" {
		t.Fatalf("replace error = %v, bindings = %+v", err, st.IPRouteBindings)
	}
}

func TestApplyDNSPatchUsesDNSNameCanonicalization(t *testing.T) {
	supervisor := &supervisor{}
	st := testPolicyState()
	err := supervisor.applyDNSPatchLocked(&st, controlapi.PatchDNSRoutesRequest{
		Bind: []controlapi.DNSRouteMutation{{
			Domain: "_Service._TCP.Corp.Example.", ProfileName: "work",
		}},
		AcceptAll: map[string]bool{"lab": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.DNSRouteBindings) != 1 ||
		st.DNSRouteBindings[0].Domain != "_service._tcp.corp.example" ||
		st.DNSRouteBindings[0].ProfileID != "p_work" {
		t.Fatalf("DNS bindings = %+v", st.DNSRouteBindings)
	}
	if !st.Profiles[0].AcceptAllDNSRoutes && !st.Profiles[1].AcceptAllDNSRoutes {
		t.Fatal("accept-all DNS policy was not set")
	}
	err = supervisor.applyDNSPatchLocked(&st, controlapi.PatchDNSRoutesRequest{
		Bind: []controlapi.DNSRouteMutation{{Domain: "bad..example", ProfileName: "work"}},
	})
	if apiErr, ok := err.(*controlapi.Error); !ok || apiErr.Code != "invalid_dns_name" {
		t.Fatalf("invalid DNS error = %#v", err)
	}
}

func TestSelectExitNodePeerAcceptsProfileScopedSelectors(t *testing.T) {
	exitIP := netip.MustParseAddr("100.64.0.20")
	status := tailmixprofile.Status{
		Alias: "work",
		Peers: []tailmixprofile.PeerStatus{{
			NodeID: "node-stable-id", DNSName: "gateway.work.ts.net",
			TailscaleIPs: []netip.Addr{exitIP}, ExitNodeOption: true,
		}},
	}
	for _, selector := range []string{"gateway", "gateway.work.ts.net.", "node-stable-id", exitIP.String()} {
		peer, err := selectExitNodePeer(status, selector)
		if err != nil {
			t.Fatalf("select %q: %v", selector, err)
		}
		if peer.NodeID != "node-stable-id" {
			t.Fatalf("select %q returned %+v", selector, peer)
		}
	}
	if _, err := selectExitNodePeer(status, "other"); err == nil {
		t.Fatal("unavailable exit node unexpectedly selected")
	}
}

func TestExitNodeResourceReportsSelectedState(t *testing.T) {
	exitIP := netip.MustParseAddr("100.64.0.20")
	s := &supervisor{st: testPolicyState()}
	s.st.ExitNode = &state.ExitNode{ProfileID: "p_work", NodeID: "node-id", PeerIP: exitIP}
	status := tailmixprofile.Status{
		ProfileID: "p_work", BackendState: "Running", ExitNodeID: "node-id",
		Peers: []tailmixprofile.PeerStatus{{
			NodeID: "node-id", DNSName: "gateway.work.ts.net",
			TailscaleIPs: []netip.Addr{exitIP}, Online: true, ExitNodeOption: true,
			Location: &tailmixprofile.PeerLocation{
				Country: "Canada", CountryCode: "CA", City: "Squamish", CityCode: "YSE", Priority: 10,
			},
		}},
	}
	result := s.exitNodesLocked([]tailmixprofile.Status{status})
	if result.Selected == nil || result.Selected.State != "installed" ||
		result.Selected.ProfileName != "work" || !result.Selected.Online {
		t.Fatalf("exit-node resource = %+v", result)
	}
	if len(result.Available) != 1 || result.Available[0].NodeID != "node-id" {
		t.Fatalf("available exit nodes = %+v", result.Available)
	}
	if result.Available[0].Location == nil ||
		result.Available[0].Location.City != "Squamish" ||
		result.Available[0].Location.Priority != 10 ||
		result.Selected.Location == nil ||
		result.Selected.Location.CityCode != "YSE" {
		t.Fatalf("exit-node locations = available %+v, selected %+v", result.Available[0], result.Selected)
	}

	status.ExitNodeID = ""
	result = s.exitNodesLocked([]tailmixprofile.Status{status})
	if result.Selected == nil || result.Selected.State != "waiting" ||
		result.Selected.Reason != "exit_node_not_applied" {
		t.Fatalf("exit-node resource before preference applies = %+v", result.Selected)
	}
}

func TestStatusReportsSelectedPolicyWithoutAvailableChoices(t *testing.T) {
	exitIP := netip.MustParseAddr("100.64.0.20")
	prefix := netip.MustParsePrefix("10.20.0.0/16")
	engine := &exitPreferenceEngine{status: tailmixprofile.Status{
		ProfileID: "p_work", Alias: "work", BackendState: "Running",
		MagicDNSSuffix: "work.example", ExitNodeID: "exit-node",
		AvailableRoutes: []tailmixprofile.RouteStatus{{
			Prefix: prefix, PrimaryRouter: "subnet-router",
		}},
		Peers: []tailmixprofile.PeerStatus{{
			NodeID: "exit-node", DNSName: "gateway.work.example",
			TailscaleIPs: []netip.Addr{exitIP}, Online: true, ExitNodeOption: true,
		}},
	}}
	st := testPolicyState()
	st.Profiles = st.Profiles[:1]
	st.Profiles[0].AcceptAllRoutes = true
	st.SearchDomains = []string{"work.example"}
	st.ExitNode = &state.ExitNode{ProfileID: "p_work", NodeID: "exit-node", PeerIP: exitIP}
	s := &supervisor{
		ctx: context.Background(), st: st,
		runtimes: map[string]*managedProfile{
			"p_work": {
				runtime: runtimeProfile{State: st.Profiles[0], Engine: engine},
				status:  engine.status,
			},
		},
		lastErrors: map[string]string{},
	}

	got, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Name != "work" {
		t.Fatalf("profiles = %+v", got.Profiles)
	}
	if len(got.IPRoutes.Imported) != 1 || got.IPRoutes.Imported[0].Prefix != prefix ||
		len(got.IPRoutes.Available) != 0 {
		t.Fatalf("IP routes = %+v", got.IPRoutes)
	}
	if got.ExitNodes.Selected == nil || got.ExitNodes.Selected.State != "installed" ||
		len(got.ExitNodes.Available) != 0 {
		t.Fatalf("exit nodes = %+v", got.ExitNodes)
	}
	if len(got.DNSRoutes.Automatic) != 2 || len(got.DNSRoutes.Available) != 0 {
		t.Fatalf("DNS routes = %+v", got.DNSRoutes)
	}
	if len(got.SearchDomains.Installed) != 1 || len(got.SearchDomains.Available) != 0 {
		t.Fatalf("search domains = %+v", got.SearchDomains)
	}
}

func TestExitNodePreferenceReconciliationSelectsOnlyOneProfile(t *testing.T) {
	work := &exitPreferenceEngine{status: tailmixprofile.Status{
		ProfileID: "p_work", BackendState: "Running",
		Peers: []tailmixprofile.PeerStatus{{
			NodeID: "selected-node", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.20")},
		}},
	}}
	lab := &exitPreferenceEngine{status: tailmixprofile.Status{
		ProfileID: "p_lab", BackendState: "Running", ExitNodeID: "stale-node",
	}}
	s := &supervisor{
		ctx: context.Background(),
		st:  testPolicyState(),
		runtimes: map[string]*managedProfile{
			"p_work": {
				runtime: runtimeProfile{State: state.Profile{ID: "p_work"}, Engine: work},
				status:  work.status,
			},
			"p_lab": {
				runtime: runtimeProfile{State: state.Profile{ID: "p_lab"}, Engine: lab},
				status:  lab.status,
			},
		},
		lastErrors: map[string]string{},
	}
	s.st.ExitNode = &state.ExitNode{
		ProfileID: "p_work", NodeID: "selected-node",
		PeerIP: netip.MustParseAddr("100.64.0.20"),
	}
	if err := s.setExitNodePreferencesLocked([]tailmixprofile.Status{work.status, lab.status}); err != nil {
		t.Fatal(err)
	}
	if len(work.changes) != 1 || work.changes[0] != netip.MustParseAddr("100.64.0.20") {
		t.Fatalf("work preference changes = %v", work.changes)
	}
	if len(lab.changes) != 1 || lab.changes[0].IsValid() {
		t.Fatalf("lab preference changes = %v", lab.changes)
	}
}

func TestRouteAllIsAlwaysEnabled(t *testing.T) {
	work := &exitPreferenceEngine{status: tailmixprofile.Status{
		ProfileID: "p_work", BackendState: "Running",
	}}
	s := &supervisor{
		ctx: context.Background(),
		st:  testPolicyState(),
		runtimes: map[string]*managedProfile{
			"p_work": {
				runtime: runtimeProfile{State: state.Profile{ID: "p_work"}, Engine: work},
				status:  work.status,
			},
		},
		lastErrors: map[string]string{},
	}
	if err := s.setRoutePreferencesLocked(); err != nil {
		t.Fatal(err)
	}
	if len(work.routeChanges) != 1 || work.routeChanges[0] != true {
		t.Fatalf("initial route preference changes = %v, want [true]", work.routeChanges)
	}

	s.st.ExitNode = &state.ExitNode{
		ProfileID: "p_work", NodeID: "selected-node",
		PeerIP: netip.MustParseAddr("100.64.0.20"),
	}
	if err := s.setRoutePreferencesLocked(); err != nil {
		t.Fatal(err)
	}
	s.st.ExitNode = nil
	if err := s.setRoutePreferencesLocked(); err != nil {
		t.Fatal(err)
	}
	if len(work.routeChanges) != 1 {
		t.Fatalf("route preference changed with exit-node selection = %v, want [true]", work.routeChanges)
	}
}

func exitNodeTestState() state.State {
	st := testPolicyState()
	st.Profiles[0].StateDir = "profiles/work"
	st.Profiles = append(st.Profiles, state.Profile{
		ID: "p_warp", Name: "warp", Kind: state.ProfileKindWarp,
		StateDir: "profiles/warp", Warp: &warpcfg.Config{},
	})
	return st
}

func exitNodeTestProfile(t *testing.T, s *supervisor, id string) state.Profile {
	t.Helper()
	for _, configured := range s.st.Profiles {
		if configured.ID == id {
			return configured
		}
	}
	t.Fatalf("profile %s not found", id)
	return state.Profile{}
}

func TestSetExitNodeSelectsWarpProfileWithoutPeer(t *testing.T) {
	st := exitNodeTestState()
	s := newLifecycleTestSupervisor(t, st)
	work := exitNodeTestProfile(t, s, "p_work")
	warp := exitNodeTestProfile(t, s, "p_warp")
	workEngine := &exitPreferenceEngine{status: tailmixprofile.Status{
		ProfileID: "p_work", Alias: "work", BackendState: "Running",
		Peers: []tailmixprofile.PeerStatus{{
			NodeID: "node-work-exit", DNSName: "gateway.example.ts.net.",
			TailscaleIPs:   []netip.Addr{netip.MustParseAddr("100.64.0.20")},
			ExitNodeOption: true, Online: true,
		}},
	}}
	warpStatus := tailmixprofile.Status{
		ProfileID: "p_warp", Alias: "warp", Kind: "warp", BackendState: "Running",
		ExitNodeID: "registration-1",
		SelfIPs:    []netip.Addr{netip.MustParseAddr("172.16.0.2")},
	}
	s.runtimes[work.ID] = &managedProfile{
		runtime: runtimeProfile{State: work, Engine: workEngine},
		status:  workEngine.status,
	}
	s.runtimes[warp.ID] = &managedProfile{
		runtime: runtimeProfile{State: warp, Engine: &lifecycleEngine{status: warpStatus}},
		status:  warpStatus,
	}

	if _, err := s.SetExitNode(context.Background(), controlapi.SetExitNodeRequest{
		ProfileName: "work", Peer: "gateway",
	}); err != nil {
		t.Fatal(err)
	}
	if s.st.ExitNode == nil || s.st.ExitNode.ProfileID != "p_work" {
		t.Fatalf("tailnet selection = %+v", s.st.ExitNode)
	}

	nodes, err := s.SetExitNode(context.Background(), controlapi.SetExitNodeRequest{ProfileName: "warp"})
	if err != nil {
		t.Fatal(err)
	}
	want := state.ExitNode{
		ProfileID: "p_warp", NodeID: "registration-1",
		PeerIP: netip.MustParseAddr("172.16.0.2"),
	}
	if s.st.ExitNode == nil || *s.st.ExitNode != want {
		t.Fatalf("WARP selection = %+v, want %+v", s.st.ExitNode, want)
	}
	if nodes.Selected == nil || nodes.Selected.State != "installed" || !nodes.Selected.Online {
		t.Fatalf("selected exit node = %+v", nodes.Selected)
	}
	available := false
	for _, node := range nodes.Available {
		if node.ProfileID == "p_warp" && node.NodeID == "registration-1" &&
			slices.Equal(node.IPs, warpStatus.SelfIPs) && node.Location == nil {
			available = true
		}
	}
	if !available {
		t.Fatalf("WARP profile missing from available exit nodes: %+v", nodes.Available)
	}

	loaded, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExitNode == nil || *loaded.ExitNode != want {
		t.Fatalf("reloaded selection = %+v, want %+v", loaded.ExitNode, want)
	}

	if _, err := s.SetExitNode(context.Background(), controlapi.SetExitNodeRequest{
		ProfileName: "work", Peer: "gateway",
	}); err != nil {
		t.Fatal(err)
	}
	if s.st.ExitNode == nil || s.st.ExitNode.ProfileID != "p_work" {
		t.Fatalf("tailnet reselection = %+v", s.st.ExitNode)
	}
}

func TestSetExitNodeSelectsWarpProfileWithoutIdentity(t *testing.T) {
	st := exitNodeTestState()
	s := newLifecycleTestSupervisor(t, st)
	warp := exitNodeTestProfile(t, s, "p_warp")
	warpStatus := tailmixprofile.Status{
		ProfileID: "p_warp", Alias: "warp", Kind: "warp", BackendState: "Running",
	}
	s.runtimes[warp.ID] = &managedProfile{
		runtime: runtimeProfile{State: warp, Engine: &lifecycleEngine{status: warpStatus}},
		status:  warpStatus,
	}

	nodes, err := s.SetExitNode(context.Background(), controlapi.SetExitNodeRequest{ProfileName: "warp"})
	if err != nil {
		t.Fatal(err)
	}
	want := state.ExitNode{ProfileID: "p_warp"}
	if s.st.ExitNode == nil || *s.st.ExitNode != want {
		t.Fatalf("WARP selection = %+v, want %+v", s.st.ExitNode, want)
	}
	if nodes.Selected == nil || nodes.Selected.State != "installed" || nodes.Selected.NodeID != "" {
		t.Fatalf("selected exit node = %+v", nodes.Selected)
	}
}

func TestSetExitNodeRefusesDisabledWarpProfile(t *testing.T) {
	st := exitNodeTestState()
	for i := range st.Profiles {
		if st.Profiles[i].ID == "p_warp" {
			st.Profiles[i].Disabled = true
		}
	}
	s := newLifecycleTestSupervisor(t, st)

	_, err := s.SetExitNode(context.Background(), controlapi.SetExitNodeRequest{ProfileName: "warp"})
	apiErr, ok := err.(*controlapi.Error)
	if !ok || apiErr.Code != "profile_disabled" {
		t.Fatalf("error = %#v, want profile_disabled", err)
	}
}

func TestExitPreferenceSkipsReRegisteredWarpProfile(t *testing.T) {
	st := exitNodeTestState()
	st.ExitNode = &state.ExitNode{ProfileID: "p_warp", NodeID: "old-registration"}
	s := newLifecycleTestSupervisor(t, st)

	status := tailmixprofile.Status{
		ProfileID: "p_warp", Kind: state.ProfileKindWarp,
		BackendState: "Running", ExitNodeID: "new-registration",
	}
	s.runtimes = map[string]*managedProfile{
		"p_warp": {
			runtime: runtimeProfile{Engine: &lifecycleEngine{status: status}},
			status:  status,
		},
	}

	if err := s.setExitNodePreferencesLocked([]tailmixprofile.Status{status}); err != nil {
		t.Fatalf("exit preference for a re-registered WARP profile: %v", err)
	}
}
