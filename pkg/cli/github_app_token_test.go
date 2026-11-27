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

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGitHubAppTokenCmd_WritesSecret(t *testing.T) {
	const secretToken = "ghs_supersecret_value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"` + secretToken + `","expires_at":"2027-01-01T01:00:00Z"}`))
	}))
	defer srv.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().Build()
	var out bytes.Buffer
	opts := githubAppTokenOptions{
		appID: 7, installationID: 42, keyFile: keyFile,
		namespace: "ns", secret: "gh-token", apiBase: srv.URL,
	}
	for round := 0; round < 2; round++ { // second round exercises the update path
		if err := runGitHubAppToken(context.Background(), c, srv.Client(), &out, opts); err != nil {
			t.Fatalf("run round %d: %v", round, err)
		}
	}
	var s corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "gh-token"}, &s); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(s.Data["token"]) != secretToken {
		t.Fatal("secret key token not written")
	}
	if strings.Contains(out.String(), secretToken) {
		t.Fatal("stdout leaked the token")
	}
	if !strings.Contains(out.String(), "2027-01-01") {
		t.Fatalf("stdout should show expiry, got %q", out.String())
	}
}
