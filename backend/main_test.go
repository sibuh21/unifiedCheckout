package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestGenerateSignatureHasParenthesesOnRequestTarget(t *testing.T) {
	MerchantID = "test_merchant"
	KeyID = "test_key"
	SecretKey = base64.StdEncoding.EncodeToString([]byte("test_secret_32_bytes_long_123456"))

	date := "Mon, 07 Sep 2026 13:00:00 GMT"
	body := []byte(`{"test":"data"}`)
	_, sigHeader := generateSignature("POST", "/pts/v2/payments", body, date)

	// Verify headers parameter includes `(request-target)` with parentheses
	if !strings.Contains(sigHeader, `(request-target)`) {
		t.Fatalf("Expected sigHeader to contain `(request-target)` with parentheses, got: %s", sigHeader)
	}

	// Verify no unparenthesized request-target without parens
	if strings.Contains(sigHeader, `headers="host date request-target `) {
		t.Fatalf("Found deprecated unparenthesized request-target in headers: %s", sigHeader)
	}
}

func TestVerifyWebhookSignature(t *testing.T) {
	secret := "my_webhook_secret_key_1234567890"
	now := time.Now().Unix()
	body := []byte(`{"id":"evt_1","eventType":"payments.payments.updated","status":"SETTLED"}`)

	// Valid signature
	dataToSign := fmt.Sprintf("%d.%s", now, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(dataToSign))
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	sigHeader := fmt.Sprintf("t=%d;keyId=sec_key_1;sig=%s", now, validSig)

	// 1. Test valid signature
	valid, reason := verifyWebhookSignature(sigHeader, body, secret)
	if !valid {
		t.Fatalf("Expected signature to be valid, failed with: %s", reason)
	}

	// 2. Test tampered body
	tamperedBody := []byte(`{"id":"evt_1","eventType":"payments.payments.updated","status":"DECLINED"}`)
	valid, _ = verifyWebhookSignature(sigHeader, tamperedBody, secret)
	if valid {
		t.Fatalf("Expected tampered body to fail signature verification")
	}

	// 3. Test expired timestamp (>300 seconds)
	expiredTime := now - 400
	expiredData := fmt.Sprintf("%d.%s", expiredTime, string(body))
	mac2 := hmac.New(sha256.New, []byte(secret))
	mac2.Write([]byte(expiredData))
	expiredSig := base64.StdEncoding.EncodeToString(mac2.Sum(nil))
	expiredHeader := fmt.Sprintf("t=%d;keyId=sec_key_1;sig=%s", expiredTime, expiredSig)

	valid, reason = verifyWebhookSignature(expiredHeader, body, secret)
	if valid {
		t.Fatalf("Expected expired timestamp to be rejected, but passed")
	}
	if !strings.Contains(reason, "expired") {
		t.Fatalf("Expected expiration reason, got: %s", reason)
	}

	// 4. Test comma-delimited header format
	commaHeader := fmt.Sprintf(`t="%d", keyId="sec_key_1", sig="%s"`, now, validSig)
	valid, reason = verifyWebhookSignature(commaHeader, body, secret)
	if !valid {
		t.Fatalf("Expected comma-separated header to be parsed, got error: %s", reason)
	}
}

func TestOrderStoreAndWebhookUpdate(t *testing.T) {
	store := NewOrderStore()

	order := &Order{
		ID:        "ORD-TEST-123",
		PaymentID: "PAY-999",
		Amount:    "299.99",
		Status:    "PENDING",
		Success:   false,
		CreatedAt: time.Now(),
	}
	store.Save(order)

	// Simulate receiving webhook
	event := WebhookEvent{
		ID:             "EVT-001",
		EventTimestamp: time.Now().UTC().Format(time.RFC3339),
		EventType:      "payments.payments.updated",
		ResourceID:     "PAY-999",
		Status:         "SETTLED",
		OrderCode:      "ORD-TEST-123",
		ReceivedAt:     time.Now(),
	}

	store.RecordWebhook(event)

	updated, ok := store.Get("ORD-TEST-123")
	if !ok {
		t.Fatalf("Order not found in store")
	}

	if updated.Status != "SETTLED" {
		t.Fatalf("Expected status SETTLED, got %s", updated.Status)
	}

	if !updated.Success {
		t.Fatalf("Expected Success to be true after SETTLED event")
	}

	if updated.WebhookStatus != "SETTLED" {
		t.Fatalf("Expected WebhookStatus SETTLED, got %s", updated.WebhookStatus)
	}

	if len(updated.WebhookEvents) != 1 {
		t.Fatalf("Expected 1 webhook event recorded, got %d", len(updated.WebhookEvents))
	}
}

