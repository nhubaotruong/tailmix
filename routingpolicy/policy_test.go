package routingpolicy

import (
	"net/netip"
	"testing"

	"github.com/maisem/tailmix/profile"
	"github.com/maisem/tailmix/state"
	"tailscale.com/types/dnstype"
)

func TestExplicitIPBindingOverridesAcceptAllAtRuntime(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{
			{ID: "work-id", Name: "work", AcceptAllRoutes: true},
			{ID: "lab-id", Name: "lab"},
		},
		IPRouteBindings: []state.IPRouteBinding{{
			Prefix: netip.MustParsePrefix("10.20.0.0/16"), ProfileID: "lab-id",
		}},
	}
	statuses := []profile.Status{
		{ProfileID: "work-id", AvailableRoutes: []profile.RouteStatus{{
			Prefix: netip.MustParsePrefix("10.20.0.0/16"), PrimaryRouter: "work-router",
		}}},
		{ProfileID: "lab-id", AvailableRoutes: []profile.RouteStatus{{
			Prefix: netip.MustParsePrefix("10.0.0.0/8"), PrimaryRouter: "lab-router",
		}}},
	}
	plan := BuildIP(st, statuses)
	if len(plan.Exact) != 1 || !plan.Exact[0].Active || plan.Exact[0].ProfileID != "lab-id" {
		t.Fatalf("exact route = %+v", plan.Exact)
	}
	if len(plan.Imported) != 1 || plan.Imported[0].Active ||
		plan.Imported[0].Reason != "explicit_override" ||
		plan.Imported[0].OverriddenBy != netip.MustParsePrefix("10.20.0.0/16") {
		t.Fatalf("imported route = %+v", plan.Imported)
	}
	if got := plan.Resource.Imported[0].State; got != "overridden" {
		t.Fatalf("runtime state = %q, want overridden", got)
	}
	if got := plan.Resource.Imported[0].OverrideProfileName; got != "lab" {
		t.Fatalf("override winner = %q, want lab", got)
	}
}

func TestWaitingExplicitIPBindingIsFailClosedOverride(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{
			{ID: "work-id", Name: "work", AcceptAllRoutes: true},
			{ID: "lab-id", Name: "lab"},
		},
		IPRouteBindings: []state.IPRouteBinding{{
			Prefix: netip.MustParsePrefix("10.20.0.0/16"), ProfileID: "lab-id",
		}},
	}
	plan := BuildIP(st, []profile.Status{
		{ProfileID: "work-id", AvailableRoutes: []profile.RouteStatus{{
			Prefix: netip.MustParsePrefix("10.20.0.0/16"),
		}}},
		{ProfileID: "lab-id"},
	})
	if plan.Exact[0].Active || plan.Exact[0].Reason != "route_not_advertised" {
		t.Fatalf("exact route = %+v", plan.Exact[0])
	}
	if plan.Imported[0].Active || plan.Imported[0].Reason != "explicit_override" {
		t.Fatalf("waiting binding did not override import: %+v", plan.Imported[0])
	}
}

func TestExplicitDNSBindingOverridesAcceptAllAndAutomatic(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{
			{ID: "work-id", Name: "work", AcceptAllDNSRoutes: true},
			{ID: "lab-id", Name: "lab"},
		},
		DNSRouteBindings: []state.DNSRouteBinding{{
			Domain: "DEV.Corp.Example.", ProfileID: "lab-id",
		}},
	}
	statuses := []profile.Status{
		{ProfileID: "work-id", MagicDNSSuffix: "corp.example", DNSRoutes: []profile.DNSRouteStatus{{
			Domain: "corp.example", Source: "magicdns",
		}}},
		{ProfileID: "lab-id", DNSRoutes: []profile.DNSRouteStatus{{
			Domain: "corp.example", Source: "split-dns",
		}}},
	}
	plan := BuildDNS(st, statuses)
	resolved, ok := plan.Resolve("host.dev.corp.example")
	if !ok || !resolved.Active || resolved.ProfileID != "lab-id" || resolved.Policy != "bound" {
		t.Fatalf("resolved route = %+v, %v", resolved, ok)
	}
	if len(plan.Imported) != 1 || !plan.Imported[0].Active ||
		plan.Imported[0].Reason != "partially_overridden" {
		t.Fatalf("imported route = %+v", plan.Imported)
	}
	if len(plan.Automatic) != 1 || plan.Automatic[0].Active ||
		plan.Automatic[0].Reason != "policy_override" {
		t.Fatalf("automatic route = %+v", plan.Automatic)
	}
}

