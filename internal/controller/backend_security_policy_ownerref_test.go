// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fake2 "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	"github.com/envoyproxy/ai-gateway/internal/controller/rotators"
	"github.com/envoyproxy/ai-gateway/internal/json"
	internaltesting "github.com/envoyproxy/ai-gateway/internal/testing"
)

// reproBSPController builds a BackendSecurityPolicyController over a fake client seeded with objs.
// It uses the same construction path as production callers (NewBackendSecurityPolicyController)
// so that Reconcile exercises the real reconcile → handleFinalizer → rotateCredential →
// executeRotation → owner-reference block path.
func reproBSPController(t *testing.T, objs ...client.Object) (*BackendSecurityPolicyController, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(Scheme).
		WithStatusSubresource(&aigv1b1.BackendSecurityPolicy{}).
		WithObjects(objs...).Build()
	eventCh := internaltesting.NewControllerEventChan[*aigv1b1.AIServiceBackend]()
	c := NewBackendSecurityPolicyController(cl, fake2.NewClientset(), ctrl.Log, eventCh.Ch, nil)
	return c, cl
}

// assertBSPGeneratedSecretOwnerRef asserts that the controller-generated Secret referenced by the
// BackendSecurityPolicy has a controller OwnerReference pointing back at the BSP, proving the
// owner-reference block in rotateCredential ran and set up Kubernetes garbage collection.
func assertBSPGeneratedSecretOwnerRef(t *testing.T, cl client.Client, bsp *aigv1b1.BackendSecurityPolicy, expectedTokenKey, expectedTokenValue string) {
	t.Helper()
	genName := rotators.GetBSPSecretName(bsp.Name)
	// The fix is exactly that getBSPGeneratedSecretName returns the generated secret name for the
	// Azure ClientSecretRef path and the GCP CredentialsFile path. Assert it directly so the test
	// fails with a clear regression message if the helper is ever broken again.
	require.Equal(t, genName, getBSPGeneratedSecretName(bsp),
		"getBSPGeneratedSecretName must return the generated secret name so the owner-ref block runs")

	genSecret := &corev1.Secret{}
	require.NoError(t, cl.Get(t.Context(),
		types.NamespacedName{Namespace: bsp.Namespace, Name: genName}, genSecret))
	require.Equal(t, expectedTokenValue, string(genSecret.Data[expectedTokenKey]),
		"generated Secret should contain the rotated credential")

	// The fix: the controller must set a controller OwnerReference so K8s garbage-collects the
	// generated Secret when the BackendSecurityPolicy is deleted.
	require.Len(t, genSecret.OwnerReferences, 1,
		"generated Secret must have exactly one OwnerReference (the BackendSecurityPolicy)")
	owner := genSecret.OwnerReferences[0]
	require.True(t, owner.Controller != nil && *owner.Controller,
		"OwnerReference must mark the BackendSecurityPolicy as the controller")
	require.Equal(t, bsp.Name, owner.Name)
	require.Equal(t, bsp.UID, owner.UID,
		"OwnerReference must point at the BackendSecurityPolicy UID so K8s GC targets it on deletion")
	// Sanity-check the owner GVK is BackendSecurityPolicy via the APIVersion/Kind fields.
	gvk := schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)
	require.Equal(t, aigv1b1.SchemeGroupVersion.WithKind("BackendSecurityPolicy"), gvk)
}

