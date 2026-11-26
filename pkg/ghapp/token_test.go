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

package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

func TestMint_PostsJWTToInstallationEndpoint(t *testing.T) {
	key := testKey(t)
	now := time.Unix(1_800_000_000, 0)
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"tok","expires_at":"2027-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	if _, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 7, 42, key, now); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/app/installations/42/access_tokens" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	jwt := strings.TrimPrefix(gotAuth, "Bearer ")
	if jwt == gotAuth {
		t.Fatal("no Bearer prefix")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("sig decode: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(hb, &hdr); err != nil || hdr.Alg != "RS256" {
		t.Fatalf("header alg = %q err=%v", hdr.Alg, err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(cb, &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.Iss != "7" || claims.Iat != now.Add(-60*time.Second).Unix() || claims.Exp != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestMint_ReturnsTokenAndExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_abc","expires_at":"2027-01-01T01:00:00Z"}`))
	}))
	defer srv.Close()
	tok, exp, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 7, 42, testKey(t), time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	want := time.Date(2027, 1, 1, 1, 0, 0, 0, time.UTC)
	if tok != "ghs_abc" || !exp.Equal(want) {
		t.Fatalf("tok/exp mismatch (exp=%v)", exp)
	}
}

func TestMint_Non201IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("SECRET-BODY-MARKER"))
	}))
	defer srv.Close()
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 7, 42, testKey(t), time.Now())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET-BODY-MARKER") {
		t.Fatalf("error echoes response body: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should carry status: %v", err)
	}
}

func TestMint_ZeroExpiryIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_abc"}`))
	}))
	defer srv.Close()
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 7, 42, testKey(t), time.Now())
	if err == nil {
		t.Fatal("expected error for missing expires_at")
	}
}
