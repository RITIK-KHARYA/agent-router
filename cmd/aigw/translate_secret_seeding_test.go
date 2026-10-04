// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
)

// TestTranslateCustomResourceObjects_SeedsUserSecretsIntoFakeClient is the regression test for
// the misleading "secrets <name> not found" panic on the Azure/GCP rotation paths of `aigw run`.
//
// translateCustomResourceObjects wires two fake Kubernetes clients:
//
//   - fakeClientSet (client-go), used by the Gateway controller's getSecretData, and
//   - fakeClient (controller-runtime), used as c.client by the BackendSecurityPolicyController,
//     which reads credential secrets via rotators.LookupSecret(ctx, c.client, ...) for the
//     Azure ClientSecretRef and GCP credentialsFile rotation paths.
//
// The seeding loop previously created user-defined Secrets only in fakeClientSet, so the BSP
// controller's LookupSecret deterministically returned NotFound and the harness panicked with
// "secrets <name> not found" — naming the Secret the user *did* supply — instead of an accurate
// credential-construction error. This test verifies the fix seeds user Secrets into both fakes.
func TestTranslateCustomResourceObjects_SeedsUserSecretsIntoFakeClient(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "user-supplied-secret"},
		Data:       map[string][]byte{"apiKey": []byte("super-secret-value")},
	}
	fakeClient, _, _, _, _, _, _, _, _, err := translateCustomResourceObjects(
		t.Context(), nil, nil, nil, nil, nil, nil,
		[]*corev1.Secret{secret}, slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)

	// The BSP controller reads credential secrets via c.client (= fakeClient). The fix seeds
	// user-defined secrets into fakeClient too, so the secret must be retrievable from it.
	// Before the fix this Get returned NotFound — the root cause of the misleading panic.
	got := &corev1.Secret{}
	err = fakeClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "user-supplied-secret"}, got)
	require.NoError(t, err)
	require.Equal(t, []byte("super-secret-value"), got.Data["apiKey"])
	require.Equal(t, "default", got.Namespace)
	require.Equal(t, "user-supplied-secret", got.Name)
}

// TestTranslateCustomResourceObjects_AzureGCPRotation_NoMisleadingNotFound drives the real Azure
// ClientSecretRef and GCP credentialsFile rotation paths and asserts that, after the seeding
// fix, the BSP controller finds the user-supplied Secret in fakeClient and surfaces an accurate
// in-codebase error (missing expected key) instead of the misleading "secrets <name> not found".
//
// The user Secret is intentionally supplied without the provider-specific data key, so the
// reconcile stops at the key-existence check inside our own code (backend_security_policy.go) —
// a stable message that does not depend on third-party SDK wording or network behavior.
func TestTranslateCustomResourceObjects_AzureGCPRotation_NoMisleadingNotFound(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bsp           *aigv1b1.BackendSecurityPolicy
		secret        *corev1.Secret
		wantSubstring string
	}{
		{
			name: "Azure ClientSecretRef",
			bsp: &aigv1b1.BackendSecurityPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "azure-bsp"},
				Spec: aigv1b1.BackendSecurityPolicySpec{
					Type: aigv1b1.BackendSecurityPolicyTypeAzureCredentials,
					AzureCredentials: &aigv1b1.BackendSecurityPolicyAzureCredentials{
						ClientID:        "client-id",
						TenantID:        "tenant",
						ClientSecretRef: &gwapiv1.SecretObjectReference{Name: "azure-secret"},
					},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "azure-secret"},
				Data:       map[string][]byte{"wrong-key": []byte("oops")},
			},
			wantSubstring: "missing azure client secret key client-secret",
		},
		{
			name: "GCP credentialsFile",
			bsp: &aigv1b1.BackendSecurityPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gcp-bsp"},
				Spec: aigv1b1.BackendSecurityPolicySpec{
					Type: aigv1b1.BackendSecurityPolicyTypeGCPCredentials,
					GCPCredentials: &aigv1b1.BackendSecurityPolicyGCPCredentials{
						ProjectName: "my-project",
						Region:      "us-central1",
						CredentialsFile: &aigv1b1.GCPCredentialsFile{
							SecretRef: &gwapiv1.SecretObjectReference{Name: "gcp-secret"},
						},
					},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gcp-secret"},
				Data:       map[string][]byte{"wrong-key": []byte("oops")},
			},
			wantSubstring: "missing gcp service account key service_account.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			panicMsg := driveTranslatePanicking(t.Context(), t, tc.bsp, tc.secret)
			// After the fix, the BSP controller finds the user-supplied secret in fakeClient and
			// reports the accurate, in-codebase "missing ... key" error.
			require.Contains(t, panicMsg, tc.wantSubstring)
			// The misleading "secrets <name> not found" panic — the actual reported bug — must
			// be gone. Either the user supplied the Secret (its presence is now visible to
			// fakeClient) and the failure is a credential-construction error.
			require.NotContains(t, panicMsg, "not found")
		})
	}
}

// driveTranslatePanicking runs the real translateCustomResourceObjects with the given BSP and
// matching user Secret, returning the panic value formatted as a string. The translate path
// panics (via mustReconcile) when reconcile returns an error; that panic is the behavior under
// test for the rotation paths.
func driveTranslatePanicking(ctx context.Context, t *testing.T, bsp *aigv1b1.BackendSecurityPolicy, secret *corev1.Secret) string {
	t.Helper()
	var panicVal any
	func() {
		defer func() { panicVal = recover() }()
		_, _, _, _, _, _, _, _, _, err := translateCustomResourceObjects(
			ctx, nil, nil, nil,
			[]*aigv1b1.BackendSecurityPolicy{bsp}, nil, nil,
			[]*corev1.Secret{secret}, slog.New(slog.DiscardHandler),
		)
		t.Fatalf("expected a panic from the rotation path, got return err=%v", err)
	}()
	require.NotNil(t, panicVal, "expected a panic from the rotation path, got none")
	return fmt.Sprintf("%v", panicVal)
}