// buildFakeGCPSAKeyJSON returns a valid GCP service-account JSON whose token_uri is set to
// tokenURL, causing google.CredentialsFromJSON + Token() to POST a JWT-bearer assertion to that
// URL instead of the real oauth2.googleapis.com endpoint.
func buildFakeGCPSAKeyJSON(t *testing.T, tokenURL string) []byte {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyDER := x509.MarshalPKCS1PrivateKey(rsaKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	sa := map[string]any{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": "key1",
		"private_key":    string(keyPEM),
		"client_email":   "test@test-project.iam.gserviceaccount.com",
		"client_id":      "123",
		"auth_uri":       "https://accounts.google.com/o/oauth2/auth",
		"token_uri":      tokenURL,
	}
	raw, err := json.Marshal(sa)
	require.NoError(t, err)
	return raw
}

// TestBSP_GCP_CredentialsFile_ReconcileSetsOwnerRef is a TRUE end-to-end test driving c.Reconcile
// (the production entry point) for a GCP CredentialsFile BackendSecurityPolicy. It exercises the
// full production path: Reconcile → reconcile → handleFinalizer → rotateCredential →
// validateGCPCredentialsParams → referenceGrantValidator.validateSecretReference → LookupSecret →
// NewGCPTokenProvider → NewGCPTokenRotator → executeRotation → Rotate creates ai-eg-bsp-<name> →
// owner-ref block at backend_security_policy.go (getBSPGeneratedSecretName now returns the name
// → SetControllerReference is called).
//
// The GCP provider's network call is redirected to a local httptest.Server via the SA JSON's
// token_uri field — no production code is modified.
//
// Regression for the bug where getBSPGeneratedSecretName returned "" for the GCP CredentialsFile
// path (keyed the guard on WorkloadIdentityFederationConfig instead of CredentialsFile), so the
// owner-ref block was silently skipped and the generated Secret was orphaned on BSP deletion.
func TestBSP_GCP_CredentialsFile_ReconcileSetsOwnerRef(t *testing.T) {
	var tokCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&tokCount, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"gcp-fake-token-%d","token_type":"Bearer","expires_in":3600}`, n)
	}))
	defer srv.Close()

	saJSON := buildFakeGCPSAKeyJSON(t, srv.URL+"/token")

	ns := "default"
	bspName := "gcp-bsp-ownerref"
	saKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-sa-key", Namespace: ns},
		Data:       map[string][]byte{rotators.GCPServiceAccountJSON: saJSON},
	}
	c, cl := reproBSPController(t, saKeySecret)

	bsp := &aigv1b1.BackendSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: bspName, Namespace: ns},
		Spec: aigv1b1.BackendSecurityPolicySpec{
			Type: aigv1b1.BackendSecurityPolicyTypeGCPCredentials,
			GCPCredentials: &aigv1b1.BackendSecurityPolicyGCPCredentials{
				ProjectName: "test-project",
				Region:      "us-central1",
				CredentialsFile: &aigv1b1.GCPCredentialsFile{
					SecretRef: &gwapiv1.SecretObjectReference{
						Name:      "gcp-sa-key",
						Namespace: ptr.To[gwapiv1.Namespace](gwapiv1.Namespace(ns)),
					},
				},
			},
		},
	}
	require.NoError(t, cl.Create(t.Context(), bsp))

	// Drive c.Reconcile — the REAL production entry point through the entire rotateCredential
	// path including the owner-ref block.
	res, err := c.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: bspName},
	})
	require.NoError(t, err)
	require.NotZero(t, res.RequeueAfter)

	// Re-fetch the BSP so it carries the UID assigned by the fake client before asserting on
	// the OwnerReference UID.
	require.NoError(t, cl.Get(t.Context(),
		types.NamespacedName{Namespace: ns, Name: bspName}, bsp))

	// The fix: the generated Secret has a controller OwnerReference to the BSP.
	assertBSPGeneratedSecretOwnerRef(t, cl, bsp, "gcpAccessToken", "gcp-fake-token-1")

	// After deleting the BSP and Reconciling the deletion path, the BSP is gone. The fake client
	// does not run a garbage collector, so the generated Secret still exists and retains the
	// OwnerReference; in a real cluster, Kubernetes GC uses that OwnerReference to delete the
	// generated Secret. Assert the OwnerReference survived so the precondition for GC is in place.
	require.NoError(t, cl.Delete(t.Context(), bsp))
	_, err = c.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: bspName},
	})
	require.NoError(t, err)

	// BSP should now be gone (finalizer was removed by handleFinalizer).
	require.Error(t, cl.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: bspName}, &aigv1b1.BackendSecurityPolicy{}),
		"BSP should be deleted after Reconcile of the deletion path")

	// Generated Secret retains the OwnerReference set by the fix; real K8s GC would delete it.
	genName := rotators.GetBSPSecretName(bspName)
	genSecret := &corev1.Secret{}
	require.NoError(t, cl.Get(t.Context(),
		types.NamespacedName{Namespace: ns, Name: genName}, genSecret))
	require.Len(t, genSecret.OwnerReferences, 1,
		"generated Secret must retain its controller OwnerReference so Kubernetes GC cleans it up")
	require.True(t, *genSecret.OwnerReferences[0].Controller)
}

// startFakeAzureADProxy starts an HTTP CONNECT proxy that intercepts HTTPS traffic to
// login.microsoftonline.com, presents a self-signed cert for that host (trusted via SSL_CERT_FILE,
// which Go's x509.SystemCertPool honors on Linux), and serves fake Azure AD instance-discovery,
// OpenID-configuration, and token responses inside the TLS tunnel. The proxy URL returned should be
// set as AI_GATEWAY_AZURE_PROXY_URL (consumed by GetClientSecretCredentialOptions in
// azure_client_secret_token_provider.go).
//
// This redirects all Azure SDK HTTPS traffic to the fake server without modifying production code.
// Instance discovery uses a hardcoded https://login.microsoftonline.com/common/discovery/instance
// endpoint, so simply overriding AZURE_AUTHORITY_HOST isn't sufficient — the proxy must intercept
// traffic to login.microsoftonline.com itself.
func startFakeAzureADProxy(t *testing.T) (proxy *httptest.Server) {
	t.Helper()

	azKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	azCertTPL := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "login.microsoftonline.com"},
		DNSNames:     []string{"login.microsoftonline.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	azCertDER, err := x509.CreateCertificate(rand.Reader, &azCertTPL, &azCertTPL, &azKey.PublicKey, azKey)
	require.NoError(t, err)
	azCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: azCertDER})

	// Trust the proxy's cert via SSL_CERT_FILE so the Azure SDK's default transport validates the
	// TLS handshake inside the CONNECT tunnel.
	certPath := path.Join(t.TempDir(), "az-cert.pem")
	require.NoError(t, os.WriteFile(certPath, azCertPEM, 0o600))
	t.Setenv("SSL_CERT_FILE", certPath)

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{azCertDER}, PrivateKey: azKey}}}
	var tokCount int32
	proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "this proxy only supports CONNECT", http.StatusMethodNotAllowed)
			return
		}
		hj, ok := w.(http.Hijacker)
		require.True(t, ok, "proxy server must support hijacking")
		conn, _, err := hj.Hijack()
		require.NoError(t, err)
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		tlsConn := tls.Server(conn, tlsConfig)
		require.NoError(t, tlsConn.Handshake())
		defer tlsConn.Close()
		reader := bufio.NewReader(tlsConn)
		for {
			req, rerr := http.ReadRequest(reader)
			if rerr != nil {
				return
			}
			n := int32(0)
			if req.Method == http.MethodPost {
				n = atomic.AddInt32(&tokCount, 1)
			}
			var body string
			isTokenResp := false
			switch {
			case strings.Contains(req.URL.Path, "/discovery/"):
				body = `{"tenant_discovery_endpoint":"https://login.microsoftonline.com/tenant-id/v2.0/.well-known/openid-configuration"}`
			case req.Method == http.MethodGet:
				body = `{"authorization_endpoint":"https://login.microsoftonline.com/tenant-id/oauth2/v2.0/authorize","token_endpoint":"https://login.microsoftonline.com/tenant-id/oauth2/v2.0/token","issuer":"https://login.microsoftonline.com/tenant-id/v2.0"}`
			default:
				isTokenResp = true
				body = fmt.Sprintf(`{"access_token":"azure-fake-token-%d","token_type":"Bearer","expires_in":3600,"expires_on":%d}`,
					n, 9999999999)
			}
			_, _ = fmt.Fprintf(tlsConn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
				len(body), body)
			if isTokenResp {
				return
			}
		}
	}))
	return proxy
}

// TestBSP_Azure_ClientSecret_ReconcileSetsOwnerRef is a TRUE end-to-end test driving c.Reconcile
// for an Azure ClientSecretRef BackendSecurityPolicy. All Azure SDK HTTPS traffic is intercepted
// by a CONNECT proxy (AI_GATEWAY_AZURE_PROXY_URL, consumed by GetClientSecretCredentialOptions at
// azure_client_secret_token_provider.go) that presents a self-signed cert for
// login.microsoftonline.com (trusted via SSL_CERT_FILE). The proxy serves fake instance-discovery,
// OpenID-configuration, and token responses. No production code is modified.
//
// Regression for the bug where getBSPGeneratedSecretName returned "" for the Azure ClientSecretRef
// path (keyed the guard on OIDCExchangeToken instead of including ClientSecretRef), so the
// owner-ref block was silently skipped and the generated Secret was orphaned on BSP deletion.
func TestBSP_Azure_ClientSecret_ReconcileSetsOwnerRef(t *testing.T) {
	proxy := startFakeAzureADProxy(t)
	defer proxy.Close()
	// Route all Azure SDK HTTPS traffic through the fake proxy, which intercepts CONNECT for
	// login.microsoftonline.com and serves fake Azure AD responses inside the TLS tunnel.
	t.Setenv("AI_GATEWAY_AZURE_PROXY_URL", proxy.URL)

	ns := "default"
	bspName := "azure-bsp-ownerref"
	clientSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-client-secret", Namespace: ns},
		Data:       map[string][]byte{clientSecretKey: []byte("super-secret")},
	}
	c, cl := reproBSPController(t, clientSecret)

	bsp := &aigv1b1.BackendSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: bspName, Namespace: ns},
		Spec: aigv1b1.BackendSecurityPolicySpec{
			Type: aigv1b1.BackendSecurityPolicyTypeAzureCredentials,
			AzureCredentials: &aigv1b1.BackendSecurityPolicyAzureCredentials{
				ClientID: "client-id",
				TenantID: "tenant-id",
				ClientSecretRef: &gwapiv1.SecretObjectReference{
					Name:      "azure-client-secret",
					Namespace: ptr.To[gwapiv1.Namespace](gwapiv1.Namespace(ns)),
				},
			},
		},
	}
	require.NoError(t, cl.Create(t.Context(), bsp))

	// Drive c.Reconcile — the REAL production entry point through the entire rotateCredential
	// path including the owner-ref block.
	res, err := c.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: bspName},
	})
	require.NoError(t, err)
	require.NotZero(t, res.RequeueAfter)

	// Re-fetch the BSP so it carries the UID assigned by the fake client before asserting on
	// the OwnerReference UID.
	require.NoError(t, cl.Get(t.Context(),
		types.NamespacedName{Namespace: ns, Name: bspName}, bsp))

	// The fix: the generated Secret has a controller OwnerReference to the BSP.
	assertBSPGeneratedSecretOwnerRef(t, cl, bsp, rotators.AzureAccessTokenKey, "azure-fake-token-1")

	// After deleting the BSP and Reconciling the deletion path, the BSP is gone. The fake client
	// does not run a garbage collector, so the generated Secret still exists and retains the
	// OwnerReference; in a real cluster, Kubernetes GC uses that OwnerReference to delete the
	// generated Secret.
	require.NoError(t, cl.Delete(t.Context(), bsp))
	_, err = c.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: bspName},
	})
	require.NoError(t, err)

	require.Error(t, cl.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: bspName}, &aigv1b1.BackendSecurityPolicy{}),
		"BSP should be deleted after Reconcile of the deletion path")

	genName := rotators.GetBSPSecretName(bspName)
	genSecret := &corev1.Secret{}
	require.NoError(t, cl.Get(t.Context(),
		types.NamespacedName{Namespace: ns, Name: genName}, genSecret))
	require.Len(t, genSecret.OwnerReferences, 1,
		"generated Secret must retain its controller OwnerReference so Kubernetes GC cleans it up")
	require.True(t, *genSecret.OwnerReferences[0].Controller)
}
