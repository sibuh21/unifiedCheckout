package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

type CartItem struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
	Quantity int     `json:"quantity"`
	Image    string  `json:"image"`
}

// Order represents checkout order and payment state
type Order struct {
	ID            string                 `json:"id"`
	PaymentID     string                 `json:"paymentId,omitempty"`
	Amount        string                 `json:"amount"`
	Currency      string                 `json:"currency"`
	Status        string                 `json:"status"` // e.g. AUTHORIZED, SETTLED, DECLINED, FAILED
	Success       bool                   `json:"success"`
	Capture       bool                   `json:"capture"`
	Error         string                 `json:"error,omitempty"`
	WebhookStatus string                 `json:"webhookStatus,omitempty"`
	WebhookEvents []WebhookEvent         `json:"webhookEvents,omitempty"`
	CreatedAt     time.Time              `json:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt"`
	Details       map[string]interface{} `json:"details,omitempty"`
}

// WebhookEvent represents a received Cybersource notification
type WebhookEvent struct {
	ID             string                 `json:"id"`
	EventTimestamp string                 `json:"eventTimestamp"`
	EventType      string                 `json:"eventType"`
	OrganizationID string                 `json:"organizationId,omitempty"`
	ResourceID     string                 `json:"resourceId,omitempty"`
	Status         string                 `json:"status,omitempty"`
	OrderCode      string                 `json:"orderCode,omitempty"`
	ReceivedAt     time.Time              `json:"receivedAt"`
	RawPayload     map[string]interface{} `json:"rawPayload,omitempty"`
}

// Thread-safe OrderStore
type OrderStore struct {
	mu     sync.RWMutex
	orders map[string]*Order
	events []WebhookEvent
}

func NewOrderStore() *OrderStore {
	return &OrderStore{
		orders: make(map[string]*Order),
		events: make([]WebhookEvent, 0),
	}
}

func (s *OrderStore) Save(order *Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order.UpdatedAt = time.Now()
	s.orders[order.ID] = order
}

func (s *OrderStore) Get(id string) (*Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	order, ok := s.orders[id]
	return order, ok
}

func (s *OrderStore) FindByPaymentID(paymentID string) (*Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.orders {
		if o.PaymentID != "" && o.PaymentID == paymentID {
			return o, true
		}
	}
	return nil, false
}

func (s *OrderStore) RecordWebhook(event WebhookEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Append to recent global events (keep last 50)
	s.events = append([]WebhookEvent{event}, s.events...)
	if len(s.events) > 50 {
		s.events = s.events[:50]
	}

	// Correlate with existing order by OrderCode or ResourceID
	var targetOrder *Order
	if event.OrderCode != "" {
		if o, ok := s.orders[event.OrderCode]; ok {
			targetOrder = o
		}
	}
	if targetOrder == nil && event.ResourceID != "" {
		for _, o := range s.orders {
			if o.PaymentID == event.ResourceID {
				targetOrder = o
				break
			}
		}
	}

	if targetOrder != nil {
		targetOrder.WebhookEvents = append(targetOrder.WebhookEvents, event)
		targetOrder.WebhookStatus = event.Status
		targetOrder.UpdatedAt = time.Now()

		// Update order status based on webhook event
		switch strings.ToUpper(event.Status) {
		case "SETTLED", "SUCCESS", "COMPLETED":
			targetOrder.Status = "SETTLED"
			targetOrder.Success = true
		case "AUTHORIZED", "TRANSMITTED":
			targetOrder.Status = event.Status
			targetOrder.Success = true
		case "DECLINED", "FAILED", "REJECTED", "CANCELLED":
			targetOrder.Status = event.Status
			targetOrder.Success = false
		}
	} else if event.OrderCode != "" {
		// If order didn't exist yet, create a placeholder
		targetOrder = &Order{
			ID:            event.OrderCode,
			PaymentID:     event.ResourceID,
			Status:        event.Status,
			Success:       !strings.EqualFold(event.Status, "FAILED") && !strings.EqualFold(event.Status, "DECLINED"),
			WebhookStatus: event.Status,
			WebhookEvents: []WebhookEvent{event},
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
			Details:       event.RawPayload,
		}
		s.orders[event.OrderCode] = targetOrder
	}
}

func (s *OrderStore) GetRecentEvents() []WebhookEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copied := make([]WebhookEvent, len(s.events))
	copy(copied, s.events)
	return copied
}

var (
	MerchantID   string
	KeyID        string
	SecretKey    string
	WebhookKeyID string
	WebhookKey   string
	Host         = "apitest.cybersource.com" // Sandbox environment
	orderStore   = NewOrderStore()
)

