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
	testKeyInfoPath      = "/key/info"
	testKeyAlias         = "application"
	testKeyModel         = "openai/gpt-5"
	testKeyTeam          = "team-a"

	testReflectorAnnotation = "reflector.v1.k8s.emberstack.com/reflection-allowed"
	testReflectorAllowed    = "true"
	// Metadata on the output Secret that the operator never manages.
	testUnmanagedKey   = "other"
	testUnmanagedValue = "keep"
)

// writeTestKeyInfo answers /key/info with the key testVirtualKey asks for, so the
// reconciler sees the live key as already matching the spec.
func writeTestKeyInfo(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"key_alias": testKeyAlias,
		"models":    []string{testKeyModel},
		"team_id":   testKeyTeam,
	})
}

func TestLiteLLMVirtualKeyReconciler_ReconcileCreatesSecretOnce(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer master", r.Header.Get("Authorization"))
		if r.URL.Path == testKeyInfoPath {
			writeTestKeyInfo(w)
			return
		}
		assert.Equal(t, "/key/generate", r.URL.Path)
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		requests = append(requests, request)
		_ = json.NewEncoder(w).Encode(map[string]string{testMasterSecretKey: "sk-generated"})
	}))
	defer srv.Close()

	key := testVirtualKey()
	key.Spec.SecretAnnotations = map[string]string{testReflectorAnnotation: testReflectorAllowed}
	key.Spec.SecretLabels = map[string]string{"app.kubernetes.io/part-of": testKeyAlias}
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
	assert.Equal(t, testReflectorAllowed, secret.Annotations[testReflectorAnnotation])
	assert.Equal(t, testReflectorAnnotation, secret.Annotations[managedAnnotationKeysAnnotation])
	assert.Equal(t, testKeyAlias, secret.Labels["app.kubernetes.io/part-of"])
	assert.Equal(t, "app.kubernetes.io/part-of", secret.Annotations[managedLabelKeysAnnotation])
	require.Len(t, requests, 1)
	assert.Equal(t, testKeyAlias, requests[0]["key_alias"])
	assert.Equal(t, []any{testKeyModel}, requests[0]["models"])
	assert.Equal(t, testKeyTeam, requests[0]["team_id"])
}

func TestLiteLLMVirtualKeyReconciler_ReconcileUpdatesChangedSpec(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case testKeyInfoPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"key_alias": testKeyAlias, "models": []string{"old"}, "team_id": testKeyTeam})
		case "/key/update":
			var request map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			assert.Equal(t, []any{testKeyModel}, request["models"])
		}
	}))
	defer srv.Close()

	key := testVirtualKey()
	proxy := testVirtualKeyProxy(srv.URL)
	masterKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testMasterSecretName, Namespace: key.Namespace}, Data: map[string][]byte{testMasterSecretKey: []byte("master")}}
	output := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Spec.SecretName, Namespace: key.Namespace}, Data: map[string][]byte{key.SecretDataKey(): []byte("sk-generated")}}
	r := testVirtualKeyReconciler(t, key, proxy, masterKey, output)
	require.NoError(t, ctrl.SetControllerReference(key, output, r.Scheme))
	require.NoError(t, r.Update(t.Context(), output))

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: key.Namespace, Name: key.Name}})
	require.NoError(t, err)
	assert.Equal(t, []string{testKeyInfoPath, "/key/update"}, paths)
}

