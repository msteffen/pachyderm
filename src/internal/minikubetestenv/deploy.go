package minikubetestenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/helm"
	"github.com/gruntwork-io/terratest/modules/k8s"
	terraTest "github.com/gruntwork-io/terratest/modules/testing"
	"github.com/pachyderm/pachyderm/v2/src/client"
	"github.com/pachyderm/pachyderm/v2/src/internal/backoff"
	"github.com/pachyderm/pachyderm/v2/src/internal/config"
	"github.com/pachyderm/pachyderm/v2/src/internal/errors"
	"github.com/pachyderm/pachyderm/v2/src/internal/grpcutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/require"
	"github.com/pachyderm/pachyderm/v2/src/internal/testutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kube "k8s.io/client-go/kubernetes"
)

const (
	helmChartPublishedPath = "pach/pachyderm"
	localImage             = "local"
	licenseKeySecretName   = "enterprise-license-key-secret"
)

// defensively lock around helm calls
var mu sync.Mutex

type DeployOpts struct {
	Version            string
	Enterprise         bool
	AuthUser           string
	CleanupAfter       bool
	UseLeftoverCluster bool
	// Because NodePorts are cluster-wide, we use a PortOffset to
	// assign separate ports per deployment.
	// NOTE: it might make more sense to declare port instead of offset
	PortOffset uint16
}

type helmPutE func(t terraTest.TestingT, options *helm.Options, chart string, releaseName string) error

func helmLock(f helmPutE) helmPutE {
	return func(t terraTest.TestingT, options *helm.Options, chart string, releaseName string) error {
		mu.Lock()
		defer mu.Unlock()
		return f(t, options, chart, releaseName)
	}
}

func helmChartLocalPath(t testing.TB) string {
	dir, err := os.Getwd()
	require.NoError(t, err)
	parts := strings.Split(dir, string(os.PathSeparator))
	var relPathParts []string
	for i := len(parts) - 1; i >= 0; i-- {
		relPathParts = append(relPathParts, "..")
		if parts[i] == "src" {
			break
		}
	}
	relPathParts = append(relPathParts, "etc", "helm", "pachyderm")
	return filepath.Join(relPathParts...)
}

func getPachAddress(t testing.TB) *grpcutil.PachdAddress {
	cfg, err := config.Read(true, true)
	require.NoError(t, err)
	_, context, err := cfg.ActiveContext(true)
	require.NoError(t, err)
	address, err := client.GetUserMachineAddr(context)
	require.NoError(t, err)
	if address == nil {
		copy := grpcutil.DefaultPachdAddress
		address = &copy
	}
	return address
}

func localDeploymentWithMinioOptions(namespace, image string) *helm.Options {
	os := runtime.GOOS
	serviceType := ""
	switch os {
	case "darwin":
		serviceType = "LoadBalancer"
	default:
		serviceType = "NodePort"
	}
	return &helm.Options{
		KubectlOptions: &k8s.KubectlOptions{Namespace: namespace},
		SetValues: map[string]string{
			"deployTarget": "custom",

			"pachd.service.type":        serviceType,
			"pachd.image.tag":           image,
			"pachd.clusterDeploymentID": "dev",
			"pachd.lokiDeploy":          "true",

			"pachd.storage.backend":        "MINIO",
			"pachd.storage.minio.bucket":   "pachyderm-test",
			"pachd.storage.minio.endpoint": "minio.default.svc.cluster.local:9000",
			"pachd.storage.minio.id":       "minioadmin",
			"pachd.storage.minio.secret":   "minioadmin",

			"global.postgresql.postgresqlPassword":         "pachyderm",
			"global.postgresql.postgresqlPostgresPassword": "pachyderm",
		},
		SetStrValues: map[string]string{
			"pachd.storage.minio.signature": "",
			"pachd.storage.minio.secure":    "false",
		},
	}
}

func withEnterprise(t testing.TB, namespace string, address *grpcutil.PachdAddress) *helm.Options {
	return &helm.Options{
		KubectlOptions: &k8s.KubectlOptions{Namespace: namespace},
		SetValues: map[string]string{
			"pachd.enterpriseLicenseKeySecretName": licenseKeySecretName,
			"pachd.rootToken":                      testutil.RootToken,
			"pachd.oauthClientSecret":              "oidc-client-secret",
			"pachd.enterpriseSecret":               "enterprise-secret",
			// TODO: make these ports configurable to support IDP Login in parallel deployments
			"oidc.userAccessibleOauthIssuerHost": fmt.Sprintf("%s:30658", address.Host),
			"ingress.host":                       fmt.Sprintf("%s:30657", address.Host),
		},
	}
}

func withPort(t testing.TB, namespace string, port uint16) *helm.Options {
	return &helm.Options{
		KubectlOptions: &k8s.KubectlOptions{Namespace: namespace},
		SetValues: map[string]string{
			"pachd.service.apiGRPCPort":    fmt.Sprintf("%v", port),
			"pachd.service.oidcPort":       fmt.Sprintf("%v", port+1),
			"pachd.service.identityPort":   fmt.Sprintf("%v", port+2),
			"pachd.service.s3GatewayPort":  fmt.Sprintf("%v", port+3),
			"pachd.service.prometheusPort": fmt.Sprintf("%v", port+4),
		},
	}
}

func union(a, b *helm.Options) *helm.Options {
	c := &helm.Options{
		KubectlOptions: &k8s.KubectlOptions{Namespace: b.KubectlOptions.Namespace},
		SetValues:      make(map[string]string),
		SetStrValues:   make(map[string]string),
	}
	copy := func(src, dst *helm.Options) {
		for k, v := range src.SetValues {
			dst.SetValues[k] = v
		}
		for k, v := range src.SetStrValues {
			dst.SetStrValues[k] = v
		}
	}
	copy(a, c)
	copy(b, c)
	return c
}