func init() {
	_ = godotenv.Load()

	MerchantID = os.Getenv("CS_MERCHANT_ID")
	KeyID = os.Getenv("CS_KEY_ID")
	SecretKey = os.Getenv("CS_SECRET_KEY")
	WebhookKeyID = os.Getenv("CS_WEBHOOK_KEY_ID")
	WebhookKey = os.Getenv("CS_WEBHOOK_KEY")
}

var mockCart = []CartItem{
	{
		ID:       "prod_1",
		Name:     "Sample Item",
		Price:    21.00,
		Quantity: 1,
		Image:    "https://images.unsplash.com/photo-1505740420928-5e560c06d30e?w=800&q=80",
	},
}

// Helper to decode JWT payload claims
func parseJWTPayload(token string) (map[string]interface{}, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid JWT format")
	}
	segment := parts[1]
	segment = strings.ReplaceAll(segment, "-", "+")
	segment = strings.ReplaceAll(segment, "_", "/")
	switch len(segment) % 4 {
	case 2:
		segment += "=="
	case 3:
		segment += "="
	}
	data, err := base64.StdEncoding.DecodeString(segment)
	if err != nil {
		return nil, fmt.Errorf("failed to base64 decode JWT payload: %w", err)
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(data, &claims); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JWT payload: %w", err)
	}
	return claims, nil
}

// Check if origin is a valid HTTPS FQDN (rejects raw IP addresses)
func isValidFQDNOrigin(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	// Cybersource strictly rejects raw IP addresses (IPv4 or IPv6)
	if net.ParseIP(host) != nil {
		return false
	}
	return true
}

// Generate the HTTP signature for Cybersource REST API requests.
// Complies with Cybersource Cavage specification: pseudo-header must be `(request-target)` with parentheses.
func generateSignature(method, path string, bodyBytes []byte, date string) (string, string) {
	methodLower := strings.ToLower(method)
	var sigString string
	var signatureHeader string
	var digest string

	decodedSecret, _ := base64.StdEncoding.DecodeString(SecretKey)
	mac := hmac.New(sha256.New, decodedSecret)

	if len(bodyBytes) > 0 {
		hasher := sha256.New()
		hasher.Write(bodyBytes)
		digest = "SHA-256=" + base64.StdEncoding.EncodeToString(hasher.Sum(nil))

		// Note the parentheses around `(request-target)`
		sigString = fmt.Sprintf("host: %s\ndate: %s\n(request-target): %s %s\ndigest: %s\nv-c-merchant-id: %s",
			Host, date, methodLower, path, digest, MerchantID)

		mac.Write([]byte(sigString))
		signatureHash := base64.StdEncoding.EncodeToString(mac.Sum(nil))

		signatureHeader = fmt.Sprintf(`keyid="%s", algorithm="HmacSHA256", headers="host date (request-target) digest v-c-merchant-id", signature="%s"`,
			KeyID, signatureHash)
	} else {
		sigString = fmt.Sprintf("host: %s\ndate: %s\n(request-target): %s %s\nv-c-merchant-id: %s",
			Host, date, methodLower, path, MerchantID)

		mac.Write([]byte(sigString))
		signatureHash := base64.StdEncoding.EncodeToString(mac.Sum(nil))

		signatureHeader = fmt.Sprintf(`keyid="%s", algorithm="HmacSHA256", headers="host date (request-target) v-c-merchant-id", signature="%s"`,
			KeyID, signatureHash)
	}

	return digest, signatureHeader
}

