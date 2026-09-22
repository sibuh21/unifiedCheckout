package cybersource

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// GenerateMLEKeyPair generates a 2048-bit RSA key pair and a self-signed X.509 certificate
// compliant with Cybersource Message-Level Encryption (MLE) requirements.
// Returns the private key PEM, certificate PEM, single-line certificate base64 string, and any error.
func GenerateMLEKeyPair(orgID, requestKey string) (privateKeyPEM []byte, certPEM []byte, certBase64SingleLine string, err error) {
	if requestKey == "" {
		requestKey = "RequestKey"
	}
	if orgID == "" {
		return nil, nil, "", errors.New("organizationId cannot be empty")
	}

	// 1. Generate 2048-bit RSA private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to generate RSA key: %w", err)
	}

	// 2. Prepare X.509 Certificate Template (valid for 365 days)
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   requestKey,
			Organization: []string{orgID},
			Country:      []string{"US"},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageDataEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	// 3. Self-sign certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to create self-signed certificate: %w", err)
	}

	// 4. Encode Private Key to PKCS#8 PEM
	privBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to marshal private key to PKCS#8: %w", err)
	}
	privateKeyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privBytes,
	})

	// 5. Encode Certificate to PEM
	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	// 6. Single-line Base64 for Cybersource KMS pub field (no headers, no newlines)
	certBase64SingleLine = base64.StdEncoding.EncodeToString(certDER)

	return privateKeyPEM, certPEM, certBase64SingleLine, nil
}

// LoadPrivateKey loads an RSA private key from either a file path or raw PEM content.
// Supports both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY").
func LoadPrivateKey(pathOrPEM string) (*rsa.PrivateKey, error) {
	var pemData []byte
	trimmed := strings.TrimSpace(pathOrPEM)

	if strings.Contains(trimmed, "-----BEGIN") {
		pemData = []byte(trimmed)
	} else {
		data, err := os.ReadFile(pathOrPEM)
		if err != nil {
			return nil, fmt.Errorf("failed to read private key file: %w", err)
		}
		pemData = data
	}

	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("failed to decode PEM block containing private key")
	}

	// Try PKCS#8 first
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("parsed PKCS#8 key is not an RSA private key")
	}

	// Fallback to PKCS#1
	if rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rsaKey, nil
	}

	return nil, errors.New("could not parse private key as PKCS#8 or PKCS#1")
}

// IsJWE checks if a string appears to be a compact serialized JWE.
// JWE compact serialization consists of 5 base64url-encoded parts separated by 4 dots.
func IsJWE(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return false
	}
	parts := strings.Split(s, ".")
	return len(parts) == 5 && !strings.ContainsAny(s, " \r\n\t")
}

// DecryptJWE decrypts a compact serialized JWE string using the provided RSA private key.
// Cybersource uses RSA-OAEP / RSA-OAEP-256 for key encipherment and A256GCM for content encryption.
func DecryptJWE(compactJWE string, privateKey *rsa.PrivateKey) ([]byte, error) {
	if privateKey == nil {
		return nil, errors.New("private key cannot be nil")
	}

	object, err := jose.ParseEncrypted(
		strings.TrimSpace(compactJWE),
		[]jose.KeyAlgorithm{jose.RSA_OAEP, jose.RSA_OAEP_256},
		[]jose.ContentEncryption{jose.A256GCM, jose.A128GCM},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to parse compact JWE: %w", err)
	}

	decrypted, err := object.Decrypt(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt JWE payload: %w", err)
	}

	return decrypted, nil
}

// EncryptJWE creates a compact serialized JWE using RSA-OAEP and A256GCM.
// Useful for unit testing decryption locally.
func EncryptJWE(plaintext []byte, pubKey *rsa.PublicKey) (string, error) {
	if pubKey == nil {
		return "", errors.New("public key cannot be nil")
	}

	encrypter, err := jose.NewEncrypter(
		jose.A256GCM,
		jose.Recipient{
			Algorithm: jose.RSA_OAEP,
			Key:       pubKey,
		},
		(&jose.EncrypterOptions{}).WithType("JWE"),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create JWE encrypter: %w", err)
	}

	object, err := encrypter.Encrypt(plaintext)
	if err != nil {
		return "", fmt.Errorf("failed to encrypt payload: %w", err)
	}

	return object.CompactSerialize()
}
