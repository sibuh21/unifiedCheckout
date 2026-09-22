package cybersource

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestMLEKeyGenerationAndDecryption(t *testing.T) {
	orgID := "test_merchant_123"
	reqKey := "test_api_key_456"

	// 1. Generate key pair and certificate
	privPEM, certPEM, certBase64, err := GenerateMLEKeyPair(orgID, reqKey)
	if err != nil {
		t.Fatalf("GenerateMLEKeyPair failed: %v", err)
	}

	if len(privPEM) == 0 || len(certPEM) == 0 || len(certBase64) == 0 {
		t.Fatal("GenerateMLEKeyPair returned empty output")
	}

	// 2. Verify certificate subject
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("Failed to decode cert PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse X.509 certificate: %v", err)
	}
	if cert.Subject.CommonName != reqKey {
		t.Errorf("Expected Subject.CommonName to be %s, got %s", reqKey, cert.Subject.CommonName)
	}
	if len(cert.Subject.Organization) == 0 || cert.Subject.Organization[0] != orgID {
		t.Errorf("Expected Subject.Organization to be %s, got %v", orgID, cert.Subject.Organization)
	}

	// 3. Load private key
	privKey, err := LoadPrivateKey(string(privPEM))
	if err != nil {
		t.Fatalf("LoadPrivateKey failed: %v", err)
	}

	// 4. Test JWE Encryption & Decryption roundtrip
	mockPayload := []byte(`{"eventType":"uc.orders.transactionresults","orderId":"ORD-999","status":"AUTHORIZED"}`)
	compactJWE, err := EncryptJWE(mockPayload, &privKey.PublicKey)
	if err != nil {
		t.Fatalf("EncryptJWE failed: %v", err)
	}

	if !IsJWE(compactJWE) {
		t.Errorf("IsJWE returned false for valid JWE: %s", compactJWE)
	}
	if IsJWE(string(mockPayload)) {
		t.Error("IsJWE returned true for regular JSON")
	}

	decrypted, err := DecryptJWE(compactJWE, privKey)
	if err != nil {
		t.Fatalf("DecryptJWE failed: %v", err)
	}

	if string(decrypted) != string(mockPayload) {
		t.Errorf("Decrypted payload mismatch.\nExpected: %s\nGot: %s", string(mockPayload), string(decrypted))
	}
}
