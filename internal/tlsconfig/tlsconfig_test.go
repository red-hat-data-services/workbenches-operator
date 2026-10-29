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

package tlsconfig

import (
	"context"
	"crypto/tls"
	stderrors "errors"
	"fmt"
	"net/http"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errBoom = stderrors.New("boom")

// fakeFetcher implements ProfileFetcher for testing.
type fakeFetcher struct {
	profileResult     configv1.TLSProfileSpec
	profileErr        error
	adherenceResult   configv1.TLSAdherencePolicy
	adherenceErr      error
	profileCalled     bool
	adherenceCalled   bool
	newConfigCalled   bool
	configFromProfile func(*tls.Config)
	unsupported       []string
}

func (f *fakeFetcher) FetchTLSProfile(_ context.Context, _ client.Client) (configv1.TLSProfileSpec, error) {
	f.profileCalled = true
	return f.profileResult, f.profileErr
}

func (f *fakeFetcher) FetchTLSAdherencePolicy(_ context.Context, _ client.Client) (configv1.TLSAdherencePolicy, error) {
	f.adherenceCalled = true
	return f.adherenceResult, f.adherenceErr
}

func (f *fakeFetcher) NewTLSConfigFromProfile(_ configv1.TLSProfileSpec) (func(*tls.Config), []string) {
	f.newConfigCalled = true
	fn := f.configFromProfile
	if fn == nil {
		fn = func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS12
		}
	}
	return fn, f.unsupported
}

// applyTLSOpts applies a slice of TLS option functions to a fresh tls.Config and returns it.
func applyTLSOpts(opts []func(*tls.Config)) *tls.Config {
	c := &tls.Config{}
	for _, fn := range opts {
		fn(c)
	}
	return c
}

func TestBootstrap_NonOpenShiftCluster(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{
			Group: "config.openshift.io", Resource: "apiservers",
		}},
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=false for non-OpenShift cluster")
	}
	if result.TLSAdherenceFetched {
		t.Error("expected TLSAdherenceFetched=false when config API is unavailable")
	}
	if !fetcher.profileCalled {
		t.Error("expected FetchTLSProfile to be called")
	}
	if fetcher.adherenceCalled {
		t.Error("expected FetchTLSAdherencePolicy NOT to be called for non-OpenShift")
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion=TLS12, got %d", cfg.MinVersion)
	}
	if len(cfg.CipherSuites) != len(IntermediateCiphers) {
		t.Errorf("expected %d intermediate ciphers, got %d", len(IntermediateCiphers), len(cfg.CipherSuites))
	}
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != protoHTTP1 {
		t.Errorf("expected NextProtos=[http/1.1] (HTTP/2 disabled), got %v", cfg.NextProtos)
	}
}

func TestBootstrap_APIServerNotFound(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: errors.NewNotFound(schema.GroupResource{
			Group: "config.openshift.io", Resource: "apiservers",
		}, "cluster"),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=false for NotFound")
	}
	if result.TLSAdherenceFetched {
		t.Error("expected TLSAdherenceFetched=false when APIServer not found")
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion=TLS12, got %d", cfg.MinVersion)
	}
}

func TestBootstrap_TransientError_ServiceUnavailable(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: errors.NewServiceUnavailable("api down"),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=true for transient error (watcher self-healing)")
	}
	if !result.TLSAdherenceFetched {
		t.Error("expected TLSAdherenceFetched=true when config API is assumed present")
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion=TLS12 (hardened defaults), got %d", cfg.MinVersion)
	}
	if len(cfg.CipherSuites) != len(IntermediateCiphers) {
		t.Errorf("expected intermediate cipher suite fallback, got %d ciphers", len(cfg.CipherSuites))
	}
}

func TestBootstrap_TransientError_Timeout(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: errors.NewTimeoutError("timed out", 10),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=true for timeout (watcher self-healing)")
	}
}

func TestBootstrap_TransientError_ContextDeadlineExceeded(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: fmt.Errorf("failed to get APIServer: %w", context.DeadlineExceeded),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("context.DeadlineExceeded should be transient, got fatal: %v", err)
	}

	if !result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=true for context deadline (watcher self-healing)")
	}
}

func TestBootstrap_TransientError_ServerTimeout(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: errors.NewServerTimeout(schema.GroupResource{
			Group: "config.openshift.io", Resource: "apiservers",
		}, "get", 10),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=true for server timeout (watcher self-healing)")
	}
}

func TestBootstrap_TransientError_TooManyRequests(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: errors.NewTooManyRequestsError("throttled"),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=true for TooManyRequests (watcher self-healing)")
	}
}