func TestExplicitDNSDefaultOverridesAndSurvivesExitNodeDefault(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{
			{ID: "work-id", Name: "work"},
			{ID: "dns-id", Name: "dns"},
		},
		DNSRouteBindings: []state.DNSRouteBinding{{
			Domain: ".", ProfileID: "dns-id",
		}},
		ExitNode: &state.ExitNode{
			ProfileID: "work-id",
			NodeID:    "exit-node",
			PeerIP:    netip.MustParseAddr("100.64.0.20"),
		},
	}
	statuses := []profile.Status{
		{ProfileID: "work-id", BackendState: "Running", ExitNodeID: "exit-node"},
		{ProfileID: "dns-id", BackendState: "Running", DNSRoutes: []profile.DNSRouteStatus{{
			Domain: ".", Source: "default", Resolvers: []*dnstype.Resolver{{Addr: "9.9.9.9"}},
		}}},
	}

	plan := BuildDNS(st, statuses)
	resolved, ok := plan.Resolve("example.com")
	if !ok || !resolved.Active || resolved.ProfileID != "dns-id" || resolved.Policy != "bound" {
		t.Fatalf("resolved route with exit node = %+v, %v", resolved, ok)
	}
	if len(plan.Automatic) != 1 ||
		plan.Automatic[0].Source != "exit-node" ||
		plan.Automatic[0].Active ||
		plan.Automatic[0].Reason != "policy_override" {
		t.Fatalf("automatic exit-node route = %+v", plan.Automatic)
	}

	st.ExitNode = nil
	plan = BuildDNS(st, statuses)
	resolved, ok = plan.Resolve("example.com")
	if !ok || !resolved.Active || resolved.ProfileID != "dns-id" || resolved.Policy != "bound" {
		t.Fatalf("resolved route after clearing exit node = %+v, %v", resolved, ok)
	}
	if len(plan.Automatic) != 0 {
		t.Fatalf("automatic routes after clearing exit node = %+v", plan.Automatic)
	}
}

func TestExplicitDNSDefaultFallsBackToAutomaticMagicDNS(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{
			{ID: "work-id", Name: "work"},
			{ID: "home-id", Name: "home"},
		},
		DNSRouteBindings: []state.DNSRouteBinding{{
			Domain: ".", ProfileID: "work-id",
		}},
		SearchDomains: []string{"home.example"},
	}
	statuses := []profile.Status{
		{
			ProfileID: "work-id",
			DNSRoutes: []profile.DNSRouteStatus{{
				Domain: ".", Source: "default", Resolvers: []*dnstype.Resolver{{Addr: "9.9.9.9"}},
			}},
		},
		{ProfileID: "home-id", MagicDNSSuffix: "home.example"},
	}

	plan := BuildDNS(st, statuses)
	root, ok := plan.Resolve("example.com")
	if !ok || !root.Active || root.ProfileID != "work-id" || root.Domain != "." {
		t.Fatalf("default DNS route = %+v, %v", root, ok)
	}
	magic, ok := plan.Resolve("peer.home.example")
	if !ok || !magic.Active || magic.ProfileID != "home-id" || magic.Domain != "home.example" {
		t.Fatalf("MagicDNS route = %+v, %v", magic, ok)
	}
	if len(plan.Automatic) != 1 || !plan.Automatic[0].Active || plan.Automatic[0].Reason != "" {
		t.Fatalf("automatic MagicDNS routes = %+v", plan.Automatic)
	}
	if len(plan.Search.Installed) != 1 || plan.Search.Installed[0].Domain != "home.example" {
		t.Fatalf("installed search domains = %+v", plan.Search.Installed)
	}
}

