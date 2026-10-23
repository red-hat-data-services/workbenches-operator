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

// Package main is the entry point for the workbenches-operator.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/controller"
	"github.com/opendatahub-io/workbenches-operator/internal/platform"
	"github.com/opendatahub-io/workbenches-operator/internal/tlsconfig"
	"github.com/opendatahub-io/workbenches-operator/internal/webhook"
	webhooktls "github.com/opendatahub-io/workbenches-operator/internal/webhook/tls"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(admissionregistrationv1.AddToScheme(scheme))
	utilruntime.Must(componentsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(configv1.Install(scheme))
	utilruntime.Must(imagev1.Install(scheme))
}

func main() {
	var (
		metricsAddr          string
		enableLeaderElection bool
		probeAddr            string
		secureMetrics        bool
		enableHTTP2          bool
		enableWebhooks       bool
		manifestsBasePath    string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS with the default certificate or :8080 for HTTP.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Enable webhook server for connection injection and hardware profile mutation.")
	flag.StringVar(&manifestsBasePath, "manifests-base-path", "/opt/manifests",
		"Base path for component manifests.")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	info, err := os.Stat(manifestsBasePath)
	if err != nil {
		setupLog.Error(err, "invalid manifests-base-path", "path", manifestsBasePath)
		os.Exit(1)
	}

	if !info.IsDir() {
		setupLog.Error(errors.New("path exists but is not a directory"), "invalid manifests-base-path", "path", manifestsBasePath)
		os.Exit(1)
	}

	restCfg := ctrl.GetConfigOrDie()
	// Bound the bootstrap client's requests, including REST mapper discovery,
	// which the bootstrap context does not cover.
	bootstrapCfg := rest.CopyConfig(restCfg)
	bootstrapCfg.Timeout = 10 * time.Second
	bootstrapClient, err := client.New(bootstrapCfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create bootstrap client for TLS setup")
		os.Exit(1)
	}

	// Bootstrap cipher/min-version config from the cluster's APIServer TLS profile (OpenShift only).
	bootstrapCtx, bootstrapCancel := context.WithTimeout(context.Background(), 10*time.Second)
	tlsResult, err := tlsconfig.Bootstrap(bootstrapCtx, bootstrapClient, enableHTTP2, tlsconfig.DefaultFetcher())
	bootstrapCancel()
	if err != nil {
		setupLog.Error(err, "unable to bootstrap TLS configuration, refusing to start with unknown TLS posture")
		os.Exit(1)
	}
	logTLSBootstrapResult(tlsResult)

	// Configure webhook serving certs when requested. If no TLS provider is
	// available, disable the webhook server and skip the periodic ensurer so we
	// never re-annotate a MutatingWebhookConfiguration without a listening server.
	enableWebhooks, err = configureWebhookServingCerts(restCfg, bootstrapClient, enableWebhooks)
	if err != nil {
		setupLog.Error(err, "unable to configure webhook TLS")
		os.Exit(1)
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsResult.TLSOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	applicationsNamespace := resolveApplicationsNamespace()

	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "workbenches-operator.platform.opendatahub.io",
	}

	if enableWebhooks {
		mgrOptions.WebhookServer = ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    9443,
			TLSOpts: tlsResult.TLSOpts,
		})
	}
	// Partially addresses https://github.com/opendatahub-io/workbenches-operator/issues/43:
	// scope ConfigMap informer cache to APPLICATIONS_NAMESPACE. When unset, watch both
	// platform default apps namespaces so handshake works before CR platform is known.
	// Deployment watches remain cluster-scoped; the mapper filters to the resolved apps NS.
	cmNamespaces := map[string]cache.Config{}
	if applicationsNamespace != "" {
		cmNamespaces[applicationsNamespace] = cache.Config{}
	} else {
		cmNamespaces[platform.DefaultApplicationsNamespaceODH] = cache.Config{}
		cmNamespaces[platform.DefaultApplicationsNamespaceRHOAI] = cache.Config{}
	}
	mgrOptions.Cache = cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {
				Namespaces: cmNamespaces,
			},
		},
	}

	mgr, err := ctrl.NewManager(restCfg, mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.WorkbenchesReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		ManifestsBasePath:     manifestsBasePath,
		ApplicationsNamespace: applicationsNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Workbenches")
		os.Exit(1)
	}

	if enableWebhooks {
		if err := webhook.RegisterAllWebhooks(mgr); err != nil {
			setupLog.Error(err, "unable to register webhooks")
			os.Exit(1)
		}
	}

	if enableWebhooks {
		if err := addWebhookTLSEnsurer(mgr, restCfg, bootstrapClient); err != nil {
			setupLog.Error(err, "unable to set up webhook TLS ensurer")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Register SecurityProfileWatcher on OpenShift: cancel context on TLS profile change so pod restarts.
	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	if err := registerTLSProfileWatcher(mgr, tlsResult, cancel); err != nil {
		setupLog.Error(err, "unable to register TLS security profile watcher")
		cancel()
		os.Exit(1)
	}

	setupLog.Info("starting manager")

	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		cancel()
		os.Exit(1)
	}
	cancel()
}