func TestBootstrap_UnknownError_Fatal(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: fmt.Errorf("unexpected: %w", errors.NewInternalError(errBoom)),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err == nil {
		t.Fatal("expected error for unknown API error, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result on fatal error, got %+v", result)
	}
}

func TestBootstrap_Success_OpenShift(t *testing.T) {
	profileSpec := configv1.TLSProfileSpec{
		// Mixed list: one TLS 1.3-only suite, one RC4 suite, and one this
		// build can negotiate.
		Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "TLS_RSA_WITH_RC4_128_SHA", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
		MinTLSVersion: configv1.VersionTLS12,
	}

	appliedMinVersion := uint16(0)
	fetcher := &fakeFetcher{
		profileResult:   profileSpec,
		adherenceResult: configv1.TLSAdherencePolicyStrictAllComponents,
		configFromProfile: func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS12
			appliedMinVersion = c.MinVersion
		},
	}

	result, err := Bootstrap(context.Background(), nil, true, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.HasOpenShiftConfigAPI {
		t.Error("expected HasOpenShiftConfigAPI=true on success")
	}
	if result.Profile.MinTLSVersion != configv1.VersionTLS12 {
		t.Errorf("expected profile to be stored, got MinTLSVersion=%s", result.Profile.MinTLSVersion)
	}
	if !result.TLSAdherenceFetched {
		t.Error("expected TLSAdherenceFetched=true")
	}
	if result.TLSAdherence != configv1.TLSAdherencePolicyStrictAllComponents {
		t.Errorf("expected TLSAdherence=StrictAllComponents, got %q", result.TLSAdherence)
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if appliedMinVersion != tls.VersionTLS12 {
		t.Error("expected profile-driven TLS config function to be applied")
	}
	// Mixed lists keep their usable subset: the TLS 1.3-only and RC4 IDs are
	// dropped.
	if len(cfg.CipherSuites) != 1 || cfg.CipherSuites[0] != tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 {
		t.Errorf("expected usable subset [TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256], got %v", cfg.CipherSuites)
	}
	// HTTP/2 enabled: expect both h2 and http/1.1
	if len(cfg.NextProtos) != 2 || cfg.NextProtos[0] != protoH2 || cfg.NextProtos[1] != protoHTTP1 {
		t.Errorf("expected NextProtos=[h2, http/1.1] (HTTP/2 enabled), got %v", cfg.NextProtos)
	}
}

func TestBootstrap_Success_UnsupportedCiphers(t *testing.T) {
	fetcher := &fakeFetcher{
		profileResult: configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12},
		unsupported:   []string{"FAKE_CIPHER_1", "FAKE_CIPHER_2"},
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.UnsupportedCiphers) != 2 {
		t.Errorf("expected 2 unsupported ciphers, got %d", len(result.UnsupportedCiphers))
	}
}

func TestBootstrap_NextProtos_HTTP2Enabled(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{
			Group: "config.openshift.io", Resource: "apiservers",
		}},
	}

	result, err := Bootstrap(context.Background(), nil, true, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if len(cfg.NextProtos) != 2 || cfg.NextProtos[0] != protoH2 || cfg.NextProtos[1] != protoHTTP1 {
		t.Errorf("expected NextProtos=[h2, http/1.1], got %v", cfg.NextProtos)
	}
}

func TestBootstrap_NextProtos_HTTP2Disabled(t *testing.T) {
	fetcher := &fakeFetcher{
		profileErr: &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{
			Group: "config.openshift.io", Resource: "apiservers",
		}},
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != protoHTTP1 {
		t.Errorf("expected NextProtos=[http/1.1], got %v", cfg.NextProtos)
	}
}

func TestBootstrap_AdherenceFetchTransientError_NonFatalButNotFetched(t *testing.T) {
	fetcher := &fakeFetcher{
		profileResult: configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12},
		adherenceErr:  errors.NewServiceUnavailable("adherence API down"),
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.TLSAdherenceFetched {
		t.Error("expected TLSAdherenceFetched=false on transient adherence fetch failure")
	}
	if result.TLSAdherence != configv1.TLSAdherencePolicyNoOpinion {
		t.Errorf("expected zero TLSAdherence on fetch failure, got %q", result.TLSAdherence)
	}
}

func TestBootstrap_AdherenceUnsetPolicy_IsAFetchedResult(t *testing.T) {
	fetcher := &fakeFetcher{
		profileResult:   configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12},
		adherenceResult: configv1.TLSAdherencePolicyNoOpinion,
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.TLSAdherenceFetched {
		t.Error("expected TLSAdherenceFetched=true for a successfully unset policy")
	}
	if result.TLSAdherence != configv1.TLSAdherencePolicyNoOpinion {
		t.Errorf("expected TLSAdherence=NoOpinion, got %q", result.TLSAdherence)
	}
}