func TestExitNodeAddsAutomaticEffectiveProfileDNSDefault(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{{ID: "work-id", Name: "work"}},
		ExitNode: &state.ExitNode{
			ProfileID: "work-id",
			NodeID:    "exit-node",
			PeerIP:    netip.MustParseAddr("100.64.0.20"),
		},
	}
	status := profile.Status{
		ProfileID: "work-id", BackendState: "Running", ExitNodeID: "exit-node",
	}
	plan := BuildDNS(st, []profile.Status{status})
	resolved, ok := plan.Resolve("example.com")
	if !ok || !resolved.Active ||
		resolved.ProfileID != "work-id" ||
		resolved.Policy != "automatic" ||
		resolved.Source != "exit-node" ||
		!resolved.ProfileDNS {
		t.Fatalf("automatic exit-node DNS route = %+v, %v", resolved, ok)
	}

	status.ExitNodeID = ""
	plan = BuildDNS(st, []profile.Status{status})
	if _, ok := plan.Resolve("example.com"); ok {
		t.Fatalf("exit-node DNS route installed before preference became active: %+v", plan.Automatic)
	}
}

func TestDNSContainsUsesTailscaleDNSNameRules(t *testing.T) {
	if !DNSContains("_service._tcp.example.com.", "Node._Service._TCP.Example.COM") {
		t.Fatal("expected dnsname-valid service labels to match case-insensitively")
	}
	if DNSContains("bad..example.com", "node.bad.example.com") {
		t.Fatal("invalid DNS suffix unexpectedly matched")
	}
}

func TestAcceptAllDoesNotImportDirectTailscaleAddressSpace(t *testing.T) {
	st := state.State{Profiles: []state.Profile{{ID: "work-id", Name: "work", AcceptAllRoutes: true}}}
	plan := BuildIP(st, []profile.Status{{
		ProfileID: "work-id",
		AvailableRoutes: []profile.RouteStatus{{
			Prefix: netip.MustParsePrefix("100.64.0.0/10"),
		}},
	}})
	if len(plan.Imported) != 1 || plan.Imported[0].Active || plan.Imported[0].Reason != "host_route_conflict" {
		t.Fatalf("reserved import = %+v", plan.Imported)
	}
}

func TestBuildDNSListsDetectedSearchDomainsByProfile(t *testing.T) {
	st := state.State{Profiles: []state.Profile{
		{ID: "work-id", Name: "work"},
		{ID: "lab-id", Name: "lab"},
	}}
	plan := BuildDNS(st, []profile.Status{
		{ProfileID: "work-id", SearchDomains: []string{"Corp.Example.", "corp.example", "."}},
		{ProfileID: "lab-id", SearchDomains: []string{"Lab.Example."}},
	})
	if len(plan.Search.Available) != 2 {
		t.Fatalf("available search domains = %+v", plan.Search.Available)
	}
	if got := plan.Search.Available[0]; got.Domain != "corp.example" || got.ProfileName != "work" {
		t.Fatalf("first available search domain = %+v", got)
	}
	if got := plan.Search.Available[1]; got.Domain != "lab.example" || got.ProfileName != "lab" {
		t.Fatalf("second available search domain = %+v", got)
	}
}

func TestWarpExitNodeOwnsRootDNSInEveryState(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{{ID: "warp-id", Name: "warp", Kind: state.ProfileKindWarp}},
		ExitNode: &state.ExitNode{ProfileID: "warp-id"},
	}
	for name, statuses := range map[string][]profile.Status{
		"running": {{ProfileID: "warp-id", Kind: state.ProfileKindWarp, BackendState: "Running", ExitNodeID: "registration"}},
		"disconnected": {{
			ProfileID: "warp-id", Kind: state.ProfileKindWarp, BackendState: "Unavailable", ExitNodeID: "registration",
		}},
		"stopped": nil,
	} {
		plan := BuildDNS(st, statuses)
		resolved, ok := plan.Resolve("example.com")
		if !ok || !resolved.Active || resolved.ProfileID != "warp-id" ||
			resolved.Source != "exit-node" || !resolved.ProfileDNS {
			t.Fatalf("%s: warp root DNS route = %+v, %v", name, resolved, ok)
		}
	}
}

