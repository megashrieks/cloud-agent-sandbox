// Package proxy assigns sandboxes to the MITM proxy pool and provides the
// proxy's CA certificate for injection into sandbox trust stores.
//
// Credential model: this package NEVER handles tokens. Real credentials live
// only inside the user-configured mitmproxy addon. Here we only decide which
// proxy endpoint a sandbox routes through and which CA it must trust.
//
// Topology: all pool replicas share ONE CA (provisioned as a Kubernetes Secret
// and mounted into every mitmproxy pod), so a sandbox can talk to any replica
// behind the Service and still trust it. The endpoint is the stable proxy
// Service DNS name; the Service load-balances across the pool (pool-per-group).
package proxy

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/megashrieks/cloud-agent-sandbox/internal/config"
)

// CASecretName is the Secret holding the shared proxy CA (cert + key). The
// mitmproxy Deployment mounts it as its confdir; this package reads the cert
// from it to inject into sandboxes.
const CASecretName = "mitmproxy-ca"

// caCertKey / caKeyKey are the keys within the CA Secret. These filenames match
// what mitmproxy expects in its confdir.
const (
	caCertKey = "mitmproxy-ca-cert.pem"
	caKeyKey  = "mitmproxy-ca.pem"
)

// ServiceAssigner implements manager.ProxyAssigner using the shared proxy
// Service endpoint and shared CA Secret.
type ServiceAssigner struct {
	cs        kubernetes.Interface
	namespace string
	endpoint  string
	caCert    []byte
}

// NewServiceAssigner builds an assigner. It reads the shared CA cert from the
// CA Secret once at startup and caches it (the CA is stable for the deployment).
func NewServiceAssigner(ctx context.Context, cs kubernetes.Interface, cfg config.ProxyConfig, namespace string) (*ServiceAssigner, error) {
	endpoint := fmt.Sprintf("%s.%s.svc.cluster.local:%d", cfg.ServiceName, namespace, cfg.Port)
	ca, err := readCACert(ctx, cs, namespace)
	if err != nil {
		return nil, err
	}
	return &ServiceAssigner{
		cs:        cs,
		namespace: namespace,
		endpoint:  endpoint,
		caCert:    ca,
	}, nil
}

// Assign returns the shared proxy endpoint and CA. The proxyID is the Service
// name (the pool is fronted by one Service).
func (a *ServiceAssigner) Assign(_ context.Context, _ string) (string, []byte, string, error) {
	if len(a.caCert) == 0 {
		return "", nil, "", errors.New("proxy CA not available")
	}
	return a.endpoint, a.caCert, "mitmproxy-pool", nil
}

// Release is a no-op: assignments to a shared Service hold no per-session state.
func (a *ServiceAssigner) Release(_ context.Context, _ string) {}

// readCACert loads the PEM CA cert from the shared CA Secret.
func readCACert(ctx context.Context, cs kubernetes.Interface, namespace string) ([]byte, error) {
	sec, err := cs.CoreV1().Secrets(namespace).Get(ctx, CASecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read CA secret %s/%s: %w", namespace, CASecretName, err)
	}
	cert, ok := sec.Data[caCertKey]
	if !ok || len(cert) == 0 {
		return nil, fmt.Errorf("CA secret %s missing key %s", CASecretName, caCertKey)
	}
	return cert, nil
}

// EnsureCASecret creates the shared proxy CA Secret if it does not already
// exist, generating a fresh CA keypair. Idempotent: if the Secret exists it is
// left untouched (so the CA is stable across restarts). Intended to be called
// once at startup / by an init job before the mitmproxy pods start.
func EnsureCASecret(ctx context.Context, cs kubernetes.Interface, namespace string) error {
	_, err := cs.CoreV1().Secrets(namespace).Get(ctx, CASecretName, metav1.GetOptions{})
	if err == nil {
		return nil // already exists, keep the stable CA
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("check CA secret: %w", err)
	}

	certPEM, keyPEM, err := generateCA()
	if err != nil {
		return fmt.Errorf("generate CA: %w", err)
	}
	// mitmproxy expects a single PEM file (cert + key concatenated) named
	// mitmproxy-ca.pem, plus the cert-only file for distribution.
	combined := append(append([]byte{}, keyPEM...), certPEM...)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CASecretName,
			Namespace: namespace,
			Labels:    map[string]string{"app": "mitmproxy"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			caCertKey: certPEM,
			caKeyKey:  combined,
		},
	}
	if _, err := cs.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create CA secret: %w", err)
	}
	return nil
}