func logTLSBootstrapResult(tlsResult *tlsconfig.BootstrapResult) {
	if !tlsResult.HasOpenShiftConfigAPI {
		setupLog.Info("TLS profile not available, using hardened defaults (non-OpenShift cluster)")
		return
	}
	if len(tlsResult.UnsupportedCiphers) > 0 {
		setupLog.Info("some ciphers from TLS profile are not supported by Go", "unsupported", tlsResult.UnsupportedCiphers)
	}
}

// configureWebhookServingCerts detects the TLS provider and ensures serving certs.
// When enableWebhooks is false, it is a no-op. When the provider is None, it returns
// false so the webhook server (and TLS ensurer) are not started.
func configureWebhookServingCerts(cfg *rest.Config, cli client.Client, enableWebhooks bool) (bool, error) {
	if !enableWebhooks {
		return false, nil
	}

	startupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	enabled, err := webhookTLSConfigure(startupCtx, cfg, cli)
	if err != nil {
		return false, err
	}
	if !enabled {
		setupLog.Info("webhook TLS provider unavailable; disabling webhooks")
		return false, nil
	}
	return true, nil
}

// webhookTLSConfigure is the Configure entrypoint; overridden in unit tests.
var webhookTLSConfigure = webhooktls.Configure

func resolveApplicationsNamespace() string {
	provided := os.Getenv("APPLICATIONS_NAMESPACE")
	if valid := platform.ValidApplicationsNamespace(provided); valid != "" {
		return valid
	}

	// Leave empty so the reconciler can fall back from Workbenches.spec.platform
	// (opendatahub vs redhat-ods-applications). Product installs always inject the env.
	// setupLog is intentional here: this runs during process startup before a
	// reconcile context exists.
	if strings.TrimSpace(provided) == "" {
		setupLog.Info("APPLICATIONS_NAMESPACE not set; reconciler will use platform default")
	} else {
		setupLog.Info("APPLICATIONS_NAMESPACE invalid; reconciler will use platform default",
			"provided", provided)
	}
	return ""
}

func addWebhookTLSEnsurer(mgr ctrl.Manager, cfg *rest.Config, cli client.Client) error {
	ensurer, err := webhooktls.NewEnsurer(cfg, cli)
	if err != nil {
		return err
	}
	return mgr.Add(ensurer)
}

func registerTLSProfileWatcher(mgr ctrl.Manager, tlsResult *tlsconfig.BootstrapResult, cancel context.CancelFunc) error {
	if !tlsResult.HasOpenShiftConfigAPI {
		return nil
	}

	watcher := &tlspkg.SecurityProfileWatcher{
		Client:                mgr.GetClient(),
		InitialTLSProfileSpec: tlsResult.Profile,
		OnProfileChange: func(_ context.Context, _, _ configv1.TLSProfileSpec) {
			setupLog.Info("TLS profile changed, initiating graceful shutdown to reload")
			cancel()
		},
		// Adherence tracking is armed even when the bootstrap read failed
		// transiently: the assumed NoOpinion then differs from the real policy
		// and triggers a reload once the watcher observes it.
		InitialTLSAdherencePolicy: tlsResult.TLSAdherence,
		OnAdherencePolicyChange: func(_ context.Context, _, _ configv1.TLSAdherencePolicy) {
			setupLog.Info("TLS adherence policy changed, initiating shutdown to reload")
			cancel()
		},
	}
	return watcher.SetupWithManager(mgr)
}
