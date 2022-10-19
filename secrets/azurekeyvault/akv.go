// Copyright 2019 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package azurekeyvault provides a secrets implementation backed by Azure KeyVault.
// See https://docs.microsoft.com/en-us/azure/key-vault/key-vault-whatis for more information.
// Use OpenKeeper to construct a *secrets.Keeper.
//
// # URLs
//
// For secrets.OpenKeeper, azurekeyvault registers for the scheme "azurekeyvault".
// The default URL opener will use Dial, which gets default credentials from the
// environment, unless the AZURE_KEYVAULT_AUTH_VIA_CLI environment variable is
// set to true, in which case it uses DialUsingCLIAuth to get credentials from the
// "az" command line.
//
// To customize the URL opener, or for more details on the URL format,
// see URLOpener.
// See https://gocloud.dev/concepts/urls/ for background information.
//
// # As
//
// azurekeyvault exposes the following type for As:
// - Error: autorest.DetailedError, see https://godoc.org/github.com/Azure/go-autorest/autorest#DetailedError
package azurekeyvault

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/keyvault/azkeys"
	"github.com/Azure/go-autorest/autorest"
	"github.com/google/wire"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/gcerr"
	"gocloud.dev/internal/useragent"
	"gocloud.dev/secrets"
)

var (
	// Map of HTTP Status Code to go-cloud ErrorCode
	errorCodeMap = map[int]gcerrors.ErrorCode{
		200: gcerrors.OK,
		400: gcerrors.InvalidArgument,
		401: gcerrors.PermissionDenied,
		403: gcerrors.PermissionDenied,
		404: gcerrors.NotFound,
		408: gcerrors.DeadlineExceeded,
		429: gcerrors.ResourceExhausted,
		500: gcerrors.Internal,
		501: gcerrors.Unimplemented,
	}
)

func init() {
	secrets.DefaultURLMux().RegisterKeeper(Scheme, new(defaultDialer))
}

// Set holds Wire providers for this package.
var Set = wire.NewSet(
	NewEnvironmentCredential,
	wire.Struct(new(URLOpener), "Client"),
)

// defaultDialer dials Azure KeyVault from the environment on the first call to OpenKeeperURL.
type defaultDialer struct {
	init   sync.Once
	opener *URLOpener
	err    error
}

func (o *defaultDialer) OpenKeeperURL(ctx context.Context, u *url.URL) (*secrets.Keeper, error) {
	o.init.Do(func() {
		// Determine the dialer to use. The default one gets
		// credentials from the environment, but an alternative is
		// to get credentials from the az CLI.
		dialer := NewEnvironmentCredential
		useCLIStr := os.Getenv("AZURE_KEYVAULT_AUTH_VIA_CLI")
		if useCLIStr != "" {
			if b, err := strconv.ParseBool(useCLIStr); err != nil {
				o.err = fmt.Errorf("invalid value %q for environment variable AZURE_KEYVAULT_AUTH_VIA_CLI: %v", useCLIStr, err)
				return
			} else if b {
				dialer = NewAzureCLICredential
			}
		}
		cred, err := dialer()
		if err != nil {
			o.err = err
			return
		}
		o.opener = &URLOpener{Credential: cred}
	})
	if o.err != nil {
		return nil, fmt.Errorf("open keeper %v: failed to Dial default KeyVault: %v", u, o.err)
	}
	return o.opener.OpenKeeperURL(ctx, u)
}

// Scheme is the URL scheme azurekeyvault registers its URLOpener under on secrets.DefaultMux.
const Scheme = "azurekeyvault"

// URLOpener opens Azure KeyVault URLs like
// "azurekeyvault://{keyvault-name}.vault.azure.net/keys/{key-name}/{key-version}?algorithm=RSA-OAEP-256".
//
// The "azurekeyvault" URL scheme is replaced with "https" to construct an Azure
// Key Vault keyID, as described in https://docs.microsoft.com/en-us/azure/key-vault/about-keys-secrets-and-certificates.
// The "/{key-version}"" suffix is optional; it defaults to the latest version.
//
// The "algorithm" query parameter sets the algorithm to use; see
// https://docs.microsoft.com/en-us/rest/api/keyvault/encrypt/encrypt#jsonwebkeyencryptionalgorithm
// for supported algorithms. It defaults to "RSA-OAEP-256".
//
// No other query parameters are supported.
type URLOpener struct {
	// Credential must be a valid Azure Identity credential.
	Credential azcore.TokenCredential

	// Options specifies the options to pass to OpenKeeper.
	Options KeeperOptions
}

// OpenKeeperURL opens an Azure KeyVault Keeper based on u.
func (o *URLOpener) OpenKeeperURL(ctx context.Context, u *url.URL) (*secrets.Keeper, error) {
	q := u.Query()
	algorithm := q.Get("algorithm")
	if algorithm != "" {
		o.Options.Algorithm = azkeys.JSONWebKeyEncryptionAlgorithm(algorithm)
		q.Del("algorithm")
	}
	for param := range q {
		return nil, fmt.Errorf("open keeper %v: invalid query parameter %q", u, param)
	}
	keyID := "https://" + path.Join(u.Host, u.Path)
	return OpenKeeper(o.Credential, keyID, &o.Options)
}

type keeper struct {
	client      *azkeys.Client
	keyVaultURI string
	keyName     string
	keyVersion  string
	options     *KeeperOptions
}

