package warpcfg

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSaveSecretsIsOwnerOnlyAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := Secrets{
		PrivateKey:     "private-key",
		RegistrationID: "registration-id",
		AccessToken:    "access-token",
		License:        "license",
	}
	if err := SaveSecrets(dir, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(SecretPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("credentials file mode = %v, want 0600", perm)
	}
	got, err := LoadSecrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("credentials = %+v, want %+v", got, want)
	}
}

func TestLoadSecretsReportsReRegistrationWhenMissing(t *testing.T) {
	_, err := LoadSecrets(t.TempDir())
	if !errors.Is(err, ErrCredentialsMissing) {
		t.Fatalf("error = %v, want ErrCredentialsMissing", err)
	}
	if !strings.Contains(err.Error(), "re-register") {
		t.Fatalf("error %q does not name re-registration as the fix", err)
	}
}

func TestSaveSecretsKeepsPreviousCredentialsWhenWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := SaveSecrets(dir, Secrets{PrivateKey: "old", RegistrationID: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if err := SaveSecrets(dir, Secrets{PrivateKey: "new", RegistrationID: "new"}); err == nil {
		t.Fatal("write to a read-only directory unexpectedly succeeded")
	}
	got, err := LoadSecrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrivateKey != "old" || got.RegistrationID != "old" {
		t.Fatalf("credentials = %+v, want the previous ones", got)
	}
}