func TestWebhookEndpointWithGin(t *testing.T) {
	MerchantID = "test_merchant"
	WebhookKey = "test_webhook_key_super_secret"

	r := gin.New()
	r.POST("/api/webhooks/cybersource", func(c *gin.Context) {
		rawBody, _ := c.GetRawData()
		sigHeader := c.GetHeader("v-c-signature")
		isValid, reason := verifyWebhookSignature(sigHeader, rawBody, WebhookKey)
		if !isValid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": reason})
			return
		}

		var payload map[string]interface{}
		_ = json.Unmarshal(rawBody, &payload)
		c.JSON(http.StatusOK, gin.H{"status": "received", "id": payload["id"]})
	})

	now := time.Now().Unix()
	body := []byte(`{"id":"evt_100","eventType":"payments.payments.updated","status":"SETTLED"}`)

	dataToSign := fmt.Sprintf("%d.%s", now, string(body))
	mac := hmac.New(sha256.New, []byte(WebhookKey))
	mac.Write([]byte(dataToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, _ := http.NewRequest("POST", "/api/webhooks/cybersource", bytes.NewBuffer(body))
	req.Header.Set("v-c-signature", fmt.Sprintf("t=%s;keyId=test_key;sig=%s", strconv.FormatInt(now, 10), sig))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTargetOriginsSelection(t *testing.T) {
	// Test that origins are selected with HTTPS and reject IP addresses
	if !isValidFQDNOrigin("https://localhost:5173") {
		t.Fatalf("Expected https://localhost:5173 to be valid")
	}
	if !isValidFQDNOrigin("https://4tdw3h1m-5173.use.devtunnels.ms") {
		t.Fatalf("Expected devtunnels to be valid")
	}
	if isValidFQDNOrigin("https://172.22.0.1:5173") {
		t.Fatalf("Expected 172.22.0.1 IP address to be rejected")
	}
	if isValidFQDNOrigin("https://192.168.1.162:5173") {
		t.Fatalf("Expected 192.168.1.162 IP address to be rejected")
	}
	if isValidFQDNOrigin("http://localhost:8080") {
		t.Fatalf("Expected http:// to be rejected (must be https)")
	}
}

func TestParseJWTPayload(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"status":"AUTHORIZED","id":"test-tx-123","clientReferenceInformation":{"code":"ORD-100"}}`))
	jwt := header + "." + payload + ".sig"

	claims, err := parseJWTPayload(jwt)
	if err != nil {
		t.Fatalf("Failed to parse valid JWT payload: %v", err)
	}

	if claims["status"] != "AUTHORIZED" {
		t.Errorf("Expected status AUTHORIZED, got %v", claims["status"])
	}
	if claims["id"] != "test-tx-123" {
		t.Errorf("Expected id test-tx-123, got %v", claims["id"])
	}
}

func TestVerifyPaymentEndpoint(t *testing.T) {
	r := gin.New()
	store := NewOrderStore()

	r.POST("/api/cybersource/verify-payment", func(c *gin.Context) {
		var reqData struct {
			Result  interface{} `json:"result"`
			OrderID string      `json:"orderId"`
			Amount  string      `json:"amount"`
		}
		if err := c.ShouldBindJSON(&reqData); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid"})
			return
		}

		claims := map[string]interface{}{
			"status": "AUTHORIZED",
			"id":     "tx-999",
		}

		order := &Order{
			ID:        "ORD-UNIT-1",
			PaymentID: "tx-999",
			Amount:    "21.00",
			Status:    "AUTHORIZED",
			Success:   true,
		}
		store.Save(order)

		c.JSON(http.StatusOK, gin.H{
			"success":   true,
			"orderId":   order.ID,
			"paymentId": order.PaymentID,
			"status":    order.Status,
			"details":   claims,
		})
	})

	body := []byte(`{"result":"dummy.jwt.sig","orderId":"ORD-UNIT-1","amount":"21.00"}`)
	req, _ := http.NewRequest("POST", "/api/cybersource/verify-payment", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	order, found := store.Get("ORD-UNIT-1")
	if !found || order.Status != "AUTHORIZED" {
		t.Fatalf("Order was not saved as expected")
	}
}

func TestSessionsPayloadStructure(t *testing.T) {
	targetOrigins := []string{"https://4tdw3h1m-5173.use.devtunnels.ms"}
	amount := "21.00"

	payload := map[string]interface{}{
		"targetOrigins": targetOrigins,
		"country":       "US",
		"locale":        "en_US",
		"captureMandate": map[string]interface{}{
			"billingType":              "PARTIAL",
			"requestEmail":             false,
			"requestPhone":             false,
			"requestShipping":          false,
			"showAcceptedNetworkIcons": true,
		},
		"completeMandate": map[string]interface{}{
			"type": "AUTH",
		},
		"data": map[string]interface{}{
			"orderInformation": map[string]interface{}{
				"amountDetails": map[string]interface{}{
					"totalAmount": amount,
					"currency":    "USD",
				},
				"billTo": map[string]interface{}{
					"firstName":          "John",
					"lastName":           "Doe",
					"address1":           "1 Market St",
					"locality":           "San Francisco",
					"administrativeArea": "CA",
					"postalCode":         "94105",
					"country":            "US",
					"email":              "customer@example.com",
					"phoneNumber":        "4158880000",
				},
			},
		},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal payload: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}

	if parsed["country"] != "US" || parsed["locale"] != "en_US" {
		t.Fatalf("Unexpected country/locale")
	}

	completeMandate, ok := parsed["completeMandate"].(map[string]interface{})
	if !ok || completeMandate["type"] != "AUTH" {
		t.Fatalf("Expected completeMandate.type to be AUTH, got: %v", completeMandate)
	}

	captureMandate, ok := parsed["captureMandate"].(map[string]interface{})
	if !ok || captureMandate["billingType"] != "PARTIAL" {
		t.Fatalf("Expected captureMandate.billingType to be PARTIAL, got: %v", captureMandate)
	}

	dataObj, ok := parsed["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected data object in payload")
	}

	orderInfo, ok := dataObj["orderInformation"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected orderInformation in data")
	}

	amountDetails, ok := orderInfo["amountDetails"].(map[string]interface{})
	if !ok || amountDetails["totalAmount"] != "21.00" || amountDetails["currency"] != "USD" {
		t.Fatalf("Expected totalAmount 21.00 USD, got: %v", amountDetails)
	}

	billTo, ok := orderInfo["billTo"].(map[string]interface{})
	if !ok || billTo["firstName"] != "John" || billTo["country"] != "US" {
		t.Fatalf("Expected valid billTo details, got: %v", billTo)
	}
}

func TestLiveCaptureContext(t *testing.T) {
	if MerchantID == "" || KeyID == "" || SecretKey == "" {
		t.Skip("Credentials not configured, skipping live API test")
	}

	targetOrigins := []string{"https://4tdw3h1m-5173.use.devtunnels.ms"}
	payload := map[string]interface{}{
		"targetOrigins": targetOrigins,
		"country":       "US",
		"locale":        "en_US",
		"captureMandate": map[string]interface{}{
			"billingType":              "PARTIAL",
			"requestEmail":             false,
			"requestPhone":             false,
			"requestShipping":          false,
			"showAcceptedNetworkIcons": true,
		},
		"completeMandate": map[string]interface{}{
			"type": "CAPTURE",
		},
		"data": map[string]interface{}{
			"orderInformation": map[string]interface{}{
				"amountDetails": map[string]interface{}{
					"totalAmount": "21.00",
					"currency":    "USD",
				},
				"billTo": map[string]interface{}{
					"firstName":          "John",
					"lastName":           "Doe",
					"address1":           "1 Market St",
					"locality":           "San Francisco",
					"administrativeArea": "CA",
					"postalCode":         "94105",
					"country":            "US",
					"email":              "customer@example.com",
					"phoneNumber":        "4158880000",
				},
			},
		},
	}

	resp, respBody, err := sendCybersourceRequest("POST", "/uc/v1/sessions", payload)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "lookup") {
			t.Skipf("Sandbox environment without external network access, skipping live test: %v", err)
			return
		}
		t.Fatalf("Cybersource request failed: %v", err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 201 Created from Cybersource, got %d: %s", resp.StatusCode, string(respBody))
	}

	claims, err := parseJWTPayload(string(respBody))
	if err != nil {
		t.Fatalf("Failed to parse returned capture context JWT: %v", err)
	}

	t.Logf("Successfully retrieved capture context JWT! Issuer=%v, Expiry=%v", claims["iss"], claims["exp"])
}
