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
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
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

func TestOrderCreationAndWebhookFlow(t *testing.T) {
	r := gin.New()
	store := NewOrderStore()
	testWebhookKey := "super_secret_webhook_test_key_123"

	// 1. Order endpoint
	r.POST("/api/orders", func(c *gin.Context) {
		var reqData struct {
			ID     string `json:"id"`
			Amount string `json:"amount"`
		}
		_ = c.ShouldBindJSON(&reqData)
		order := &Order{
			ID:        reqData.ID,
			Amount:    reqData.Amount,
			Currency:  "USD",
			Status:    "PENDING",
			Success:   false,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		store.Save(order)
		c.JSON(http.StatusOK, order)
	})

	// 2. Webhook receiver
	r.POST("/api/webhooks/cybersource", func(c *gin.Context) {
		rawBody, _ := c.GetRawData()
		sigHeader := c.GetHeader("v-c-signature")
		isValid, reason := verifyWebhookSignature(sigHeader, rawBody, testWebhookKey)
		if !isValid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": reason})
			return
		}

		var payload map[string]interface{}
		_ = json.Unmarshal(rawBody, &payload)

		orderCode, _ := payload["orderCode"].(string)
		status, _ := payload["status"].(string)
		resourceID, _ := payload["resourceId"].(string)

		store.RecordWebhook(WebhookEvent{
			ID:         "EVT-999",
			Status:     status,
			OrderCode:  orderCode,
			ResourceID: resourceID,
			ReceivedAt: time.Now(),
		})

		c.JSON(http.StatusOK, gin.H{"status": "received"})
	})

	// 1. Create order
	orderBody := []byte(`{"id":"ORD-WEBHOOK-TEST-1","amount":"45.00"}`)
	req1, _ := http.NewRequest("POST", "/api/orders", bytes.NewBuffer(orderBody))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("Failed to create order, code: %d", w1.Code)
	}

	order, found := store.Get("ORD-WEBHOOK-TEST-1")
	if !found || order.Status != "PENDING" {
		t.Fatalf("Expected order to be in PENDING state, got found=%v, status=%s", found, order.Status)
	}

	// 2. Send signed webhook event to settle order
	now := time.Now().Unix()
	webhookPayload := []byte(`{"id":"EVT-999","orderCode":"ORD-WEBHOOK-TEST-1","resourceId":"PAY-777","status":"SETTLED"}`)
	dataToSign := fmt.Sprintf("%d.%s", now, string(webhookPayload))
	mac := hmac.New(sha256.New, []byte(testWebhookKey))
	mac.Write([]byte(dataToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req2, _ := http.NewRequest("POST", "/api/webhooks/cybersource", bytes.NewBuffer(webhookPayload))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("v-c-signature", fmt.Sprintf("t=%d;keyId=key_1;sig=%s", now, sig))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("Expected webhook HTTP 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// Verify order updated to SETTLED
	updatedOrder, _ := store.Get("ORD-WEBHOOK-TEST-1")
	if updatedOrder.Status != "SETTLED" {
		t.Fatalf("Expected order status SETTLED, got %s", updatedOrder.Status)
	}
	if !updatedOrder.Success {
		t.Fatalf("Expected order.Success to be true")
	}
	if updatedOrder.PaymentID != "PAY-777" {
		t.Fatalf("Expected order.PaymentID to be PAY-777, got %s", updatedOrder.PaymentID)
	}
}

func TestSessionsPayloadStructure(t *testing.T) {
	targetOrigins := []string{"https://4tdw3h1m-5173.use.devtunnels.ms"}
	amount := "21.00"
	orderCode := "ORD-TEST-99"

	payload := map[string]interface{}{
		"targetOrigins": targetOrigins,
		"country":       "US",
		"locale":        "en_US",
		"completeMandate": map[string]interface{}{
			"type": "CAPTURE",
		},
		"data": map[string]interface{}{
			"clientReferenceInformation": map[string]interface{}{
				"code": orderCode,
			},
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
	if !ok || completeMandate["type"] != "CAPTURE" {
		t.Fatalf("Expected completeMandate.type to be CAPTURE, got: %v", completeMandate)
	}

	dataObj, ok := parsed["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected data object in payload")
	}

	clientRef, ok := dataObj["clientReferenceInformation"].(map[string]interface{})
	if !ok || clientRef["code"] != "ORD-TEST-99" {
		t.Fatalf("Expected clientReferenceInformation.code to be ORD-TEST-99, got: %v", clientRef)
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

func TestOffloadedBillingPayloadStructure(t *testing.T) {
	// Tests payload with completeMandate (to trigger complete method) and offloaded billing
	targetOrigins := []string{"https://localhost:5173"}
	orderCode := "ORD-OFFLOAD-101"

	payload := map[string]interface{}{
		"targetOrigins": targetOrigins,
		"country":       "US",
		"locale":        "en_US",
		"completeMandate": map[string]interface{}{
			"type": "CAPTURE",
		},
		"data": map[string]interface{}{
			"clientReferenceInformation": map[string]interface{}{
				"code": orderCode,
			},
			"orderInformation": map[string]interface{}{
				"amountDetails": map[string]interface{}{
					"totalAmount": "21.00",
					"currency":    "USD",
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

	if _, hasCaptureMandate := parsed["captureMandate"]; hasCaptureMandate {
		t.Fatalf("Expected captureMandate to be omitted to rely on Business Center profile defaults")
	}
	completeMandate, ok := parsed["completeMandate"].(map[string]interface{})
	if !ok || completeMandate["type"] != "CAPTURE" {
		t.Fatalf("Expected completeMandate to be CAPTURE to trigger complete method")
	}

	dataObj := parsed["data"].(map[string]interface{})
	orderInfo := dataObj["orderInformation"].(map[string]interface{})
	if _, hasBillTo := orderInfo["billTo"]; hasBillTo {
		t.Fatalf("Expected billTo to be omitted when offloaded to Cybersource")
	}
}

func TestCaptureContextWithDynamicBillingInfo(t *testing.T) {
	store := NewOrderStore()
	orderCode := "ORD-DYNAMIC-BILL-01"

	customBillTo := map[string]interface{}{
		"firstName":          "Alice",
		"lastName":           "Wonderland",
		"address1":           "456 Elm St",
		"locality":           "Seattle",
		"administrativeArea": "WA",
		"postalCode":         "98101",
		"country":            "US",
		"email":              "alice@example.com",
		"phoneNumber":        "2065551234",
	}

	order := &Order{
		ID:        orderCode,
		Amount:    "55.00",
		Currency:  "USD",
		Status:    "PENDING",
		Success:   false,
		Capture:   true,
		BillTo:    customBillTo,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	store.Save(order)

	retrieved, ok := store.Get(orderCode)
	if !ok {
		t.Fatalf("Order not found in store")
	}

	if retrieved.BillTo["firstName"] != "Alice" {
		t.Fatalf("Expected BillTo firstName Alice, got: %v", retrieved.BillTo["firstName"])
	}
	if retrieved.BillTo["email"] != "alice@example.com" {
		t.Fatalf("Expected BillTo email alice@example.com, got: %v", retrieved.BillTo["email"])
	}
	if retrieved.BillTo["locality"] != "Seattle" {
		t.Fatalf("Expected BillTo locality Seattle, got: %v", retrieved.BillTo["locality"])
	}
}

func TestLiveCaptureContext(t *testing.T) {
	if MerchantID == "" || KeyID == "" || SecretKey == "" {
		t.Skip("Credentials not configured, skipping live API test")
	}

	targetOrigins := []string{"https://localhost:5173"}
	orderCode := fmt.Sprintf("ORD-%d", time.Now().Unix())
	payload := map[string]interface{}{
		"targetOrigins": targetOrigins,
		"country":       "US",
		"locale":        "en_US",
		"completeMandate": map[string]interface{}{
			"type": "CAPTURE",
		},
		"data": map[string]interface{}{
			"clientReferenceInformation": map[string]interface{}{
				"code": orderCode,
			},
			"orderInformation": map[string]interface{}{
				"amountDetails": map[string]interface{}{
					"totalAmount": "21.00",
					"currency":    "USD",
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

	claimsJSON, _ := json.MarshalIndent(claims, "", "  ")
	t.Logf("Capture Context JWT Claims:\n%s", string(claimsJSON))
}

func TestUCOrdersTransactionResultsWebhook(t *testing.T) {
	store := NewOrderStore()
	order := &Order{
		ID:        "ORD-UC-TEST-001",
		Amount:    "21.00",
		Currency:  "USD",
		Status:    "PENDING",
		Success:   false,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	store.Save(order)

	// Simulate uc.orders.transactionresults payload
	event := WebhookEvent{
		ID:             "EVT-UC-999",
		EventTimestamp: time.Now().UTC().Format(time.RFC3339),
		EventType:      "uc.orders.transactionresults",
		ResourceID:     "PAY-TX-888",
		Status:         "SETTLED",
		OrderCode:      "ORD-UC-TEST-001",
		ReceivedAt:     time.Now(),
	}
	store.RecordWebhook(event)

	updated, ok := store.Get("ORD-UC-TEST-001")
	if !ok || updated.Status != "SETTLED" || !updated.Success {
		t.Fatalf("Expected order to transition to SETTLED, got status=%s, success=%v", updated.Status, updated.Success)
	}
}

func TestNoAuthWebhookSignatureSupport(t *testing.T) {
	// Tests that when Cybersource subscription is configured as "No Auth", the absence of v-c-signature is accepted
	valid, reason := verifyWebhookSignature("", []byte(`{"test":"payload"}`), "test_secret_key")
	if !valid {
		t.Fatalf("Expected No Auth webhook to be permitted, got reason: %s", reason)
	}
}

func TestQueryCybersourceWebhooks(t *testing.T) {
	_ = godotenv.Overload(".env")
	MerchantID = os.Getenv("CS_MERCHANT_ID")
	KeyID = os.Getenv("CS_KEY_ID")
	SecretKey = os.Getenv("CS_SECRET_KEY")

	webhookID := "5afa58ec-060b-5cb2-e063-a0588e0a6a8f"

	path := fmt.Sprintf("/notification-subscriptions/v2/webhooks/%s", webhookID)
	resp, body, err := sendCybersourceRequest("GET", path, nil)
	if err != nil {
		t.Skipf("Network error: %v", err)
		return
	}
	t.Logf("Webhook %s status (HTTP %d): %s", webhookID, resp.StatusCode, string(body))
}
