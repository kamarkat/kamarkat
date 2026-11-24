package openbao

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ListenerCASecret holds the listener CA's key, in cert-manager's cluster-resource
	// namespace, backing ClusterIssuer openbao-listener. Only cert-manager reads it.
	ListenerCASecret    = "openbao-listener-ca"
	certManagerNS       = "cert-manager"
	ListenerTrustConfig = "openbao-listener-trust"
	listenerCAValidity  = 5 * 365 * 24 * time.Hour
)

// ListenerTrustNamespaces are where in-cluster OpenBao clients read the listener CA
// certificate: OpenBao's own namespace (hub-operator's client and the Gateway's
// backend policy read it there), ESO's, and cert-manager's.
var ListenerTrustNamespaces = []string{Namespace, "platform-ops", certManagerNS}

// EnsureListenerCA creates the listener CA if absent and (re)writes its public
// certificate into ListenerTrustNamespaces (ADR-117 §2).
//
// The CA is independent of OpenBao's PKI, so OpenBao's start, unseal and recovery
// never wait on OpenBao. It is created once per cluster (Class N): an existing CA is
// kept, never regenerated, because every listener certificate chains to it.
// Consumers are given only the public certificate, as a ConfigMap; no consumer is
// granted the Secret holding the key, because RBAC grants whole Secrets.
func EnsureListenerCA(ctx context.Context, cs kubernetes.Interface, now time.Time) error {
	sec, err := cs.CoreV1().Secrets(certManagerNS).Get(ctx, ListenerCASecret, metav1.GetOptions{})
	var certPEM []byte
	switch {
	case apierrors.IsNotFound(err):
		certPEM, keyPEM, genErr := generateListenerCA(now)
		if genErr != nil {
			return genErr
		}
		_, err = cs.CoreV1().Secrets(certManagerNS).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ListenerCASecret,
				Namespace: certManagerNS,
				Labels:    map[string]string{"app.kubernetes.io/part-of": "openbao", "app.kubernetes.io/managed-by": "kakt"},
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create %s/%s: %w", certManagerNS, ListenerCASecret, err)
		}
		return writeTrust(ctx, cs, certPEM)
	case err != nil:
		return fmt.Errorf("read %s/%s: %w", certManagerNS, ListenerCASecret, err)
	default:
		certPEM = sec.Data[corev1.TLSCertKey]
		if _, perr := parseCertPEM(certPEM); perr != nil {
			return fmt.Errorf("%s/%s holds no usable CA certificate: %w", certManagerNS, ListenerCASecret, perr)
		}
		return writeTrust(ctx, cs, certPEM)
	}
}

func generateListenerCA(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate listener CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "OpenBao listener CA", Organization: []string{"Kamarkat"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(listenerCAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("self-sign listener CA: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// writeTrust writes the public certificate as ConfigMap openbao-listener-trust
// (key ca.crt) in every trust namespace. A rollover (ADR-035 dual trust) writes the
// old and new certificates concatenated through the same function.
func writeTrust(ctx context.Context, cs kubernetes.Interface, certPEM []byte) error {
	for _, ns := range ListenerTrustNamespaces {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ListenerTrustConfig,
				Namespace: ns,
				Labels:    map[string]string{"app.kubernetes.io/part-of": "openbao", "app.kubernetes.io/managed-by": "kakt"},
			},
			Data: map[string]string{"ca.crt": string(certPEM)},
		}
		existing, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, ListenerTrustConfig, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = cs.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		case err == nil:
			existing.Data = cm.Data
			_, err = cs.CoreV1().ConfigMaps(ns).Update(ctx, existing, metav1.UpdateOptions{})
		}
		if err != nil {
			return fmt.Errorf("write %s/%s: %w", ns, ListenerTrustConfig, err)
		}
	}
	return nil
}
