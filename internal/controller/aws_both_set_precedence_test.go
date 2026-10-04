// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fake2 "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	"github.com/envoyproxy/ai-gateway/internal/controller/rotators"
	"github.com/envoyproxy/ai-gateway/internal/filterapi"
)

// TestAWSCredentialsBothSetPrecedenceAgreement guards against the divergence
// documented for BackendSecurityPolicyAWSCredentials when both credentialsFile
// and oidcExchangeToken are set. The CRD's XValidation now rejects that state at
// admission, but already-admitted objects must still behave consistently: both the
// rotation path (getBackendSecurityPolicyAuthOIDC / getBSPGeneratedSecretName) and
// the gateway auth-filter path (bspToFilterAPIBackendAuth) must select oidcExchangeToken
// and target the same generated secret ai-eg-bsp-<name>. The index function
// (backendSecurityPolicyIndexFunc) must likewise key on the BSP, not the static
// credentialsFile secret.
func TestAWSCredentialsBothSetPrecedenceAgreement(t *testing.T) {
	const namespace = "ns"
	bspName := "aws-both-set"
	bsp := &aigv1b1.BackendSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: bspName, Namespace: namespace},
		Spec: aigv1b1.BackendSecurityPolicySpec{
			Type: aigv1b1.BackendSecurityPolicyTypeAWSCredentials,
			AWSCredentials: &aigv1b1.BackendSecurityPolicyAWSCredentials{
				Region: "us-east-1",
				CredentialsFile: &aigv1b1.AWSCredentialsFile{
					SecretRef: &gwapiv1.SecretObjectReference{Name: "my-static-aws-credentials"},
				},
				OIDCExchangeToken: &aigv1b1.AWSOIDCExchangeToken{AwsRoleArn: "arn:aws:iam::123456789012:role/oidc"},
			},
		},
	}
	rotationSecretName := rotators.GetBSPSecretName(bspName) // ai-eg-bsp-<name>

	// --- Rotation path: OIDC is selected and writing targets the generated secret. ---
	oidc := getBackendSecurityPolicyAuthOIDC(&bsp.Spec)
	require.NotNil(t, oidc, "rotation path must select oidcExchangeToken when both are set")
	generatedSecretName := getBSPGeneratedSecretName(bsp)
	require.Equal(t, rotationSecretName, generatedSecretName,
		"rotation path must target the BSP generated secret, not the static credentialsFile secret")

	// --- Gateway path: the auth filter reads the SAME generated secret (OIDC wins). ---
	fakeClient := requireNewFakeClientWithIndexes(t)
	kube := fake2.NewClientset()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true, Level: zapcore.DebugLevel})))
	c := newTestGatewayController(fakeClient, kube, ctrl.Log, "envoy-gateway-system",
		"docker.io/envoyproxy/ai-gateway:latest", "info", false, nil, true)
	require.NoError(t, fakeClient.Create(t.Context(), bsp))

	// Distinct content per secret so the source is unambiguously identified.
	_, err := kube.CoreV1().Secrets(namespace).Create(t.Context(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-static-aws-credentials", Namespace: namespace},
			StringData: map[string]string{rotators.AwsCredentialsKey: "static-aws-creds"},
		}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kube.CoreV1().Secrets(namespace).Create(t.Context(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: rotationSecretName, Namespace: namespace},
			StringData: map[string]string{rotators.AwsCredentialsKey: "oidc-rotated-aws-creds"},
		}, metav1.CreateOptions{})
	require.NoError(t, err)

	got, err := c.bspToFilterAPIBackendAuth(t.Context(), bsp)
	require.NoError(t, err)
	require.Equal(t, &filterapi.BackendAuth{
		AWSAuth: &filterapi.AWSAuth{CredentialFileLiteral: "oidc-rotated-aws-creds", Region: "us-east-1"},
	}, got, "gateway path must read the OIDC-rotated secret, not the static credentialsFile secret")

	// The two paths now agree on the same secret.
	t.Logf("rotation secret (OIDC rotator writes here) = %q", rotationSecretName)
	t.Logf("gateway  secret (auth filter reads from)  = %q", rotationSecretName)

	// --- Index function: with both set, the BSP keys on itself (OIDC path), not the
	// static credentialsFile secret, so changes to ai-eg-bsp-<name> are mapped back. ---
	idx := backendSecurityPolicyIndexFunc(bsp)
	require.Contains(t, idx, backendSecurityPolicyKey(namespace, bspName),
		"index func must key on the BSP (OIDC) when both are set, not the static credentialsFile secret")
	for _, key := range idx {
		require.NotContains(t, key, "my-static-aws-credentials",
			"index func must NOT key on the static credentialsFile secret when OIDC is also set")
	}
}

// TestAWSCredentialsBothSetPrecedenceAgreement_OnlyCredentialsFile is a sanity
// complement: when only credentialsFile is set, the index key is the static secret
// (the credentialsFile path is still the sole path). This guards against
// over-broadening the OIDC-wins change.
func TestAWSCredentialsBothSetPrecedenceAgreement_OnlyCredentialsFile(t *testing.T) {
	const namespace = "ns"
	bsp := &aigv1b1.BackendSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-static-only", Namespace: namespace},
		Spec: aigv1b1.BackendSecurityPolicySpec{
			Type: aigv1b1.BackendSecurityPolicyTypeAWSCredentials,
			AWSCredentials: &aigv1b1.BackendSecurityPolicyAWSCredentials{
				Region: "us-east-1",
				CredentialsFile: &aigv1b1.AWSCredentialsFile{
					SecretRef: &gwapiv1.SecretObjectReference{Name: "my-static-aws-credentials"},
				},
			},
		},
	}
	require.Nil(t, getBackendSecurityPolicyAuthOIDC(&bsp.Spec))
	require.Empty(t, getBSPGeneratedSecretName(bsp))
	idx := backendSecurityPolicyIndexFunc(bsp)
	require.Contains(t, idx, "my-static-aws-credentials."+namespace)
}
