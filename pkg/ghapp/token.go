/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package ghapp mints GitHub App installation tokens using only the standard
// library: it signs a short-lived RS256 JWT with the App's private key and
// exchanges it for an installation access token.
package ghapp

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds how much of the GitHub response is read.
const maxResponseBytes = 1 << 20

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signJWT builds an RS256 JWT for the App: iat is backdated 60s for clock
// skew, exp is 9 minutes out (GitHub's maximum is 10).
func signJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	hdr, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": strconv.FormatInt(appID, 10),
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := b64(hdr) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("ghapp: sign jwt: %w", err)
	}
	return signing + "." + b64(sig), nil
}

// MintInstallationToken exchanges a freshly signed App JWT for an
// installation access token. The response body is never echoed into errors.
func MintInstallationToken(
	ctx context.Context, hc *http.Client, apiBase string,
	appID, installationID int64, key *rsa.PrivateKey, now time.Time,
) (string, time.Time, error) {
	if key == nil {
		return "", time.Time{}, errors.New("ghapp: nil private key")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	jwt, err := signJWT(appID, key, now)
	if err != nil {
		return "", time.Time{}, err
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", strings.TrimRight(apiBase, "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("ghapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := hc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("ghapp: request failed: %w", errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("ghapp: installation token request returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", time.Time{}, errors.New("ghapp: decode installation token response")
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("ghapp: response carried no token")
	}
	return out.Token, out.ExpiresAt, nil
}
