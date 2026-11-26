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
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/meitheal/pkg/ghapp"
)

type githubAppTokenOptions struct {
	appID, installationID int64
	keyFile               string
	namespace, secret     string
	apiBase               string
}

func newGitHubAppTokenCommand() *cobra.Command {
	opts := githubAppTokenOptions{apiBase: "https://api.github.com"}
	cmd := &cobra.Command{
		Use:   "github-app-token",
		Short: "Mint a GitHub App installation token into a Secret",
		Long: `Mint a GitHub App installation token (valid one hour) from the App's
private key and write it to key "token" of a Kubernetes Secret, creating or
updating it. Prints only the token's expiry; the token and key are never
printed. Intended to run from a CronJob every 30 minutes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newMeithealClient()
			if err != nil {
				return err
			}
			return runGitHubAppToken(cmd.Context(), c, &http.Client{Timeout: 30 * time.Second}, cmd.OutOrStdout(), opts)
		},
	}
	f := cmd.Flags()
	f.Int64Var(&opts.appID, "app-id", 0, "GitHub App ID")
	f.Int64Var(&opts.installationID, "installation-id", 0, "GitHub App installation ID")
	f.StringVar(&opts.keyFile, "key-file", "", "Path to the App's PEM private key")
	f.StringVar(&opts.namespace, "namespace", "", "Namespace of the Secret to write")
	f.StringVar(&opts.secret, "secret", "", "Name of the Secret to write (key: token)")
	f.StringVar(&opts.apiBase, "api-base", opts.apiBase, "GitHub API base URL")
	for _, n := range []string{"app-id", "installation-id", "key-file", "namespace", "secret"} {
		_ = cmd.MarkFlagRequired(n)
	}
	return cmd
}

func loadAppKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied key path
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("key file is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("key file is not a PKCS#1 or PKCS#8 RSA private key")
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("key file is not an RSA private key")
	}
	return k, nil
}

func runGitHubAppToken(
	ctx context.Context, c client.Client, hc *http.Client, out io.Writer, o githubAppTokenOptions,
) error {
	key, err := loadAppKey(o.keyFile)
	if err != nil {
		return err
	}
	token, expires, err := ghapp.MintInstallationToken(ctx, hc, o.apiBase, o.appID, o.installationID, key, time.Now())
	if err != nil {
		return err
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: o.namespace, Name: o.secret}}
	err = c.Get(ctx, client.ObjectKeyFromObject(s), s)
	switch {
	case apierrors.IsNotFound(err):
		s.Type = corev1.SecretTypeOpaque
		s.Data = map[string][]byte{"token": []byte(token)}
		if err := c.Create(ctx, s); err != nil {
			return fmt.Errorf("create secret %s/%s: %w", o.namespace, o.secret, err)
		}
	case err != nil:
		return fmt.Errorf("get secret %s/%s: %w", o.namespace, o.secret, err)
	default:
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data["token"] = []byte(token)
		if err := c.Update(ctx, s); err != nil {
			return fmt.Errorf("update secret %s/%s: %w", o.namespace, o.secret, err)
		}
	}
	_, _ = fmt.Fprintf(out, "token written to secret %s/%s; expires %s\n",
		o.namespace, o.secret, expires.UTC().Format(time.RFC3339))
	return nil
}