func TestBootstrap_AdherenceDeniedRead_StopsStartup(t *testing.T) {
	deniedErrors := []struct {
		name string
		err  error
	}{
		{"Forbidden", errors.NewForbidden(schema.GroupResource{Resource: "apiservers"}, "cluster", stderrors.New("no access"))},
		{"Unauthorized", errors.NewUnauthorized("bad creds")},
		{"InternalError", errors.NewInternalError(errBoom)},
	}

	for _, tc := range deniedErrors {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &fakeFetcher{
				profileResult: configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12},
				adherenceErr:  tc.err,
			}
			result, err := Bootstrap(context.Background(), nil, false, fetcher)
			if err == nil {
				t.Fatalf("denied adherence read %s must stop startup, got nil error", tc.name)
			}
			if result != nil {
				t.Errorf("expected nil result on denied adherence read %s", tc.name)
			}
		})
	}
}

func TestBootstrap_ProfileInvalidMinimum_FailsWithoutPanic(t *testing.T) {
	invalidMinima := []configv1.TLSProtocolVersion{
		"VersionTLS09",
		"VersionTLS14",
		"not-a-version",
	}

	for _, minVersion := range invalidMinima {
		t.Run(string(minVersion), func(t *testing.T) {
			fetcher := &fakeFetcher{
				profileResult: configv1.TLSProfileSpec{MinTLSVersion: minVersion},
			}
			result, err := Bootstrap(context.Background(), nil, false, fetcher)
			if err == nil {
				t.Fatalf("invalid minimum %q must fail, got nil error", minVersion)
			}
			if result != nil {
				t.Errorf("expected nil result for invalid minimum %q", minVersion)
			}
			if fetcher.newConfigCalled {
				t.Error("expected NewTLSConfigFromProfile not to be called for an invalid profile")
			}
		})
	}
}

func TestBootstrap_ProfileNoUsableCipherSuites_Fails(t *testing.T) {
	unusableProfiles := []struct {
		name    string
		profile configv1.TLSProfileSpec
	}{
		{
			name: "TLS 1.3-only suites under a TLS 1.2 minimum",
			profile: configv1.TLSProfileSpec{
				Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "TLS_CHACHA20_POLY1305_SHA256"},
				MinTLSVersion: configv1.VersionTLS12,
			},
		},
		{
			name: "unresolvable cipher names",
			profile: configv1.TLSProfileSpec{
				Ciphers:       []string{"NOT_A_CIPHER", "ALSO_NOT_A_CIPHER"},
				MinTLSVersion: configv1.VersionTLS12,
			},
		},
		{
			name: "RC4-only suites are prohibited by RFC 7465",
			profile: configv1.TLSProfileSpec{
				Ciphers:       []string{"TLS_RSA_WITH_RC4_128_SHA"},
				MinTLSVersion: configv1.VersionTLS12,
			},
		},
	}

	for _, tc := range unusableProfiles {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &fakeFetcher{profileResult: tc.profile}
			result, err := Bootstrap(context.Background(), nil, false, fetcher)
			if err == nil {
				t.Fatalf("profile with no usable cipher suites must fail, got nil error")
			}
			if result != nil {
				t.Errorf("expected nil result for unusable profile %q", tc.name)
			}
			if fetcher.newConfigCalled {
				t.Error("expected NewTLSConfigFromProfile not to be called for an unusable profile")
			}
		})
	}
}

func TestBootstrap_ProfileModern_RemainsValid(t *testing.T) {
	modern := *configv1.TLSProfiles[configv1.TLSProfileModernType]
	fetcher := &fakeFetcher{
		profileResult: modern,
		configFromProfile: func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS13
		},
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected MinVersion=TLS13, got %d", cfg.MinVersion)
	}
	// TLS 1.3 suites are Go-managed: CipherSuites must never be set.
	if len(cfg.CipherSuites) != 0 {
		t.Errorf("expected no CipherSuites for a TLS 1.3 minimum, got %v", cfg.CipherSuites)
	}
}