// TODO(acohen4): also wait for Loki
func waitForPachd(t testing.TB, ctx context.Context, kubeClient *kube.Clientset, namespace, version string) {
	require.NoError(t, backoff.Retry(func() error {
		pachds, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=pachd"})
		if err != nil {
			return errors.Wrap(err, "error on pod list")
		}
		for _, p := range pachds.Items {
			if p.Status.Phase == v1.PodRunning && strings.HasSuffix(p.Spec.Containers[0].Image, ":"+version) && p.Status.ContainerStatuses[0].Ready && len(pachds.Items) == 1 {
				return nil
			}
		}
		return errors.Errorf("deployment in progress")
	}, backoff.RetryEvery(5*time.Second).For(5*time.Minute)))
}

func pachClient(t testing.TB, pachAddress *grpcutil.PachdAddress, authUser, namespace string) *client.APIClient {
	var c *client.APIClient
	// retry connecting if it doesn't immediately work
	require.NoError(t, backoff.Retry(func() error {
		t.Logf("Connecting to pachd on port: %v, in namespace: %s", pachAddress.Port, namespace)
		var err error
		c, err = client.NewFromPachdAddress(pachAddress, client.WithDialTimeout(10*time.Second))
		if err != nil {
			return errors.Wrapf(err, "failed to connect to pachd on port %v", pachAddress.Port)
		}
		return nil
	}, backoff.RetryEvery(time.Second).For(50*time.Second)))
	t.Logf("Success connecting to pachd on port: %v, in namespace: %s", pachAddress.Port, namespace)
	if authUser != "" {
		c = testutil.AuthenticateClient(t, c, authUser)
	}
	return c
}

func deleteRelease(t testing.TB, ctx context.Context, namespace string, kubeClient *kube.Clientset) {
	options := &helm.Options{
		KubectlOptions: &k8s.KubectlOptions{Namespace: namespace},
	}
	mu.Lock()
	err := helm.DeleteE(t, options, namespace, true)
	mu.Unlock()
	require.True(t, err == nil || strings.Contains(err.Error(), "not found"))
	require.NoError(t, kubeClient.CoreV1().PersistentVolumeClaims(namespace).DeleteCollection(ctx, *metav1.NewDeleteOptions(0), metav1.ListOptions{LabelSelector: "suite=pachyderm"}))
	require.NoError(t, backoff.Retry(func() error {
		pvcs, err := kubeClient.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: "suite=pachyderm"})
		if err != nil {
			return errors.Wrap(err, "error on pod list")
		}
		if len(pvcs.Items) == 0 {
			return nil
		}
		return errors.Errorf("pvcs have yet to be deleted")
	}, backoff.RetryEvery(5*time.Second).For(2*time.Minute)))
}

func createSecretEnterpriseKeySecret(t testing.TB, ctx context.Context, kubeClient *kube.Clientset, ns string) {
	_, err := kubeClient.CoreV1().Secrets(ns).Create(ctx, &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: licenseKeySecretName},
		StringData: map[string]string{
			"enterprise-license-key": testutil.GetTestEnterpriseCode(t),
		},
	}, metav1.CreateOptions{})
	require.True(t, err == nil || strings.Contains(err.Error(), "already exists"))
}

func putRelease(t testing.TB, ctx context.Context, namespace string, kubeClient *kube.Clientset, f helmPutE, opts *DeployOpts) *client.APIClient {
	if opts.CleanupAfter {
		t.Cleanup(func() {
			deleteRelease(t, context.Background(), namespace, kubeClient)
		})
	}
	version := localImage
	chartPath := helmChartLocalPath(t)
	if opts.Version != "" {
		version = opts.Version
		chartPath = helmChartPublishedPath
	}
	// TODO(acohen4): apply minio deployment to this namespace
	helmOpts := localDeploymentWithMinioOptions(namespace, version)
	pachAddress := getPachAddress(t)
	if opts.PortOffset != 0 {
		pachAddress.Port += opts.PortOffset
		helmOpts = union(helmOpts, withPort(t, namespace, pachAddress.Port))
	}
	if opts.Enterprise {
		createSecretEnterpriseKeySecret(t, ctx, kubeClient, namespace)
		helmOpts = union(helmOpts, withEnterprise(t, namespace, pachAddress))
	}
	if err := f(t, helmOpts, chartPath, namespace); err != nil {
		if opts.UseLeftoverCluster {
			return pachClient(t, pachAddress, opts.AuthUser, namespace)
		}
		deleteRelease(t, context.Background(), namespace, kubeClient)
		require.NoError(t, f(t, helmOpts, chartPath, namespace))
	}
	waitForPachd(t, ctx, kubeClient, namespace, version)
	return pachClient(t, pachAddress, opts.AuthUser, namespace)
}

// Deploy pachyderm using a `helm upgrade ...`
// returns an API Client corresponding to the deployment
func UpgradeRelease(t testing.TB, ctx context.Context, namespace string, kubeClient *kube.Clientset, opts *DeployOpts) *client.APIClient {
	return putRelease(t, ctx, namespace, kubeClient, helmLock(helm.UpgradeE), opts)
}

// Deploy pachyderm using a `helm install ...`
// returns an API Client corresponding to the deployment
func InstallRelease(t testing.TB, ctx context.Context, namespace string, kubeClient *kube.Clientset, opts *DeployOpts) *client.APIClient {
	return putRelease(t, ctx, namespace, kubeClient, helmLock(helm.InstallE), opts)
}