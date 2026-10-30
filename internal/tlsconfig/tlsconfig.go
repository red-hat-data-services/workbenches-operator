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

// Package tlsconfig provides TLS bootstrap logic for integrating with the
// OpenShift cluster-wide TLS security profile.
package tlsconfig

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	protoH2    = "h2"
	protoHTTP1 = "http/1.1"

	// tls13CipherSuiteFirst and tls13CipherSuiteLast bound the IANA IDs
	// reserved for TLS 1.3 cipher suites. Go manages those suites itself and
	// never negotiates them through Config.CipherSuites.
	tls13CipherSuiteFirst = 0x1300
	tls13CipherSuiteLast  = 0x13ff
)

// IntermediateCiphers is the Mozilla Intermediate cipher set, used as the
// hardened default on non-OpenShift clusters where the APIServer TLS profile
// is not available.
var IntermediateCiphers = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// BootstrapResult holds the results of TLS profile bootstrap.
type BootstrapResult struct {
	TLSOpts               []func(*tls.Config)
	Profile               configv1.TLSProfileSpec
	HasOpenShiftConfigAPI bool
	TLSAdherence          configv1.TLSAdherencePolicy
	TLSAdherenceFetched   bool
	UnsupportedCiphers    []string
}

// ProfileFetcher abstracts TLS profile fetching for testability.
type ProfileFetcher interface {
	FetchTLSProfile(ctx context.Context, k8sClient client.Client) (configv1.TLSProfileSpec, error)
	FetchTLSAdherencePolicy(ctx context.Context, k8sClient client.Client) (configv1.TLSAdherencePolicy, error)
	NewTLSConfigFromProfile(profile configv1.TLSProfileSpec) (func(*tls.Config), []string)
}

// defaultFetcher delegates to the real controller-runtime-common/pkg/tls functions.
type defaultFetcher struct{}

func (defaultFetcher) FetchTLSProfile(ctx context.Context, k8sClient client.Client) (configv1.TLSProfileSpec, error) {
	return tlspkg.FetchAPIServerTLSProfile(ctx, k8sClient)
}

func (defaultFetcher) FetchTLSAdherencePolicy(ctx context.Context, k8sClient client.Client) (configv1.TLSAdherencePolicy, error) {
	return tlspkg.FetchAPIServerTLSAdherencePolicy(ctx, k8sClient)
}

func (defaultFetcher) NewTLSConfigFromProfile(profile configv1.TLSProfileSpec) (func(*tls.Config), []string) {
	return tlspkg.NewTLSConfigFromProfile(profile)
}

// DefaultFetcher returns the production ProfileFetcher that uses the real
// controller-runtime-common library.
func DefaultFetcher() ProfileFetcher {
	return defaultFetcher{}
}

// isTransientAPIError returns true for API errors that should be treated as
// recoverable during startup (watcher will self-heal when the API recovers).
func isTransientAPIError(err error) bool {
	return k8serr.IsServiceUnavailable(err) ||
		k8serr.IsTimeout(err) ||
		k8serr.IsServerTimeout(err) ||
		k8serr.IsTooManyRequests(err) ||
		errors.Is(err, context.DeadlineExceeded)
}

// validateProfile resolves the profile's minimum TLS version and the cipher
// suites the running Go implementation can negotiate for minimums below
// TLS 1.3. It reports invalid minima as errors instead of letting
// NewTLSConfigFromProfile panic on them, and rejects cipher requests with no
// usable suite, which would otherwise silently widen to Go's default suites.
func validateProfile(profile configv1.TLSProfileSpec) (minVersion uint16, usableSuites []uint16, err error) {
	minVersion, err = libgocrypto.TLSVersion(string(profile.MinTLSVersion))
	if err != nil {
		return 0, nil, fmt.Errorf("invalid minimum TLS version %q: %w", profile.MinTLSVersion, err)
	}
	// Go 1.22+ defaults the server minimum to TLS 1.2 only when tls.Config.MinVersion
	// is unset; the explicit minimum set here needs no GODEBUG.
	if minVersion >= tls.VersionTLS13 {
		// TLS 1.3 cipher suites are Go-managed and never set through CipherSuites.
		return minVersion, nil, nil
	}
	if len(profile.Ciphers) == 0 {
		// No cipher request: leave CipherSuites unset so Go's defaults apply.
		return minVersion, nil, nil
	}

	implemented := implementedCipherSuites()
	for _, cipher := range profile.Ciphers {
		id, ok := resolveCipherSuite(cipher)
		if !ok || (id >= tls13CipherSuiteFirst && id <= tls13CipherSuiteLast) || !implemented[id] {
			continue
		}
		usableSuites = append(usableSuites, id)
	}
	if len(usableSuites) == 0 {
		return 0, nil, fmt.Errorf("no cipher suite in the profile is negotiable by this Go build for minimum TLS version %q: %v",
			profile.MinTLSVersion, profile.Ciphers)
	}
	return minVersion, usableSuites, nil
}