// KeeperOptions provides configuration options for encryption/decryption operations.
type KeeperOptions struct {
	// Algorithm sets the encryption algorithm used.
	// Defaults to "RSA-OAEP-256".
	// See https://docs.microsoft.com/en-us/rest/api/keyvault/encrypt/encrypt#jsonwebkeyencryptionalgorithm
	// for more details.
	Algorithm azkeys.JSONWebKeyEncryptionAlgorithm
}

// NewEnvironmentCredential gets a new *keyvault.BaseClient using authorization from the environment.
// See https://docs.microsoft.com/en-us/go/azure/azure-sdk-go-authorization#use-environment-based-authentication.
func NewEnvironmentCredential() (azcore.TokenCredential, error) {
	credential, err := azidentity.NewEnvironmentCredential(nil)
	if err != nil {
		return nil, err
	}
	return credential, nil
}

// NewAzureCLICredential gets a new *keyvault.BaseClient using authorization from the "az" CLI.
func NewAzureCLICredential() (azcore.TokenCredential, error) {
	credential, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		return nil, err
	}
	return credential, nil
}

// dial is a helper for Dial and DialUsingCLIAuth.
func dial(vaultURL string, credential azcore.TokenCredential) (*azkeys.Client, error) {
	return azkeys.NewClient(vaultURL, credential, &azkeys.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Telemetry: policy.TelemetryOptions{
				ApplicationID: useragent.AzureUserAgentPrefix("secrets"),
			},
		},
	}), nil
}

var (
	// Note that the last binding may be just a key, or key/version.
	keyIDRE = regexp.MustCompile(`^(https://.+\.vault\.(?:[a-z\d-.]+)/)keys/(.+)$`)
)

// OpenKeeper returns a *secrets.Keeper that uses Azure keyVault.
//
// client is a *keyvault.BaseClient instance, see https://godoc.org/github.com/Azure/azure-sdk-for-go/services/keyvault/v7.0/keyvault#BaseClient.
//
// keyID is a Azure Key Vault key identifier like "https://{keyvault-name}.vault.azure.net/keys/{key-name}/{key-version}".
// The "/{key-version}" suffix is optional; it defaults to the latest version.
// See https://docs.microsoft.com/en-us/azure/key-vault/about-keys-secrets-and-certificates
// for more details.
func OpenKeeper(client azcore.TokenCredential, keyID string, opts *KeeperOptions) (*secrets.Keeper, error) {
	drv, err := openKeeper(client, keyID, opts)
	if err != nil {
		return nil, err
	}
	return secrets.NewKeeper(drv), nil
}

func openKeeper(credential azcore.TokenCredential, keyID string, opts *KeeperOptions) (*keeper, error) {
	if opts == nil {
		opts = &KeeperOptions{}
	}
	if opts.Algorithm == "" {
		opts.Algorithm = azkeys.JSONWebKeyEncryptionAlgorithmRSAOAEP256
	}
	matches := keyIDRE.FindStringSubmatch(keyID)
	if len(matches) != 3 {
		return nil, fmt.Errorf("invalid keyID %q; must match %v %v", keyID, keyIDRE, matches)
	}
	// matches[0] is the whole keyID, [1] is the keyVaultURI, and [2] is the key or the key/version.
	keyVaultURI := matches[1]
	parts := strings.SplitN(matches[2], "/", 2)
	keyName := parts[0]
	var keyVersion string
	if len(parts) > 1 {
		keyVersion = parts[1]
	}
	client := azkeys.NewClient(keyVaultURI, credential, &azkeys.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Telemetry: policy.TelemetryOptions{
				ApplicationID: useragent.AzureUserAgentPrefix("secrets"),
			},
		},
	})

	return &keeper{
		client:      client,
		keyVaultURI: keyVaultURI,
		keyName:     keyName,
		keyVersion:  keyVersion,
		options:     opts,
	}, nil
}

// Encrypt encrypts the plaintext into a ciphertext.
func (k *keeper) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	keyOpsResult, err := k.client.Encrypt(ctx, k.keyName, k.keyVersion, azkeys.KeyOperationsParameters{
		Algorithm: &k.options.Algorithm,
		Value:     plaintext,
	}, nil)
	if err != nil {
		return nil, err
	}
	return keyOpsResult.Result, nil
}

// Decrypt decrypts the ciphertext into a plaintext.
func (k *keeper) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	keyOpsResult, err := k.client.Decrypt(ctx, k.keyName, k.keyVersion, azkeys.KeyOperationsParameters{
		Algorithm: &k.options.Algorithm,
		Value:     ciphertext,
	}, nil)
	if err != nil {
		return nil, err
	}
	return keyOpsResult.Result, nil
}

// Close implements driver.Keeper.Close.
func (k *keeper) Close() error { return nil }

// ErrorAs implements driver.Keeper.ErrorAs.
func (k *keeper) ErrorAs(err error, i interface{}) bool {
	e, ok := err.(autorest.DetailedError)
	if !ok {
		return false
	}
	p, ok := i.(*autorest.DetailedError)
	if !ok {
		return false
	}
	*p = e
	return true
}

// ErrorCode implements driver.ErrorCode.
func (k *keeper) ErrorCode(err error) gcerrors.ErrorCode {
	de, ok := err.(autorest.DetailedError)
	if !ok {
		return gcerr.Unknown
	}
	ec, ok := errorCodeMap[de.StatusCode.(int)]
	if !ok {
		return gcerr.Unknown
	}
	return ec
}
