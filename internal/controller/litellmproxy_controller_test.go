package controller

import (
	"testing"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	litellmv1alpha1 "github.com/home-operations/litellm-operator/api/v1alpha1"
)

const (
	testProxyName      = "main"
	testProxyNamespace = "ai"
	testMetricsSecret  = "metrics-key"
	testMetricsKey     = "key"
	testMetricsReason  = "MetricsSecretMissing"
)

func testMetricsProxy(ref *litellmv1alpha1.SecretKeyRef) *litellmv1alpha1.LiteLLMProxy {
	return &litellmv1alpha1.LiteLLMProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testProxyName, Namespace: testProxyNamespace},
		Spec: litellmv1alpha1.LiteLLMProxySpec{
			Metrics: &litellmv1alpha1.MetricsSpec{Enabled: true, BearerTokenRef: ref},
		},
	}
}

func testProxyReconciler(t *testing.T, objects ...runtime.Object) *LiteLLMProxyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, litellmv1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, monitoringv1.AddToScheme(scheme))
	return &LiteLLMProxyReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&litellmv1alpha1.LiteLLMProxy{}).
			WithRuntimeObjects(objects...).Build(),
		Scheme:                  scheme,
		serviceMonitorAvailable: true,
	}
}

func reconcileTestProxy(t *testing.T, r *LiteLLMProxyReconciler) error {
	t.Helper()
	_, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyName},
	})
	return err
}

func testProxyCondition(t *testing.T, r *LiteLLMProxyReconciler) *metav1.Condition {
	t.Helper()
	var proxy litellmv1alpha1.LiteLLMProxy
	require.NoError(t, r.Get(t.Context(),
		types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyName}, &proxy))
	return meta.FindStatusCondition(proxy.Status.Conditions, conditionTypeReady)
}

func testServiceMonitorExists(t *testing.T, r *LiteLLMProxyReconciler) bool {
	t.Helper()
	var monitor monitoringv1.ServiceMonitor
	err := r.Get(t.Context(),
		types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyName}, &monitor)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// A bearerTokenRef the operator cannot resolve must fail the proxy rather than
// leave a ServiceMonitor behind that Prometheus can never authenticate with.
func TestLiteLLMProxyReconciler_MetricsBearerTokenUnresolvable(t *testing.T) {
	tests := []struct {
		name   string
		secret *corev1.Secret
	}{
		{
			name: "secret does not exist",
		},
		{
			name: "secret has no such key",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testMetricsSecret, Namespace: testProxyNamespace},
				Data:       map[string][]byte{"other": []byte("sk-metrics")},
			},
		},
		{
			name: "key is present but empty",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testMetricsSecret, Namespace: testProxyNamespace},
				Data:       map[string][]byte{testMetricsKey: {}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxy := testMetricsProxy(&litellmv1alpha1.SecretKeyRef{Name: testMetricsSecret, Key: testMetricsKey})
			objects := []runtime.Object{proxy}
			if tc.secret != nil {
				objects = append(objects, tc.secret)
			}
			r := testProxyReconciler(t, objects...)

			err := reconcileTestProxy(t, r)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testMetricsReason)

			ready := testProxyCondition(t, r)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Equal(t, testMetricsReason, ready.Reason)
			assert.Contains(t, ready.Message, "spec.metrics.bearerTokenRef")
			assert.NotContains(t, ready.Message, "sk-metrics")

			assert.False(t, testServiceMonitorExists(t, r), "no ServiceMonitor for an unusable token")
		})
	}
}

func TestLiteLLMProxyReconciler_MetricsBearerTokenResolves(t *testing.T) {
	proxy := testMetricsProxy(&litellmv1alpha1.SecretKeyRef{Name: testMetricsSecret, Key: testMetricsKey})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testMetricsSecret, Namespace: testProxyNamespace},
		Data:       map[string][]byte{testMetricsKey: []byte("sk-metrics")},
	}
	r := testProxyReconciler(t, proxy, secret)

	require.NoError(t, reconcileTestProxy(t, r))

	ready := testProxyCondition(t, r)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.True(t, testServiceMonitorExists(t, r))
}

// Metrics without a bearerTokenRef is the requireAuth=false shape: there is no
// reference to resolve, so the check must not invent a failure.
func TestLiteLLMProxyReconciler_MetricsWithoutBearerTokenRef(t *testing.T) {
	r := testProxyReconciler(t, testMetricsProxy(nil))

	require.NoError(t, reconcileTestProxy(t, r))
	assert.True(t, testServiceMonitorExists(t, r))
}

// Without the Prometheus Operator CRDs no ServiceMonitor is written, so nothing
// consumes the token and an unresolvable ref must not fail the proxy.
func TestLiteLLMProxyReconciler_MetricsBearerTokenIgnoredWithoutCRD(t *testing.T) {
	proxy := testMetricsProxy(&litellmv1alpha1.SecretKeyRef{Name: testMetricsSecret, Key: testMetricsKey})
	r := testProxyReconciler(t, proxy)
	r.serviceMonitorAvailable = false

	require.NoError(t, reconcileTestProxy(t, r))

	ready := testProxyCondition(t, r)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}
