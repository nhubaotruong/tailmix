package main

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maisem/tailmix/controlapi"
)

func TestWarpRegisterRefusesWithoutTermsConsent(t *testing.T) {
	client := &fakeManagementClient{}
	var stdout, stderr bytes.Buffer
	deps := testDependencies(client, &stdout, &stderr, nil)
	deps.stdin = strings.NewReader("no\n")

	code := runWithDependencies(context.Background(), []string{"warp", "register", "cloudflare"}, deps)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if client.warpCalls != 0 {
		t.Fatalf("apply calls = %d, want 0", client.warpCalls)
	}
	if !strings.Contains(stderr.String(), "Terms of Service") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestWarpRegisterStoresRegistrationRequest(t *testing.T) {
	client := &fakeManagementClient{warpProfile: controlapi.WarpProfile{
		Name: "cloudflare", Kind: "warp", Transport: "http2",
		Addresses: []string{"172.16.0.2"},
	}}
	var stdout, stderr bytes.Buffer
	deps := testDependencies(client, &stdout, &stderr, nil)
	deps.stdin = strings.NewReader("yes\n")

	code := runWithDependencies(context.Background(),
		[]string{"warp", "register", "cloudflare", "--dns", "9.9.9.9", "--transport", "http2"}, deps)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	request := client.warpRequest
	if request.Name != "cloudflare" || request.Register == nil || request.Register.JWT != "" {
		t.Fatalf("request = %+v", request)
	}
	if request.DNS != "9.9.9.9" || !request.UseHTTP2 {
		t.Fatalf("request = %+v", request)
	}
	for _, want := range []string{"cloudflare", "warp", "http2", "ADDRESSES:", "172.16.0.2"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output = %q, missing %q", stdout.String(), want)
		}
	}
	if strings.Contains(stdout.String(), "9.9.9.9") {
		t.Fatalf("output leaked the resolver: %q", stdout.String())
	}
}

func TestWarpRegisterReadsTeamTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team.jwt")
	if err := os.WriteFile(path, []byte("team-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeManagementClient{}
	var stdout, stderr bytes.Buffer
	deps := testDependencies(client, &stdout, &stderr, nil)
	deps.stdin = strings.NewReader("y\n")

	code := runWithDependencies(context.Background(), []string{"warp", "register", "cloudflare", "--jwt-file", path}, deps)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if client.warpRequest.Register == nil || client.warpRequest.Register.JWT != "team-token" {
		t.Fatalf("request = %+v", client.warpRequest)
	}
	if strings.Contains(stdout.String()+stderr.String(), "team-token") {
		t.Fatalf("output leaked the team token: %q %q", stdout.String(), stderr.String())
	}
}

func TestWarpRegisterImportsConfigWithoutConsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"private_key":"cHJpdmF0ZQ==","endpoint_v4":"162.159.198.1","endpoint_v6":"2606:4700:d0::a29f:c001",` +
		`"endpoint_h2_v4":"162.159.198.2","endpoint_h2_v6":"","endpoint_pub_key":"-----BEGIN PUBLIC KEY-----",` +
		`"id":"device-id","access_token":"access","ipv4":"172.16.0.2","ipv6":"2606:4700:110:8a37::1","use_http2":true}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeManagementClient{}
	var stdout, stderr bytes.Buffer
	deps := testDependencies(client, &stdout, &stderr, nil)
	deps.stdin = strings.NewReader("")

	code := runWithDependencies(context.Background(), []string{"warp", "register", "cloudflare", "--config", path}, deps)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	request := client.warpRequest
	if request.Config == nil || request.Secrets == nil {
		t.Fatalf("request = %+v", request)
	}
	if request.Config.IPv4 != netip.MustParseAddr("172.16.0.2") ||
		request.Config.IPv6 != netip.MustParseAddr("2606:4700:110:8a37::1") {
		t.Fatalf("addresses = %v %v", request.Config.IPv4, request.Config.IPv6)
	}
	if !request.Config.UseHTTP2 || !request.UseHTTP2 {
		t.Fatalf("transport = %+v", request)
	}
	if request.Secrets.RegistrationID != "device-id" || request.Secrets.PrivateKey == "" {
		t.Fatalf("secrets = %+v", request.Secrets)
	}
	if !strings.Contains(stderr.String(), "Delete "+path) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "Terms of Service") {
		t.Fatalf("import prompted for consent: %q", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "access") {
		t.Fatalf("output leaked a secret: %q %q", stdout.String(), stderr.String())
	}
}

func TestWarpRegisterReadsTeamTokenFromStdin(t *testing.T) {
	client := &fakeManagementClient{}
	var stdout, stderr bytes.Buffer
	deps := testDependencies(client, &stdout, &stderr, nil)
	deps.stdin = strings.NewReader("team-token\ny\n")

	code := runWithDependencies(context.Background(), []string{"warp", "register", "cloudflare", "--jwt", "-"}, deps)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if client.warpRequest.Register == nil || client.warpRequest.Register.JWT != "team-token" {
		t.Fatalf("request = %+v", client.warpRequest)
	}
	if strings.Contains(stdout.String()+stderr.String(), "team-token") {
		t.Fatalf("output leaked the team token: %q %q", stdout.String(), stderr.String())
	}
}

func TestWarpRegisterRejectsTeamTokenInStdinWithoutConsent(t *testing.T) {
	client := &fakeManagementClient{}
	var stdout, stderr bytes.Buffer
	deps := testDependencies(client, &stdout, &stderr, nil)
	deps.stdin = strings.NewReader("team-token\nn\n")

	code := runWithDependencies(context.Background(), []string{"warp", "register", "cloudflare", "--jwt", "-"}, deps)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if client.warpCalls != 0 {
		t.Fatalf("apply calls = %d, want 0", client.warpCalls)
	}
}

func TestWarpRegisterRejectsInlineTeamToken(t *testing.T) {
	client := &fakeManagementClient{}
	var stdout, stderr bytes.Buffer
	code := runWithDependencies(context.Background(),
		[]string{"warp", "register", "cloudflare", "--jwt", "team-token"},
		testDependencies(client, &stdout, &stderr, nil))
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if client.warpCalls != 0 {
		t.Fatalf("apply calls = %d, want 0", client.warpCalls)
	}
}

func TestWarpRegisterRejectsTwoCredentialSources(t *testing.T) {
	client := &fakeManagementClient{}
	var stdout, stderr bytes.Buffer
	code := runWithDependencies(context.Background(),
		[]string{"warp", "register", "cloudflare", "--jwt-file", "team.jwt", "--config", "config.json"},
		testDependencies(client, &stdout, &stderr, nil))
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if client.warpCalls != 0 {
		t.Fatalf("apply calls = %d, want 0", client.warpCalls)
	}
}