// Helper to execute signed HTTP requests against Cybersource
func sendCybersourceRequest(method, path string, payload interface{}) (*http.Response, []byte, error) {
	url := "https://" + Host + path

	var bodyBytes []byte
	if payload != nil {
		var err error
		bodyBytes, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal payload: %w", err)
		}
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	digest, signature := generateSignature(method, path, bodyBytes, date)

	var reqBody io.Reader
	if len(bodyBytes) > 0 {
		reqBody = bytes.NewBuffer(bodyBytes)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("v-c-merchant-id", MerchantID)
	req.Header.Set("Date", date)
	req.Header.Set("Host", Host)
	req.Header.Set("Signature", signature)
	if digest != "" {
		req.Header.Set("Digest", digest)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("cybersource request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("failed to read response: %w", err)
	}

	log.Printf("[Cybersource API] %s https://%s%s -> HTTP %d (Payload length: %d bytes)", method, Host, path, resp.StatusCode, len(bodyBytes))
	if resp.StatusCode >= 400 {
		log.Printf("[Cybersource API Error Response] %s", string(respBody))
	}

	return resp, respBody, nil
}

// Helper to extract detailed error message from Cybersource response payload
func extractCybersourceError(result map[string]interface{}, statusCode int, defaultMsg string) string {
	var parts []string

	if errInfo, ok := result["errorInformation"].(map[string]interface{}); ok {
		if msg, ok := errInfo["message"].(string); ok && msg != "" {
			parts = append(parts, msg)
		}
		if rsn, ok := errInfo["reason"].(string); ok && rsn != "" {
			parts = append(parts, fmt.Sprintf("(Reason: %s)", rsn))
		}
		if details, ok := errInfo["details"].([]interface{}); ok {
			for _, d := range details {
				if dm, ok := d.(map[string]interface{}); ok {
					if dMsg, ok := dm["message"].(string); ok && dMsg != "" {
						parts = append(parts, dMsg)
					}
				}
			}
		}
	}

	if msg, ok := result["message"].(string); ok && msg != "" {
		parts = append(parts, msg)
	}
	if rsn, ok := result["reason"].(string); ok && rsn != "" {
		parts = append(parts, fmt.Sprintf("(Reason: %s)", rsn))
	}

	if errs, ok := result["errors"].([]interface{}); ok {
		for _, e := range errs {
			if em, ok := e.(map[string]interface{}); ok {
				if msg, ok := em["message"].(string); ok && msg != "" {
					parts = append(parts, msg)
				}
			}
		}
	}

	if procInfo, ok := result["processorInformation"].(map[string]interface{}); ok {
		if rCode, ok := procInfo["responseCode"].(string); ok && rCode != "" {
			parts = append(parts, fmt.Sprintf("[Processor Code: %s]", rCode))
		}
	}

	// Specific diagnostic hint for Reason Code 150 / HTTP 502
	isReason150 := statusCode == 502
	for _, p := range parts {
		if strings.Contains(p, "150") || strings.Contains(strings.ToLower(p), "gateway name cannot be determined") {
			isReason150 = true
			break
		}
	}
	if isReason150 {
		parts = append(parts, "Cybersource Reason Code 150 / HTTP 502: The payment processor gateway name cannot be determined from the merchant configuration. Please check your Processor Connections in the Cybersource Business Center (ebc2test.cybersource.com).")
	}

	if len(parts) == 0 {
		return defaultMsg
	}
	return strings.Join(parts, " | ")
}

// Verify Cybersource Webhook Signature (v-c-signature header)
// Format: t=<timestamp>;keyId=<keyId>;sig=<signature> (or comma-separated)
func verifyWebhookSignature(rawSigHeader string, rawBody []byte, sharedSecret string) (bool, string) {
	if sharedSecret == "" {
		// Secret key not configured: warn but allow in development
		return true, "WARNING: CS_WEBHOOK_KEY not set, signature check skipped in development"
	}

	if rawSigHeader == "" {
		return false, "Missing v-c-signature header"
	}

	// Parse header values
	params := make(map[string]string)
	// Handle semicolon or comma delimiters
	var tokens []string
	if strings.Contains(rawSigHeader, ";") {
		tokens = strings.Split(rawSigHeader, ";")
	} else {
		tokens = strings.Split(rawSigHeader, ",")
	}

	for _, token := range tokens {
		parts := strings.SplitN(strings.TrimSpace(token), "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
			params[k] = v
		}
	}

	tStr, hasT := params["t"]
	sig, hasSig := params["sig"]

	if !hasT || !hasSig {
		return false, "Invalid v-c-signature format: missing 't' or 'sig'"
	}

	// Verify timestamp tolerance window (within 5 minutes = 300 seconds)
	var timestampSec int64
	if tsInt, err := strconv.ParseInt(tStr, 10, 64); err == nil {
		// Could be seconds or milliseconds
		if tsInt > 1000000000000 {
			timestampSec = tsInt / 1000
		} else {
			timestampSec = tsInt
		}
	} else if parsedTime, err := time.Parse(time.RFC3339, tStr); err == nil {
		timestampSec = parsedTime.Unix()
	} else {
		return false, "Invalid timestamp format in v-c-signature"
	}

	nowSec := time.Now().Unix()
	if math.Abs(float64(nowSec-timestampSec)) > 300 {
		return false, fmt.Sprintf("Webhook signature timestamp expired (diff %ds > 300s)", int64(math.Abs(float64(nowSec-timestampSec))))
	}

	// Construct payload string to sign: "<timestamp>.<rawBody>"
	dataToSign := fmt.Sprintf("%s.%s", tStr, string(rawBody))

	// Decode secret key (can be base64-encoded or raw)
	var secretBytes []byte
	decoded, err := base64.StdEncoding.DecodeString(sharedSecret)
	if err == nil && len(decoded) > 0 {
		secretBytes = decoded
	} else {
		secretBytes = []byte(sharedSecret)
	}

	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(dataToSign))
	expectedSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expectedSig), []byte(sig)) {
		return false, "Signature mismatch"
	}

	return true, "Verified"
}