func TestLiteLLMVirtualKeyReconciler_ReconcileUpdatesSecretMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, testKeyInfoPath, r.URL.Path)
		writeTestKeyInfo(w)
	}))
	defer srv.Close()

	key := testVirtualKey()
	key.Spec.SecretAnnotations = map[string]string{testReflectorAnnotation: testReflectorAllowed}
	key.Spec.SecretLabels = map[string]string{"tier": "backend"}
	proxy := testVirtualKeyProxy(srv.URL)
	masterKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testMasterSecretName, Namespace: key.Namespace}, Data: map[string][]byte{testMasterSecretKey: []byte("master")}}
	output := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Spec.SecretName,
			Namespace: key.Namespace,
			// A stale managed key plus metadata written by another controller, which
			// the operator must leave alone.
			Annotations: map[string]string{
				"stale":                         "yes",
				managedAnnotationKeysAnnotation: "stale",
				"reflector.v1.k8s.emberstack.com/reflected-version": "42",
			},
			Labels: map[string]string{"owner": "someone-else"},
		},
		Data: map[string][]byte{key.SecretDataKey(): []byte("sk-generated")},
	}
	r := testVirtualKeyReconciler(t, key, proxy, masterKey, output)
	require.NoError(t, ctrl.SetControllerReference(key, output, r.Scheme))
	require.NoError(t, r.Update(t.Context(), output))

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: key.Namespace, Name: key.Name}})
	require.NoError(t, err)

	var secret corev1.Secret
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Namespace: key.Namespace, Name: key.Spec.SecretName}, &secret))
	assert.Equal(t, testReflectorAllowed, secret.Annotations[testReflectorAnnotation])
	assert.Equal(t, "backend", secret.Labels["tier"])
	assert.NotContains(t, secret.Annotations, "stale", "a key dropped from the spec is removed")
	assert.Equal(t, "42", secret.Annotations["reflector.v1.k8s.emberstack.com/reflected-version"], "another controller's annotation survives")
	assert.Equal(t, "someone-else", secret.Labels["owner"])
	assert.Equal(t, []byte("sk-generated"), secret.Data[key.SecretDataKey()], "the key is not rotated")
}

func TestApplySecretMetadata(t *testing.T) {
	tests := []struct {
		name            string
		annotations     map[string]string
		labels          map[string]string
		specAnnotations map[string]string
		specLabels      map[string]string
		wantChanged     bool
		wantAnnotations map[string]string
		wantLabels      map[string]string
	}{
		{
			name:        "no metadata declared",
			annotations: map[string]string{testUnmanagedKey: testUnmanagedValue},
			wantLabels:  nil,
			wantAnnotations: map[string]string{
				testUnmanagedKey: testUnmanagedValue,
			},
		},
		{
			name:            "already applied",
			annotations:     map[string]string{"a": "1", managedAnnotationKeysAnnotation: "a"},
			specAnnotations: map[string]string{"a": "1"},
			wantAnnotations: map[string]string{"a": "1", managedAnnotationKeysAnnotation: "a"},
		},
		{
			name:            "value changed",
			annotations:     map[string]string{"a": "1", managedAnnotationKeysAnnotation: "a"},
			specAnnotations: map[string]string{"a": "2"},
			wantChanged:     true,
			wantAnnotations: map[string]string{"a": "2", managedAnnotationKeysAnnotation: "a"},
		},
		{
			name:            "all managed keys removed",
			annotations:     map[string]string{"a": "1", testUnmanagedKey: testUnmanagedValue, managedAnnotationKeysAnnotation: "a"},
			wantChanged:     true,
			wantAnnotations: map[string]string{testUnmanagedKey: testUnmanagedValue},
		},
		{
			name:        "unmanaged keys are never adopted",
			annotations: map[string]string{"a": "theirs"},
			wantAnnotations: map[string]string{
				"a": "theirs",
			},
		},
		{
			name:            "labels tracked separately from annotations",
			specAnnotations: map[string]string{"a": "1"},
			specLabels:      map[string]string{"b": "2", "c": "3"},
			wantChanged:     true,
			wantAnnotations: map[string]string{
				"a":                             "1",
				managedAnnotationKeysAnnotation: "a",
				managedLabelKeysAnnotation:      "b,c",
			},
			wantLabels: map[string]string{"b": "2", "c": "3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations, Labels: tt.labels}}
			key := testVirtualKey()
			key.Spec.SecretAnnotations = tt.specAnnotations
			key.Spec.SecretLabels = tt.specLabels

			assert.Equal(t, tt.wantChanged, applySecretMetadata(secret, key))
			assert.Equal(t, tt.wantAnnotations, secret.Annotations)
			assert.Equal(t, tt.wantLabels, secret.Labels)
			assert.False(t, applySecretMetadata(secret, key), "a second apply is a no-op")
		})
	}
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