func TestBootstrap_ProfileOld_HonoredExactly(t *testing.T) {
	old := *configv1.TLSProfiles[configv1.TLSProfileOldType]
	fetcher := &fakeFetcher{
		profileResult: old,
		// Mirror what NewTLSConfigFromProfile applies for the Old profile.
		configFromProfile: func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS10
		},
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("Old profile must remain valid: %v", err)
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if cfg.MinVersion != tls.VersionTLS10 {
		t.Errorf("expected MinVersion=TLS10 honored exactly (never floored), got %d", cfg.MinVersion)
	}
	if len(cfg.CipherSuites) == 0 {
		t.Fatal("expected a non-empty usable cipher subset for the Old profile")
	}
	implemented := implementedCipherSuites()
	for _, id := range cfg.CipherSuites {
		if id >= tls13CipherSuiteFirst && id <= tls13CipherSuiteLast {
			t.Errorf("TLS 1.3-only cipher 0x%04x must not be set for a TLS 1.0 minimum", id)
		}
		if !implemented[id] {
			t.Errorf("cipher 0x%04x is not implemented by this Go build", id)
		}
	}
	// Insecure-but-implemented suites (DES-CBC3-SHA in the Old profile) stay
	// usable: the profile is honored rather than narrowed beyond what Go
	// negotiates.
	foundDES := false
	for _, id := range cfg.CipherSuites {
		if id == tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA {
			foundDES = true
		}
	}
	if !foundDES {
		t.Error("expected DES-CBC3-SHA from the Old profile to stay in the usable subset")
	}
}

func TestBootstrap_ProfileMixedCiphers_KeepUsableSubset(t *testing.T) {
	fetcher := &fakeFetcher{
		profileResult: configv1.TLSProfileSpec{
			// TLS 1.3-only, OpenSSL-named, and unresolvable entries around a
			// usable suite: only the usable one survives.
			Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "ECDHE-RSA-AES128-GCM-SHA256", "NOT_A_CIPHER"},
			MinTLSVersion: configv1.VersionTLS12,
		},
		unsupported: []string{"NOT_A_CIPHER"},
	}

	result, err := Bootstrap(context.Background(), nil, false, fetcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := applyTLSOpts(result.TLSOpts)
	if len(cfg.CipherSuites) != 1 || cfg.CipherSuites[0] != tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 {
		t.Errorf("expected usable subset [TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256], got %v", cfg.CipherSuites)
	}
	if len(result.UnsupportedCiphers) != 1 || result.UnsupportedCiphers[0] != "NOT_A_CIPHER" {
		t.Errorf("expected unsupported=[NOT_A_CIPHER], got %v", result.UnsupportedCiphers)
	}
}

func TestBootstrap_HardenedDefaults_AllCiphersAreAEAD(t *testing.T) {
	for _, cipher := range IntermediateCiphers {
		name := tls.CipherSuiteName(cipher)
		if name == "" {
			t.Errorf("cipher 0x%04x is not recognized by Go's TLS library", cipher)
			continue
		}
		// All intermediate ciphers must be AEAD (GCM or ChaCha20-Poly1305)
		found := false
		for _, suite := range tls.CipherSuites() {
			if suite.ID == cipher {
				found = true
				if suite.Insecure {
					t.Errorf("cipher %s (0x%04x) is marked insecure", name, cipher)
				}
				break
			}
		}
		if !found {
			t.Errorf("cipher %s (0x%04x) not in tls.CipherSuites() (may be insecure)", name, cipher)
		}
	}
}

func TestBootstrap_TransientError_AllTypesClassifiedCorrectly(t *testing.T) {
	transientErrors := []struct {
		name string
		err  error
	}{
		{"ServiceUnavailable", errors.NewServiceUnavailable("down")},
		{"Timeout", errors.NewTimeoutError("slow", 5)},
		{"ServerTimeout", errors.NewServerTimeout(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "get", 10)},
		{"TooManyRequests", errors.NewTooManyRequestsError("throttled")},
		{"ContextDeadlineExceeded", context.DeadlineExceeded},
	}

	for _, tc := range transientErrors {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &fakeFetcher{profileErr: tc.err}
			result, err := Bootstrap(context.Background(), nil, false, fetcher)
			if err != nil {
				t.Fatalf("transient error %s should not be fatal: %v", tc.name, err)
			}
			if !result.HasOpenShiftConfigAPI {
				t.Errorf("transient error %s should set HasOpenShiftConfigAPI=true", tc.name)
			}
		})
	}
}

func TestBootstrap_NonTransientErrors_AreFatal(t *testing.T) {
	fatalErrors := []struct {
		name string
		err  error
	}{
		{"InternalError", errors.NewInternalError(errBoom)},
		{"Forbidden", errors.NewForbidden(schema.GroupResource{Resource: "apiservers"}, "cluster", stderrors.New("no access"))},
		{"Unauthorized", errors.NewUnauthorized("bad creds")},
		{"GenericStatusError", &errors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusBadGateway,
			Reason:  metav1.StatusReasonUnknown,
			Message: "bad gateway",
		}}},
	}

	for _, tc := range fatalErrors {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &fakeFetcher{profileErr: tc.err}
			result, err := Bootstrap(context.Background(), nil, false, fetcher)
			if err == nil {
				t.Fatalf("error %s should be fatal, got nil error", tc.name)
			}
			if result != nil {
				t.Errorf("expected nil result on fatal error %s", tc.name)
			}
		})
	}
}
