// Package warpcfg holds the stored configuration and credentials of a
// Cloudflare WARP profile.
package warpcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

const secretFile = "warp.json"

// ErrCredentialsMissing reports a profile whose credentials cannot be used and
// must be registered again.
var ErrCredentialsMissing = errors.New("WARP credentials are missing; re-register the profile")

// Config is the non-secret part of a WARP profile. The daemon state stores it.
type Config struct {
	EndpointV4            string     `json:"endpointV4,omitempty"`
	EndpointV6            string     `json:"endpointV6,omitempty"`
	EndpointH2V4          string     `json:"endpointH2V4,omitempty"`
	EndpointH2V6          string     `json:"endpointH2V6,omitempty"`
	EndpointPublicKey     string     `json:"endpointPublicKey,omitempty"`
	IPv4                  netip.Addr `json:"ipv4,omitempty"`
	IPv6                  netip.Addr `json:"ipv6,omitempty"`
	DNS                   netip.Addr `json:"dns,omitempty"`
	UseHTTP2              bool       `json:"useHttp2,omitempty"`
	CredentialsGeneration string     `json:"credentialsGeneration,omitempty"`
}

// Secrets is the owner-only part of a WARP profile.
type Secrets struct {
	PrivateKey     string `json:"privateKey"`
	RegistrationID string `json:"registrationId"`
	AccessToken    string `json:"accessToken,omitempty"`
	License        string `json:"license,omitempty"`
}

// SecretPath is the credentials file of the profile stored in stateDir.
func SecretPath(stateDir string) string {
	return filepath.Join(stateDir, secretFile)
}

// LoadSecrets reads the credentials of the profile stored in stateDir.
func LoadSecrets(stateDir string) (Secrets, error) {
	data, err := os.ReadFile(SecretPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Secrets{}, ErrCredentialsMissing
		}
		return Secrets{}, err
	}
	var secrets Secrets
	if err := json.Unmarshal(data, &secrets); err != nil {
		return Secrets{}, fmt.Errorf("%w: %v", ErrCredentialsMissing, err)
	}
	if secrets.PrivateKey == "" || secrets.RegistrationID == "" {
		return Secrets{}, ErrCredentialsMissing
	}
	return secrets, nil
}

// SaveSecrets replaces the credentials of the profile stored in stateDir.
// The file is owner-only and replaced atomically, so an interrupted write
// keeps the previous credentials.
func SaveSecrets(stateDir string, secrets Secrets) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return err
	}
	path := SecretPath(stateDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
