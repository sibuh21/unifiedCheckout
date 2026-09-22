package cybersource

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Client holds Cybersource REST API credentials and provides methods
// to send authenticated requests using HTTP Signature (Cavage) authentication.
type Client struct {
	MerchantID string
	KeyID      string
	SecretKey  string
	Host       string // e.g. "apitest.cybersource.com" for sandbox
}

// NewClient creates a new Cybersource API client with the given credentials.
func NewClient(merchantID, keyID, secretKey, host string) *Client {
	return &Client{
		MerchantID: merchantID,
		KeyID:      keyID,
		SecretKey:  secretKey,
		Host:       host,
	}
}

// GenerateSignature creates the Cavage HTTP Signature header and optional Digest
// header for a Cybersource REST API request.
func (c *Client) GenerateSignature(method, path string, bodyBytes []byte, date string) (digest string, signatureHeader string) {
	methodLower := strings.ToLower(method)

	decodedSecret, _ := base64.StdEncoding.DecodeString(c.SecretKey)
	mac := hmac.New(sha256.New, decodedSecret)

	if len(bodyBytes) > 0 {
		hasher := sha256.New()
		hasher.Write(bodyBytes)
		digest = "SHA-256=" + base64.StdEncoding.EncodeToString(hasher.Sum(nil))

		sigString := fmt.Sprintf("host: %s\ndate: %s\n(request-target): %s %s\ndigest: %s\nv-c-merchant-id: %s",
			c.Host, date, methodLower, path, digest, c.MerchantID)

		mac.Write([]byte(sigString))
		signatureHash := base64.StdEncoding.EncodeToString(mac.Sum(nil))

		signatureHeader = fmt.Sprintf(`keyid="%s", algorithm="HmacSHA256", headers="host date (request-target) digest v-c-merchant-id", signature="%s"`,
			c.KeyID, signatureHash)
	} else {
		sigString := fmt.Sprintf("host: %s\ndate: %s\n(request-target): %s %s\nv-c-merchant-id: %s",
			c.Host, date, methodLower, path, c.MerchantID)

		mac.Write([]byte(sigString))
		signatureHash := base64.StdEncoding.EncodeToString(mac.Sum(nil))

		signatureHeader = fmt.Sprintf(`keyid="%s", algorithm="HmacSHA256", headers="host date (request-target) v-c-merchant-id", signature="%s"`,
			c.KeyID, signatureHash)
	}

	return digest, signatureHeader
}

// SendRequest executes a signed HTTP request against the Cybersource REST API.
// The payload is marshaled to JSON if non-nil. Returns the HTTP response, response body bytes, and any error.
func (c *Client) SendRequest(method, path string, payload interface{}) (*http.Response, []byte, error) {
	url := "https://" + c.Host + path

	var bodyBytes []byte
	if payload != nil {
		var err error
		bodyBytes, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal payload: %w", err)
		}
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	digest, signature := c.GenerateSignature(method, path, bodyBytes, date)

	var reqBody io.Reader
	if len(bodyBytes) > 0 {
		reqBody = bytes.NewBuffer(bodyBytes)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("v-c-merchant-id", c.MerchantID)
	req.Header.Set("Date", date)
	req.Header.Set("Host", c.Host)
	req.Header.Set("Signature", signature)
	if digest != "" {
		req.Header.Set("Digest", digest)
	}

	httpClient := &http.Client{Timeout: 45 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("cybersource request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("failed to read response: %w", err)
	}

	log.Printf("[Cybersource API] %s https://%s%s -> HTTP %d (Payload length: %d bytes)", method, c.Host, path, resp.StatusCode, len(bodyBytes))
	if resp.StatusCode >= 400 {
		log.Printf("[Cybersource API Error Response] %s", string(respBody))
	}

	return resp, respBody, nil
}

// KMSAsymmetricKeyResponse represents the response from Cybersource KMS keys-asym endpoint.
type KMSAsymmetricKeyResponse struct {
	SubmitTimeUTC  string `json:"submitTimeUtc"`
	Status         string `json:"status"`
	KeyInformation struct {
		Provider       string `json:"provider"`
		Tenant         string `json:"tenant"`
		OrganizationID string `json:"organizationId"`
		KeyID          string `json:"keyId"`
		Pub            string `json:"pub"`
		KeyType        string `json:"keyType"`
		Status         string `json:"status"`
		ExpirationDate string `json:"expirationDate"`
	} `json:"keyInformation"`
}

// RegisterAsymmetricKey registers an X.509 certificate with Cybersource KMS via POST /kms/egress/v2/keys-asym.
func (c *Client) RegisterAsymmetricKey(certBase64SingleLine string) (*KMSAsymmetricKeyResponse, error) {
	path := "/kms/egress/v2/keys-asym"
	payload := map[string]interface{}{
		"clientRequestAction": "STORE",
		"keyInformation": map[string]interface{}{
			"provider":       "nrtd",
			"tenant":         c.MerchantID,
			"keyType":        "publickey",
			"organizationId": c.MerchantID,
			"pub":            certBase64SingleLine,
			"expiryDuration": "365",
		},
	}

	resp, body, err := c.SendRequest("POST", path, payload)
	if err != nil {
		return nil, fmt.Errorf("KMS request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("KMS returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var res KMSAsymmetricKeyResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("failed to parse KMS response: %w", err)
	}

	return &res, nil
}

// GetAsymmetricKey retrieves details for a registered asymmetric key from Cybersource KMS.
func (c *Client) GetAsymmetricKey(keyID string) (*KMSAsymmetricKeyResponse, error) {
	path := fmt.Sprintf("/kms/egress/v2/keys-asym/%s?organizationId=%s", keyID, c.MerchantID)
	resp, body, err := c.SendRequest("GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("KMS request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("KMS returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var res KMSAsymmetricKeyResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("failed to parse KMS response: %w", err)
	}

	return &res, nil
}
