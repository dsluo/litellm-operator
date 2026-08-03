package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	litellmv1alpha1 "github.com/home-operations/litellm-operator/api/v1alpha1"
)

const (
	testMasterSecretName = "master"
	testMasterSecretKey  = "key"
)

func TestLiteLLMVirtualKeyReconciler_ReconcileCreatesSecretOnce(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/key/generate", r.URL.Path)
		assert.Equal(t, "Bearer master", r.Header.Get("Authorization"))
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		requests = append(requests, request)
		_ = json.NewEncoder(w).Encode(map[string]string{testMasterSecretKey: "sk-generated"})
	}))
	defer srv.Close()

	key := testVirtualKey()
	proxy := testVirtualKeyProxy(srv.URL)
	masterKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testMasterSecretName, Namespace: key.Namespace},
		Data:       map[string][]byte{testMasterSecretKey: []byte("master")},
	}
	r := testVirtualKeyReconciler(t, key, proxy, masterKey)
	request := types.NamespacedName{Namespace: key.Namespace, Name: key.Name}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: request})
	require.NoError(t, err)
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: request})
	require.NoError(t, err)

	var secret corev1.Secret
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: key.Spec.SecretName}, &secret))
	assert.Equal(t, []byte("sk-generated"), secret.Data["token"])
	assert.True(t, metav1.IsControlledBy(&secret, key))
	require.Len(t, requests, 1)
	assert.Equal(t, "application", requests[0]["key_alias"])
	assert.Equal(t, []any{"openai/gpt-5"}, requests[0]["models"])
	assert.Equal(t, "team-a", requests[0]["team_id"])
}

func TestLiteLLMVirtualKeyReconciler_ReconcileDeleteDeletesRemoteKey(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/key/delete", r.URL.Path)
		var request struct {
			Keys []string `json:"keys"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		deleted = request.Keys
	}))
	defer srv.Close()

	key := testVirtualKey()
	key.Finalizers = []string{virtualKeyFinalizer}
	proxy := testVirtualKeyProxy(srv.URL)
	masterKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testMasterSecretName, Namespace: key.Namespace},
		Data:       map[string][]byte{testMasterSecretKey: []byte("master")},
	}
	output := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Spec.SecretName, Namespace: key.Namespace},
		Data:       map[string][]byte{key.SecretDataKey(): []byte("sk-generated")},
	}
	r := testVirtualKeyReconciler(t, key, proxy, masterKey, output)

	require.NoError(t, r.reconcileDelete(t.Context(), key))
	assert.Equal(t, []string{"sk-generated"}, deleted)
	assert.NotContains(t, key.Finalizers, virtualKeyFinalizer)
}

func TestVirtualKeyRequest_MaxBudget(t *testing.T) {
	tests := []struct {
		name      string
		maxBudget string
		want      float64
		wantErr   string
	}{
		{name: "omitted"},
		{name: "decimal", maxBudget: "12.50", want: 12.5},
		{name: "invalid", maxBudget: "twelve", wantErr: "parse maxBudget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := testVirtualKey()
			key.Spec.MaxBudget = tt.maxBudget
			request, err := virtualKeyRequest(key)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.maxBudget == "" {
				assert.Nil(t, request.MaxBudget)
				return
			}
			require.NotNil(t, request.MaxBudget)
			assert.Equal(t, tt.want, *request.MaxBudget)
		})
	}
}

func testVirtualKey() *litellmv1alpha1.LiteLLMVirtualKey {
	return &litellmv1alpha1.LiteLLMVirtualKey{
		ObjectMeta: metav1.ObjectMeta{Name: "application", Namespace: "default"},
		Spec: litellmv1alpha1.LiteLLMVirtualKeySpec{
			ProxyRef:   "proxy",
			SecretName: "application-key",
			SecretKey:  "token",
			KeyAlias:   "application",
			Models:     []string{"openai/gpt-5"},
			TeamID:     "team-a",
		},
	}
}

func testVirtualKeyProxy(endpoint string) *litellmv1alpha1.LiteLLMProxy {
	return &litellmv1alpha1.LiteLLMProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "default"},
		Spec: litellmv1alpha1.LiteLLMProxySpec{APIAccess: &litellmv1alpha1.APIAccessSpec{
			Endpoint:     endpoint,
			MasterKeyRef: litellmv1alpha1.SecretKeyRef{Name: testMasterSecretName, Key: testMasterSecretKey},
		}},
	}
}

func testVirtualKeyReconciler(t *testing.T, objects ...runtime.Object) *LiteLLMVirtualKeyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, litellmv1alpha1.AddToScheme(scheme))
	return &LiteLLMVirtualKeyReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&litellmv1alpha1.LiteLLMVirtualKey{}).WithRuntimeObjects(objects...).Build(),
		Scheme: scheme,
	}
}