// resolveCipherSuite maps an IANA or OpenSSL cipher suite name to its IANA ID,
// mirroring the resolution inside NewTLSConfigFromProfile.
func resolveCipherSuite(name string) (uint16, bool) {
	if id, err := libgocrypto.CipherSuite(name); err == nil {
		return id, true
	}
	iana := libgocrypto.OpenSSLToIANACipherSuites([]string{name})
	if len(iana) == 1 {
		if id, err := libgocrypto.CipherSuite(iana[0]); err == nil {
			return id, true
		}
	}
	return 0, false
}

// implementedCipherSuites returns the IDs of every cipher suite implemented
// by the running Go build, excluding RC4 suites prohibited by RFC 7465.
func implementedCipherSuites() map[uint16]bool {
	implemented := make(map[uint16]bool)
	for _, suite := range tls.CipherSuites() {
		implemented[suite.ID] = true
	}
	for _, suite := range tls.InsecureCipherSuites() {
		if !strings.Contains(suite.Name, "RC4") {
			implemented[suite.ID] = true
		}
	}
	return implemented
}

// Bootstrap fetches the OpenShift cluster TLS profile and adherence policy,
// classifies errors, and returns a BootstrapResult with configured TLS options.
//
// Profile error classification:
//   - NoMatchError (non-OpenShift): hardened defaults, no watcher
//   - NotFound (APIServer missing): hardened defaults, no watcher
//   - Transient (503/timeout/429): hardened defaults, watcher enabled for self-healing
//   - Success: profile-driven config, watcher enabled
//   - Unusable profile (invalid minimum TLS version, or no cipher suite
//     negotiable by this Go build) or unknown error: returned as error
//     (caller should exit)
//
// Adherence policy classification:
//   - Success (including a successfully unset NoOpinion policy): stored with
//     TLSAdherenceFetched=true
//   - NoMatchError/NotFound/transient error: TLSAdherenceFetched stays false so
//     an unreadable policy stays distinct from a successfully unset one; the
//     watcher self-heals when the API recovers
//   - Forbidden/Unauthorized and unknown errors: returned as error (caller
//     should exit instead of assuming NoOpinion)
func Bootstrap(ctx context.Context, k8sClient client.Client, enableHTTP2 bool, fetcher ProfileFetcher) (*BootstrapResult, error) {
	nextProtos := []string{protoH2, protoHTTP1}
	if !enableHTTP2 {
		nextProtos = []string{protoHTTP1}
	}

	result := &BootstrapResult{}

	profile, err := fetcher.FetchTLSProfile(ctx, k8sClient)
	if err != nil {
		switch {
		case apimeta.IsNoMatchError(err):
			// Non-OpenShift cluster: CRD not registered.
		case k8serr.IsNotFound(err):
			// APIServer resource not found.
		case isTransientAPIError(err):
			result.HasOpenShiftConfigAPI = true
		default:
			return nil, err
		}
		result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS12
			c.CipherSuites = IntermediateCiphers
			c.NextProtos = nextProtos
		})
	} else {
		result.HasOpenShiftConfigAPI = true
		result.Profile = profile
		// Validate before NewTLSConfigFromProfile: its TLSVersionOrDie would
		// panic on the same invalid minima.
		minVersion, usableSuites, verr := validateProfile(profile)
		if verr != nil {
			return nil, fmt.Errorf("unusable APIServer TLS profile: %w", verr)
		}
		tlsConfigFn, unsupported := fetcher.NewTLSConfigFromProfile(profile)
		result.UnsupportedCiphers = unsupported
		result.TLSOpts = append(result.TLSOpts, tlsConfigFn)
		if minVersion < tls.VersionTLS13 && len(usableSuites) > 0 {
			// Mixed lists keep their usable subset: the helper also passes
			// through TLS 1.3-only IDs, inert for TLS <= 1.2 handshakes.
			result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
				c.CipherSuites = usableSuites
			})
		}
		result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
			c.NextProtos = nextProtos
		})
	}

	if result.HasOpenShiftConfigAPI {
		adherence, adherenceErr := fetcher.FetchTLSAdherencePolicy(ctx, k8sClient)
		switch {
		case adherenceErr == nil:
			result.TLSAdherence = adherence
			result.TLSAdherenceFetched = true
		case apimeta.IsNoMatchError(adherenceErr), k8serr.IsNotFound(adherenceErr), isTransientAPIError(adherenceErr):
			// Recoverable: TLSAdherenceFetched stays false so an unreadable
			// policy stays distinct from a successfully unset one.
		default:
			// Forbidden/Unauthorized and unknown errors leave the adherence
			// posture unknowable: refuse to start rather than assume NoOpinion.
			return nil, fmt.Errorf("unable to fetch TLS adherence policy: %w", adherenceErr)
		}
	}

	return result, nil
}
