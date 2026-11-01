package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/maisem/tailmix/controlapi"
	tailmixversion "github.com/maisem/tailmix/version"
)

func runJSON(t *testing.T, client *fakeManagementClient, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	deps := testDependencies(client, &stdout, &stderr, nil)
	deps.stdin = strings.NewReader(stdin)
	code := runWithDependencies(context.Background(), args, deps)
	return code, stdout.String(), stderr.String()
}

func decodeErrorRecord(t *testing.T, stderr string) errorRecord {
	t.Helper()
	var record errorRecord
	if err := json.Unmarshal([]byte(stderr), &record); err != nil {
		t.Fatalf("stderr = %q, not a single JSON record: %v", stderr, err)
	}
	return record
}

func TestJSONFailureEnvelopeReportsDaemonCode(t *testing.T) {
	client := &fakeManagementClient{statusErr: &controlapi.Error{Code: "permission_denied", Message: "tailmix management commands require root"}}
	code, stdout, stderr := runJSON(t, client, "", "status", "--json")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	record := decodeErrorRecord(t, stderr)
	if record.Code != "permission_denied" {
		t.Errorf("code = %q, want permission_denied", record.Code)
	}
	if record.Message != "tailmix management commands require root" {
		t.Errorf("message = %q", record.Message)
	}
}

func TestJSONFailureEnvelopeMapsUnreachableDaemon(t *testing.T) {
	client := &fakeManagementClient{statusErr: fmt.Errorf("connect to tailmix daemon: %w", net.ErrClosed)}
	code, _, stderr := runJSON(t, client, "", "status", "--json")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if got := decodeErrorRecord(t, stderr).Code; got != "daemon_unavailable" {
		t.Errorf("code = %q, want daemon_unavailable", got)
	}
}

func TestJSONFailureEnvelopeReportsUsageErrors(t *testing.T) {
	code, stdout, stderr := runJSON(t, &fakeManagementClient{}, "", "profiles", "rename", "work", "--json")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if got := decodeErrorRecord(t, stderr).Code; got != "invalid_argument" {
		t.Errorf("code = %q, want invalid_argument", got)
	}
}

func TestJSONFailureEnvelopeIsAbsentWithoutJSON(t *testing.T) {
	client := &fakeManagementClient{statusErr: errors.New("boom")}
	code, _, stderr := runJSON(t, client, "", "status")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if strings.HasPrefix(strings.TrimSpace(stderr), "{") {
		t.Errorf("stderr = %q, want plain text", stderr)
	}
}

func TestDaemonActionJSON(t *testing.T) {
	client := &fakeManagementClient{daemonState: controlapi.DaemonState{State: "down"}}
	code, stdout, stderr := runJSON(t, client, "", "down", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got controlapi.DaemonState
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if got.State != "down" {
		t.Errorf("state = %q, want down", got.State)
	}
	if client.daemonUp {
		t.Error("down asked the daemon to come up")
	}
}

func TestVersionJSONReportsServerPresence(t *testing.T) {
	t.Run("daemon up", func(t *testing.T) {
		client := &fakeManagementClient{serverVersion: tailmixversion.Meta{Short: "tailmixd-test"}}
		code, stdout, stderr := runJSON(t, client, "", "version", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		var got versionReport
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout = %q: %v", stdout, err)
		}
		if !got.DaemonUp || got.ServerVersion == "" {
			t.Errorf("report = %+v, want a server version", got)
		}
		if got.ClientVersion != "tailmix test-version" {
			t.Errorf("client version = %q", got.ClientVersion)
		}
	})
	t.Run("daemon down", func(t *testing.T) {
		client := &fakeManagementClient{versionErr: errors.New("connect to tailmix daemon: no socket")}
		code, stdout, stderr := runJSON(t, client, "", "version", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		var got versionReport
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout = %q: %v", stdout, err)
		}
		if got.DaemonUp || got.ServerVersion != "" {
			t.Errorf("report = %+v, want no server version", got)
		}
	})
}

func TestProfilesRenameJSON(t *testing.T) {
	client := &fakeManagementClient{profile: controlapi.Profile{Name: "renamed"}}
	code, stdout, stderr := runJSON(t, client, "", "profiles", "rename", "work", "renamed", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got controlapi.Profile
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if got.Name != "renamed" {
		t.Errorf("profile = %+v", got)
	}
}