// Persist newly generated webhook key to .env file
func updateEnvWebhookKey(keyID, secretKey string) {
	envPath := ".env"
	content, err := os.ReadFile(envPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(content), "\n")
	hasKeyID := false
	hasKey := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "CS_WEBHOOK_KEY_ID=") {
			lines[i] = fmt.Sprintf("CS_WEBHOOK_KEY_ID=%s", keyID)
			hasKeyID = true
		} else if strings.HasPrefix(trimmed, "CS_WEBHOOK_KEY=") {
			lines[i] = fmt.Sprintf("CS_WEBHOOK_KEY=%s", secretKey)
			hasKey = true
		}
	}
	if !hasKeyID {
		lines = append(lines, fmt.Sprintf("CS_WEBHOOK_KEY_ID=%s", keyID))
	}
	if !hasKey {
		lines = append(lines, fmt.Sprintf("CS_WEBHOOK_KEY=%s", secretKey))
	}
	_ = os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0644)
	log.Printf("[KMS] Updated .env file with new CS_WEBHOOK_KEY_ID=%s", keyID)
}

func main() {
	// Reload .env in main() to ensure latest values are always read
	_ = godotenv.Overload()
	MerchantID = os.Getenv("CS_MERCHANT_ID")
	KeyID = os.Getenv("CS_KEY_ID")
	SecretKey = os.Getenv("CS_SECRET_KEY")
	WebhookKeyID = os.Getenv("CS_WEBHOOK_KEY_ID")
	WebhookKey = os.Getenv("CS_WEBHOOK_KEY")

	log.Printf("==================================================")
	log.Printf("Starting Cybersource Unified Checkout Backend")
	log.Printf("Active Merchant ID: %s", MerchantID)
	log.Printf("Active Key ID:      %s", KeyID)
	log.Printf("Webhook Key ID:     %s", WebhookKeyID)
	log.Printf("==================================================")

	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOriginFunc: func(origin string) bool {
			// In development, permit any localhost, loopback, IPv6, or LAN origin on any port
			if origin == "" || origin == "null" {
				return true
			}
			u, err := url.Parse(origin)
			if err != nil {
				return true
			}
			h := u.Hostname()
			if h == "localhost" || h == "127.0.0.1" || h == "::1" ||
				strings.HasPrefix(h, "192.168.") || strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "172.") {
				return true
			}
			return true
		},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "v-c-merchant-id", "v-c-signature", "v-c-correlation-id"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Health check endpoint (can also be registered as healthCheckUrl with Cybersource)
	r.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":     "healthy",
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
			"merchant":   MerchantID != "",
			"merchantId": MerchantID,
		})
	})

	r.GET("/api/cart", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"items": mockCart})
	})

	// 1. Generate Capture Context for Unified Checkout v1 Sessions API
	r.POST("/api/cybersource/capture-context", func(c *gin.Context) {
		var reqData struct {
			Amount       string `json:"amount"`
			TargetOrigin string `json:"targetOrigin"`
			CaptureType  string `json:"captureType"` // "AUTH" (default) or "CAPTURE"
		}
		if err := c.ShouldBindJSON(&reqData); err != nil {
			reqData.Amount = "21.00"
		}

		if MerchantID == "" || KeyID == "" || SecretKey == "" {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "Cybersource credentials not configured. Please set CS_MERCHANT_ID, CS_KEY_ID, and CS_SECRET_KEY in backend/.env.",
			})
			return
		}

		// Cybersource validates that every origin in targetOrigins matches the current page origin.
		// If extra origins are present in the array, the client SDK throws UNUSED_TARGET_ORIGINS.
		// Therefore, we use strictly the single matching origin for the current page session.
		targetOrigin := "https://localhost:5173"
		if reqData.TargetOrigin != "" && isValidFQDNOrigin(reqData.TargetOrigin) {
			targetOrigin = reqData.TargetOrigin
		} else if originHeader := c.GetHeader("Origin"); originHeader != "" && isValidFQDNOrigin(originHeader) {
			targetOrigin = originHeader
		}

		targetOrigins := []string{targetOrigin}

		amount := "21.00"
		if reqData.Amount != "" && reqData.Amount != "0.00" {
			amount = reqData.Amount
		}

		// Default to AUTH (Authorization only) to avoid Reason Code 150 (missing settlement gateway),
		// but allow CAPTURE if explicitly requested.
		captureType := "AUTH"
		if strings.EqualFold(reqData.CaptureType, "CAPTURE") {
			captureType = "CAPTURE"
		}

		path := "/uc/v1/sessions"
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
				"type": captureType,
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

		resp, respBody, err := sendCybersourceRequest("POST", path, payload)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to contact Cybersource", "details": err.Error()})
			return
		}

		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			c.JSON(resp.StatusCode, gin.H{
				"error":   "Cybersource session creation failed",
				"details": string(respBody),
			})
			return
		}

		// The capture context is returned as a signed JWT
		c.JSON(http.StatusOK, gin.H{
			"jwt":           string(respBody),
			"targetOrigins": targetOrigins,
		})
	})

	// 1b. Verify Payment Result JWT from sendToServer(result)
	r.POST("/api/cybersource/verify-payment", func(c *gin.Context) {
		var reqData struct {
			Result  interface{} `json:"result"`
			OrderID string      `json:"orderId"`
			Amount  string      `json:"amount"`
		}

		if err := c.ShouldBindJSON(&reqData); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload: result is required"})
			return
		}

		var claims map[string]interface{}
		var rawToken string

		switch v := reqData.Result.(type) {
		case string:
			rawToken = v
			parsed, err := parseJWTPayload(v)
			if err == nil {
				claims = parsed
			} else {
				claims = map[string]interface{}{"raw": v}
			}
		case map[string]interface{}:
			claims = v
			if t, ok := v["token"].(string); ok {
				rawToken = t
			} else if t, ok := v["jwt"].(string); ok {
				rawToken = t
			}
		default:
			claims = map[string]interface{}{"result": v}
		}

		orderCode := reqData.OrderID
		if orderCode == "" {
			// Check claims for clientReferenceInformation
			if clientRef, ok := claims["clientReferenceInformation"].(map[string]interface{}); ok {
				if code, ok := clientRef["code"].(string); ok && code != "" {
					orderCode = code
				}
			}
			if orderCode == "" {
				orderCode = "ORD-" + strconv.FormatInt(time.Now().Unix(), 10)
			}
		}

		paymentID := ""
		if id, ok := claims["id"].(string); ok {
			paymentID = id
		} else if id, ok := claims["transactionId"].(string); ok {
			paymentID = id
		} else if completeMandate, ok := claims["completeMandate"].(map[string]interface{}); ok {
			if txId, ok := completeMandate["transactionId"].(string); ok {
				paymentID = txId
			}
		}

		status := "AUTHORIZED"
		if s, ok := claims["status"].(string); ok && s != "" {
			status = strings.ToUpper(s)
		}

		amount := reqData.Amount
		if amount == "" {
			if orderInfo, ok := claims["orderInformation"].(map[string]interface{}); ok {
				if amountDetails, ok := orderInfo["amountDetails"].(map[string]interface{}); ok {
					if tot, ok := amountDetails["totalAmount"].(string); ok {
						amount = tot
					}
				}
			}
			if amount == "" {
				amount = "21.00"
			}
		}

		order := &Order{
			ID:        orderCode,
			PaymentID: paymentID,
			Amount:    amount,
			Currency:  "USD",
			Status:    status,
			Success:   true,
			Capture:   true,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Details:   claims,
		}
		orderStore.Save(order)

		log.Printf("[VerifyPayment] Order %s verified: PaymentID=%s, Status=%s", orderCode, paymentID, status)

		c.JSON(http.StatusOK, gin.H{
			"success":   true,
			"orderId":   orderCode,
			"paymentId": paymentID,
			"status":    status,
			"message":   "Payment result verified successfully",
			"details":   claims,
			"rawToken":  rawToken,
		})
	})

	// 2. Process Payment using Transient Token
	// Supports:
	// - Authorization only (default when capture is false or omitted)
	// - Sale (Authorization + Capture when capture is true)
	r.POST("/api/cybersource/process-payment", func(c *gin.Context) {
		var reqData struct {
			TransientToken string `json:"transientToken"`
			Amount         string `json:"amount"`
			OrderID        string `json:"orderId"`
			Capture        *bool  `json:"capture"` // Default false for authorization only
		}

		if err := c.ShouldBindJSON(&reqData); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload: transientToken and amount required"})
			return
		}

		if MerchantID == "" || KeyID == "" || SecretKey == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Cybersource credentials not configured in backend/.env"})
			return
		}

		// Default order reference ID if not provided
		orderCode := reqData.OrderID
		if orderCode == "" {
			orderCode = "ORD-" + time.Now().Format("20060102150405")
		}

		// Default capture to false (Authorization only) to avoid Reason Code 150 (missing settlement gateway),
		// unless explicitly set to true (Sale = Auth + Capture).
		capture := false
		if reqData.Capture != nil && *reqData.Capture {
			capture = true
		}

		path := "/pts/v2/payments"
		payload := map[string]interface{}{
			"clientReferenceInformation": map[string]string{
				"code": orderCode,
			},
			"orderInformation": map[string]interface{}{
				"amountDetails": map[string]string{
					"totalAmount": reqData.Amount,
					"currency":    "USD",
				},
				"billTo": map[string]string{
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
			"tokenInformation": map[string]string{
				"transientTokenJwt": reqData.TransientToken,
			},
		}

		// For Authorization-only, omit processingInformation completely (Cybersource default).
		// Only include processingInformation when Sale (capture = true) is explicitly requested.
		if capture {
			payload["processingInformation"] = map[string]interface{}{
				"capture": true,
			}
		}

		log.Printf("[ProcessPayment] Submitting %s request to Cybersource: Order=%s, Amount=%s USD, Capture=%v",
			path, orderCode, reqData.Amount, capture)

		resp, respBody, err := sendCybersourceRequest("POST", path, payload)
		if err != nil {
			log.Printf("[ProcessPayment] Gateway error communicating with Cybersource: %v", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "Cybersource gateway communication error", "details": err.Error()})
			return
		}

		var result map[string]interface{}
		_ = json.Unmarshal(respBody, &result)

		status, _ := result["status"].(string)
		paymentID, _ := result["id"].(string)

		// Note: Cybersource returns 201 Created even when payment is DECLINED/REJECTED!
		// We must inspect the actual transaction status to evaluate success.
		isSuccess := (resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK) &&
			(status == "AUTHORIZED" || status == "SETTLED" || status == "PENDING" || status == "AUTHORIZED_PENDING_REVIEW")

		var errorMsg string
		if !isSuccess {
			errorMsg = extractCybersourceError(result, resp.StatusCode, "Payment declined or rejected by processor")
			log.Printf("[ProcessPayment] Transaction failed: Order=%s, Status=%s, Cybersource HTTP=%d, Reason=%s",
				orderCode, status, resp.StatusCode, errorMsg)
		} else {
			log.Printf("[ProcessPayment] Transaction succeeded: Order=%s, PaymentID=%s, Status=%s",
				orderCode, paymentID, status)
		}

		// Record order in local store
		order := &Order{
			ID:        orderCode,
			PaymentID: paymentID,
			Amount:    reqData.Amount,
			Currency:  "USD",
			Status:    status,
			Success:   isSuccess,
			Capture:   capture,
			Error:     errorMsg,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Details:   result,
		}
		orderStore.Save(order)

		if isSuccess {
			c.JSON(http.StatusOK, gin.H{
				"success":   true,
				"status":    status,
				"orderId":   orderCode,
				"paymentId": paymentID,
				"message":   fmt.Sprintf("Payment %s successfully", strings.ToLower(status)),
				"details":   result,
			})
		} else {
			// Return 400 or original status code when payment fails
			httpCode := resp.StatusCode
			if httpCode == http.StatusCreated {
				httpCode = http.StatusBadRequest
			}
			c.JSON(httpCode, gin.H{
				"success":               false,
				"status":                status,
				"orderId":               orderCode,
				"paymentId":             paymentID,
				"error":                 errorMsg,
				"details":               result,
				"cybersourceStatusCode": resp.StatusCode,
			})
		}
	})

	// 3. Webhook Receiver: Handles Cybersource Notification Service callbacks
	r.POST("/api/webhooks/cybersource", func(c *gin.Context) {
		rawBody, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read webhook payload"})
			return
		}

		// Cybersource provides signature in `v-c-signature`
		sigHeader := c.GetHeader("v-c-signature")
		if sigHeader == "" {
			// Also check standard Signature header fallback
			sigHeader = c.GetHeader("Signature")
		}

		isValid, reason := verifyWebhookSignature(sigHeader, rawBody, WebhookKey)
		if !isValid {
			log.Printf("[Webhook] Security check failed: %s", reason)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Webhook signature verification failed", "reason": reason})
			return
		}

		var payload map[string]interface{}
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			log.Printf("[Webhook] Failed to unmarshal body: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON format"})
			return
		}

		// Extract notification fields
		eventID, _ := payload["id"].(string)
		eventTimestamp, _ := payload["eventTimestamp"].(string)
		eventType, _ := payload["eventType"].(string)
		organizationID, _ := payload["organizationId"].(string)
		status, _ := payload["status"].(string)
		resourceID, _ := payload["resourceId"].(string)

		// Look for clientReferenceInformation (may be top-level or inside payload)
		var orderCode string
		if clientRef, ok := payload["clientReferenceInformation"].(map[string]interface{}); ok {
			orderCode, _ = clientRef["code"].(string)
		} else if innerPayload, ok := payload["payload"].(map[string]interface{}); ok {
			if clientRef, ok := innerPayload["clientReferenceInformation"].(map[string]interface{}); ok {
				orderCode, _ = clientRef["code"].(string)
			}
			if status == "" {
				status, _ = innerPayload["status"].(string)
			}
			if resourceID == "" {
				resourceID, _ = innerPayload["id"].(string)
			}
		}

		event := WebhookEvent{
			ID:             eventID,
			EventTimestamp: eventTimestamp,
			EventType:      eventType,
			OrganizationID: organizationID,
			ResourceID:     resourceID,
			Status:         status,
			OrderCode:      orderCode,
			ReceivedAt:     time.Now(),
			RawPayload:     payload,
		}

		log.Printf("==================================================")
		log.Printf("🔔 [CYBERSOURCE WEBHOOK NOTIFICATION RECEIVED]")
		log.Printf("Type:        %s", eventType)
		log.Printf("Status:      %s", status)
		log.Printf("Order Code:  %s", orderCode)
		log.Printf("Resource ID: %s", resourceID)
		log.Printf("Signature:   Verified=%v (%s)", isValid, reason)
		log.Printf("Payload:     %s", string(rawBody))
		log.Printf("==================================================")

		orderStore.RecordWebhook(event)

		// Acknowledge receipt to Cybersource with 200 OK
		c.JSON(http.StatusOK, gin.H{
			"status":   "received",
			"eventId":  eventID,
			"verified": isValid,
		})
	})

	// 4. Query Order & Webhook Status
	r.GET("/api/orders/:orderId", func(c *gin.Context) {
		orderID := c.Param("orderId")
		order, found := orderStore.Get(orderID)
		if !found {
			// Try by payment ID
			order, found = orderStore.FindByPaymentID(orderID)
		}

		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
			return
		}

		c.JSON(http.StatusOK, order)
	})

	// 5. Query Recent Webhook Events (for frontend diagnostics/monitoring)
	r.GET("/api/cybersource/webhooks/events", func(c *gin.Context) {
		events := orderStore.GetRecentEvents()
		c.JSON(http.StatusOK, gin.H{
			"count":  len(events),
			"events": events,
		})
	})

	// 6. Programmatically Subscribe to Cybersource Webhooks
	r.POST("/api/cybersource/webhooks/subscribe", func(c *gin.Context) {
		var reqData struct {
			WebhookURL string   `json:"webhookUrl"`
			ProductID  string   `json:"productId"`
			EventTypes []string `json:"eventTypes"`
			Name       string   `json:"name"`
		}

		if err := c.ShouldBindJSON(&reqData); err != nil || reqData.WebhookURL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "webhookUrl is required"})
			return
		}

		if reqData.ProductID == "" {
			reqData.ProductID = "payments"
		}
		if len(reqData.EventTypes) == 0 {
			if reqData.ProductID == "unifiedCheckout" {
				reqData.EventTypes = []string{"uc.orders.transactionresults"}
			} else {
				reqData.EventTypes = []string{"payments.payments.updated", "payments.payments.created"}
			}
		}
		if reqData.Name == "" {
			reqData.Name = "Unified Checkout Subscription"
		}

		path := "/notification-subscriptions/v2/webhooks"
		payload := map[string]interface{}{
			"name":                reqData.Name,
			"description":         "Subscribed from Unified Checkout app",
			"organizationId":      MerchantID,
			"productId":           reqData.ProductID,
			"eventTypes":          reqData.EventTypes,
			"webhookUrl":          reqData.WebhookURL,
			"notificationVersion": "1.0",
			"retryPolicy": map[string]interface{}{
				"algorithm":          "EXPONENTIAL",
				"firstRetryInterval": 60,
				"numberOfRetries":    3,
			},
			"securityPolicy": map[string]interface{}{
				"securityType": "KEY",
				"proxyType":    "EXTERNAL",
			},
		}

		resp, respBody, err := sendCybersourceRequest("POST", path, payload)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Subscription request failed", "details": err.Error()})
			return
		}

		var result map[string]interface{}
		_ = json.Unmarshal(respBody, &result)

		c.JSON(resp.StatusCode, gin.H{
			"statusCode": resp.StatusCode,
			"details":    result,
		})
	})

	// 7. Retrieve Active Cybersource Webhook Subscriptions
	r.GET("/api/cybersource/webhooks/subscriptions", func(c *gin.Context) {
		path := "/notification-subscriptions/v2/webhooks"
		resp, respBody, err := sendCybersourceRequest("GET", path, nil)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch subscriptions", "details": err.Error()})
			return
		}

		var result map[string]interface{}
		_ = json.Unmarshal(respBody, &result)

		c.JSON(resp.StatusCode, result)
	})

	// 7b. Retrieve Enabled Webhook Products and Event Types
	r.GET("/api/cybersource/webhooks/products", func(c *gin.Context) {
		path := fmt.Sprintf("/notification-subscriptions/v2/products/%s", MerchantID)
		resp, respBody, err := sendCybersourceRequest("GET", path, nil)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch products", "details": err.Error()})
			return
		}

		var result map[string]interface{}
		_ = json.Unmarshal(respBody, &result)

		c.JSON(resp.StatusCode, result)
	})

	// 8. Simulate a Webhook (for local dev and testing)
	r.POST("/api/webhooks/simulate", func(c *gin.Context) {
		var simData struct {
			OrderCode string `json:"orderCode"`
			Status    string `json:"status"` // SETTLED, DECLINED, etc.
			EventType string `json:"eventType"`
		}
		if err := c.ShouldBindJSON(&simData); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid simulation payload"})
			return
		}

		if simData.Status == "" {
			simData.Status = "SETTLED"
		}
		if simData.EventType == "" {
			simData.EventType = "uc.orders.transactionresults"
		}

		mockEvent := WebhookEvent{
			ID:             "SIM-EVT-" + strconv.FormatInt(time.Now().Unix(), 10),
			EventTimestamp: time.Now().UTC().Format(time.RFC3339),
			EventType:      simData.EventType,
			OrganizationID: MerchantID,
			ResourceID:     "SIM-RES-" + strconv.FormatInt(time.Now().Unix(), 10),
			Status:         simData.Status,
			OrderCode:      simData.OrderCode,
			ReceivedAt:     time.Now(),
			RawPayload: map[string]interface{}{
				"simulation": true,
				"orderCode":  simData.OrderCode,
				"status":     simData.Status,
			},
		}

		orderStore.RecordWebhook(mockEvent)
		c.JSON(http.StatusOK, gin.H{
			"message": "Simulated webhook event dispatched successfully",
			"event":   mockEvent,
		})
	})

	// 9. Generate Webhook Symmetric Key via Cybersource KMS API
	// Cybersource Webhook Signature keys are generated via Key Management Service (KMS),
	// not through the standard Key Management menu in the Business Center!
	r.POST("/api/cybersource/webhooks/generate-key", func(c *gin.Context) {
		if MerchantID == "" || KeyID == "" || SecretKey == "" {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "REST API credentials (CS_MERCHANT_ID, CS_KEY_ID, CS_SECRET_KEY) are required to generate a webhook key.",
			})
			return
		}

		path := "/kms/egress/v2/keys-sym"
		payload := map[string]interface{}{
			"clientRequestAction": "CREATE",
			"keyInformation": map[string]interface{}{
				"provider":       "nrtd",
				"tenant":         MerchantID,
				"keyType":        "sharedSecret",
				"organizationId": MerchantID,
			},
		}

		resp, respBody, err := sendCybersourceRequest("POST", path, payload)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to call KMS API", "details": err.Error()})
			return
		}

		var result map[string]interface{}
		_ = json.Unmarshal(respBody, &result)

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			var genKeyID, genKey string
			if keyInfo, ok := result["keyInformation"].(map[string]interface{}); ok {
				genKeyID, _ = keyInfo["keyId"].(string)
				genKey, _ = keyInfo["key"].(string)

				if genKey != "" {
					WebhookKeyID = genKeyID
					WebhookKey = genKey
					log.Printf("[KMS] Webhook Key successfully created: KeyId=%s", genKeyID)
					updateEnvWebhookKey(genKeyID, genKey)
				}
			}

			c.JSON(http.StatusOK, gin.H{
				"success":          true,
				"message":          "Webhook Symmetric Key created successfully via Cybersource KMS!",
				"webhookKeyId":     genKeyID,
				"webhookSecretKey": genKey,
				"details":          result,
			})
		} else {
			c.JSON(resp.StatusCode, gin.H{
				"success": false,
				"error":   "Cybersource KMS key creation failed",
				"details": result,
			})
		}
	})

	log.Println("Unified Checkout Server starting on port 8080...")
	if err := r.Run(":8080"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