func TestWarpRootDoesNotOutrankTailnetSuffix(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{
			{ID: "corp-id", Name: "corp"},
			{ID: "warp-id", Name: "warp", Kind: state.ProfileKindWarp},
		},
		ExitNode: &state.ExitNode{ProfileID: "warp-id"},
	}
	plan := BuildDNS(st, []profile.Status{
		{ProfileID: "corp-id", BackendState: "Running", MagicDNSSuffix: "corp.example"},
		{ProfileID: "warp-id", Kind: state.ProfileKindWarp, BackendState: "Running", ExitNodeID: "registration"},
	})
	if got, ok := plan.Resolve("host.corp.example"); !ok || got.ProfileID != "corp-id" {
		t.Fatalf("corp.example resolved to %+v, %v", got, ok)
	}
	if got, ok := plan.Resolve("example.com"); !ok || got.ProfileID != "warp-id" || !got.ProfileDNS {
		t.Fatalf("example.com resolved to %+v, %v", got, ok)
	}
}

func TestWarpDefaultRouteCoversExplicitBinding(t *testing.T) {
	st := state.State{
		Profiles: []state.Profile{
			{ID: "warp-id", Name: "warp", Kind: state.ProfileKindWarp},
			{ID: "corp-id", Name: "corp"},
		},
		IPRouteBindings: []state.IPRouteBinding{
			{ProfileID: "warp-id", Prefix: netip.MustParsePrefix("0.0.0.0/0")},
			{ProfileID: "warp-id", Prefix: netip.MustParsePrefix("::/0")},
			{ProfileID: "corp-id", Prefix: netip.MustParsePrefix("0.0.0.0/0")},
		},
	}
	warpDefault := profile.RouteStatus{Prefix: netip.MustParsePrefix("0.0.0.0/0")}
	warpDefault6 := profile.RouteStatus{Prefix: netip.MustParsePrefix("::/0")}
	plan := BuildIP(st, []profile.Status{{
		ProfileID: "warp-id", Kind: state.ProfileKindWarp, BackendState: "Running",
		SelfIPs:         []netip.Addr{netip.MustParseAddr("172.16.0.2")},
		AvailableRoutes: []profile.RouteStatus{warpDefault, warpDefault6},
	}, {
		ProfileID: "corp-id", BackendState: "Running",
		SelfIPs:         []netip.Addr{netip.MustParseAddr("100.64.0.10")},
		AvailableRoutes: []profile.RouteStatus{{Prefix: netip.MustParsePrefix("0.0.0.0/0")}},
	}})
	byProfile := map[string]IPEntry{}
	for _, entry := range plan.Exact {
		byProfile[entry.ProfileID+"/"+entry.Prefix.String()] = entry
	}
	if got := byProfile["warp-id/0.0.0.0/0"]; !got.Active || got.CoveringRoute != warpDefault.Prefix {
		t.Fatalf("warp IPv4 default binding = %+v", got)
	}
	if got := byProfile["warp-id/::/0"]; got.Active || got.Reason != "route_not_advertised" {
		t.Fatalf("warp IPv6 default binding without an IPv6 tunnel = %+v", got)
	}
	if got := byProfile["corp-id/0.0.0.0/0"]; got.Active || got.Reason != "host_route_conflict" {
		t.Fatalf("tailnet IPv4 default binding = %+v", got)
	}
}

func TestWarpAdvertisementIsNotAnAcceptAllImport(t *testing.T) {
	st := state.State{Profiles: []state.Profile{{
		ID: "warp-id", Name: "warp", Kind: state.ProfileKindWarp, AcceptAllRoutes: true,
	}}}
	plan := BuildIP(st, []profile.Status{{
		ProfileID: "warp-id", Kind: state.ProfileKindWarp, BackendState: "Running",
		SelfIPs: []netip.Addr{netip.MustParseAddr("172.16.0.2")},
		AvailableRoutes: []profile.RouteStatus{
			{Prefix: netip.MustParsePrefix("0.0.0.0/0")},
			{Prefix: netip.MustParsePrefix("::/0")},
		},
	}})
	if len(plan.Imported) != 0 {
		t.Fatalf("warp default became an accept-all import: %+v", plan.Imported)
	}
	if len(plan.Resource.Available) != 0 {
		t.Fatalf("warp default listed as a bindable route: %+v", plan.Resource.Available)
	}
}

