package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/controller"
	"github.com/tkhq/infra-smoketest/internal/definition"
	"github.com/tkhq/infra-smoketest/internal/probe"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: infra-smoketest controller [flags] | probe ready|storage-write|storage-read|identity|cert-manager|kube-state-metrics")
	}
	ctx := ctrl.SetupSignalHandler()
	if os.Args[1] == "probe" {
		if len(os.Args) != 3 {
			return fmt.Errorf("probe requires one subcommand")
		}
		if os.Args[2] != "ready" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
		}
		return probe.Run(ctx, os.Args[2])
	}
	if os.Args[1] != "controller" {
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	image := flags.String("probe-image", "", "Override probe image; defaults to this controller's Pod container image")
	container := flags.String("controller-container-name", "controller", "Container name used for image discovery")
	account := flags.String("aws-account-id", "", "Optional: pin the expected AWS account; by default each Run reads the identity ServiceAccount's role annotation")
	roleName := flags.String("identity-role-name", "", "Optional: pin the IAM role name in the pinned account (defaults to infra-smoketest-aws)")
	role := flags.String("expected-role-arn", "", "Optional: pin the full expected IAM role ARN instead of deriving it from the ServiceAccount annotation")
	region := flags.String("region", "us-east-1", "AWS region expected from the webhook")
	leader := flags.Bool("leader-elect", true, "Enable controller leader election")
	metricsAddress := flags.String("metrics-bind-address", ":8080", "Metrics listen address; use 0 to disable")
	logOptions := zap.Options{}
	logOptions.BindFlags(flags)
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	accountID, roleARN, err := controller.ResolveAWSIdentity(*account, *roleName, *role)
	if err != nil {
		return err
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := api.AddToScheme(scheme); err != nil {
		return err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	resolved, err := controller.ResolveProbeImage(ctx, direct, *image, os.Getenv("POD_NAMESPACE"), os.Getenv("POD_NAME"), *container)
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{definition.Namespace: {}}}, LeaderElection: *leader, LeaderElectionID: "infra-smoketest-controller", LeaderElectionNamespace: definition.Namespace, HealthProbeBindAddress: ":8081", Metrics: metricsserver.Options{BindAddress: *metricsAddress}})
	if err != nil {
		return err
	}
	r := &controller.Reconciler{Client: direct, ProbeImage: resolved, AWSAccountID: accountID, ExpectedRoleARN: roleARN, Region: *region}
	if err = r.SetupWithManager(mgr); err != nil {
		return err
	}
	if *metricsAddress != "0" {
		// Warm the definition informer alongside the controller's Run informer.
		if _, err = mgr.GetCache().GetInformer(ctx, &api.SmokeTest{}); err != nil {
			return err
		}
		if err = metrics.Registry.Register(controller.NewRunMetrics(mgr.GetCache())); err != nil {
			return err
		}
	}
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.Info("starting smoke-test controller", "probeImage", resolved, "pinnedAWSAccountID", accountID, "pinnedExpectedRoleARN", roleARN, "region", *region)
	return mgr.Start(ctx)
}
