package tsnet

import (
	"net/netip"
	"testing"

	"tailscale.com/ipn"
)

func TestFlowHandlersDoNotInterceptNonLocalDestinations(t *testing.T) {
	server := &Server{}
	src := netip.MustParseAddrPort("100.64.0.9:1234")
	dst := netip.MustParseAddrPort("1.1.1.1:443")
	if _, intercept := server.getTCPHandlerForFlow(src, dst); intercept {
		t.Fatal("TCP flow to a non-local destination was intercepted, so netstack cannot forward it")
	}
	if _, intercept := server.getUDPHandlerForFlow(src, dst); intercept {
		t.Fatal("UDP flow to a non-local destination was intercepted, so netstack cannot forward it")
	}
}

func TestStartupPrefsPreserveNativeTailscaleSettings(t *testing.T) {
	saved := ipn.NewPrefs()
	saved.ControlURL = "https://headscale.example.com"
	saved.ShieldsUp = true
	saved.RunWebClient = true
	saved.AdvertiseTags = []string{"tag:native"}
	saved.Hostname = "old-hostname"
	saved.WantRunning = false

	server := &Server{hostname: "tailmix-work"}
	got := server.startupPrefs(saved.View())

	if got.ControlURL != saved.ControlURL {
		t.Fatalf("ControlURL = %q, want saved value %q", got.ControlURL, saved.ControlURL)
	}
	if !got.ShieldsUp {
		t.Fatal("ShieldsUp was not preserved")
	}
	if !got.RunWebClient {
		t.Fatal("RunWebClient was not preserved")
	}
	if len(got.AdvertiseTags) != 1 || got.AdvertiseTags[0] != "tag:native" {
		t.Fatalf("AdvertiseTags = %q, want saved value", got.AdvertiseTags)
	}
	if got.Hostname != "tailmix-work" {
		t.Fatalf("Hostname = %q, want Tailmix-owned hostname", got.Hostname)
	}
	if !got.WantRunning {
		t.Fatal("WantRunning = false, want true")
	}
}

func TestStartupPrefsExplicitControlURLWins(t *testing.T) {
	saved := ipn.NewPrefs()
	saved.ControlURL = "https://saved.example.com"

	server := &Server{
		hostname:   "tailmix-work",
		ControlURL: "https://configured.example.com",
	}
	got := server.startupPrefs(saved.View())

	if got.ControlURL != server.ControlURL {
		t.Fatalf("ControlURL = %q, want explicit value %q", got.ControlURL, server.ControlURL)
	}
}

func TestGetAuthKeyCanIgnoreEnvironment(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "global-key")
	t.Setenv("TS_AUTH_KEY", "global-key-alias")

	server := &Server{DisableAuthKeyEnv: true}
	if got := server.getAuthKey(); got != "" {
		t.Fatalf("getAuthKey() = %q, want global environment ignored", got)
	}

	server.AuthKey = "profile-key"
	if got := server.getAuthKey(); got != "profile-key" {
		t.Fatalf("getAuthKey() = %q, want explicit profile key", got)
	}
}

func TestGetAuthKeyUsesEnvironmentByDefault(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "global-key")

	if got := new(Server).getAuthKey(); got != "global-key" {
		t.Fatalf("getAuthKey() = %q, want default environment fallback", got)
	}
}