func TestAcceptAllDuplicateRoutesWithSameResolversAreNotAmbiguous(t *testing.T) {
	st := state.State{Profiles: []state.Profile{
		{ID: "personal-id", Name: "personal", AcceptAllDNSRoutes: true},
		{ID: "work-id", Name: "work", AcceptAllDNSRoutes: true},
	}}
	statuses := []profile.Status{
		{ProfileID: "personal-id", DNSRoutes: []profile.DNSRouteStatus{{
			Domain:    "ts.net",
			Source:    "split-dns",
			Resolvers: []*dnstype.Resolver{{Addr: "199.247.155.53"}, {Addr: "10.16.0.2"}},
		}}},
		{ProfileID: "work-id", DNSRoutes: []profile.DNSRouteStatus{{
			Domain:    "ts.net",
			Source:    "split-dns",
			Resolvers: []*dnstype.Resolver{{Addr: "10.16.0.2"}, {Addr: "199.247.155.53"}},
		}}},
	}
	plan := BuildDNS(st, statuses)
	if len(plan.Imported) != 1 || !plan.Imported[0].Active || plan.Imported[0].ProfileID != "personal-id" {
		t.Fatalf("imported route = %+v", plan.Imported)
	}
	if got := plan.Resource.Imported[0].State; got != "installed" {
		t.Fatalf("runtime state = %q, want installed", got)
	}
}

func TestAcceptAllDuplicateRoutesWithDifferentResolversStayAmbiguous(t *testing.T) {
	st := state.State{Profiles: []state.Profile{
		{ID: "personal-id", Name: "personal", AcceptAllDNSRoutes: true},
		{ID: "work-id", Name: "work", AcceptAllDNSRoutes: true},
	}}
	statuses := []profile.Status{
		{ProfileID: "personal-id", DNSRoutes: []profile.DNSRouteStatus{{
			Domain:    "corp.example",
			Source:    "split-dns",
			Resolvers: []*dnstype.Resolver{{Addr: "10.16.0.2"}},
		}}},
		{ProfileID: "work-id", DNSRoutes: []profile.DNSRouteStatus{{
			Domain:    "corp.example",
			Source:    "split-dns",
			Resolvers: []*dnstype.Resolver{{Addr: "10.99.0.2"}},
		}}},
	}
	plan := BuildDNS(st, statuses)
	if len(plan.Imported) != 1 || plan.Imported[0].Active ||
		plan.Imported[0].Reason != "ambiguous_route" {
		t.Fatalf("imported route = %+v", plan.Imported)
	}
}

func TestAcceptAllDuplicateRoutesPreferMagicDNS(t *testing.T) {
	st := state.State{Profiles: []state.Profile{
		{ID: "personal-id", Name: "personal", AcceptAllDNSRoutes: true},
		{ID: "work-id", Name: "work", AcceptAllDNSRoutes: true},
	}}
	statuses := []profile.Status{
		{ProfileID: "personal-id", DNSRoutes: []profile.DNSRouteStatus{{
			Domain:    "corp.example",
			Source:    "split-dns",
			Resolvers: []*dnstype.Resolver{{Addr: "10.99.0.2"}},
		}}},
		{ProfileID: "work-id", DNSRoutes: []profile.DNSRouteStatus{{
			Domain: "corp.example",
			Source: "magicdns",
		}}},
	}
	plan := BuildDNS(st, statuses)
	if len(plan.Imported) != 1 || !plan.Imported[0].Active ||
		plan.Imported[0].ProfileID != "work-id" || plan.Imported[0].Source != "magicdns" {
		t.Fatalf("imported route = %+v", plan.Imported)
	}
}
