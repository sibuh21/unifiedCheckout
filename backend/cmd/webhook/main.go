package main

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"unified-checkout-backend/internal/cybersource"

	"github.com/joho/godotenv"
)

var (
	client     *cybersource.Client
	merchantID string
	host       = "apitest.cybersource.com"
)

func init() {
	// Try loading .env from multiple locations
	for _, path := range []string{".env", "backend/.env", "../../backend/.env"} {
		_ = godotenv.Load(path)
	}

	merchantID = os.Getenv("CS_MERCHANT_ID")
	keyID := os.Getenv("CS_KEY_ID")
	secretKey := os.Getenv("CS_SECRET_KEY")

	if merchantID == "" || keyID == "" || secretKey == "" {
		log.Fatal("Missing required environment variables: CS_MERCHANT_ID, CS_KEY_ID, CS_SECRET_KEY")
	}

	client = cybersource.NewClient(merchantID, keyID, secretKey, host)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "products":
		cmdProducts()
	case "create":
		cmdCreate(os.Args[2:])
	case "list":
		cmdList()
	case "get":
		cmdGet(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "delete":
		cmdDelete(os.Args[2:])
	case "generate-key":
		cmdGenerateKey()
	case "generate-mle-key":
		cmdGenerateMLEKey()
	case "get-mle-cert":
		cmdGetMLECert(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: go run cmd/webhook/main.go <command> [options]

Commands:
  products                                    List available products & event types
  create    -url <url> [-health-url <url>] [-name <name>]
                                              Create a new webhook subscription
  list                                        List all webhook subscriptions
  get       -id <webhookId>                   Get subscription details
  update    -id <webhookId> [-url <url>] [-health-url <url>] [-status <ACTIVE|INACTIVE>]
                                              Update a subscription
  delete    -id <webhookId>                   Delete a subscription
  generate-key                                Generate a new digital signature key via KMS
  generate-mle-key                            Generate & register MLE certificate with KMS
  get-mle-cert -id <keyId>                    Get MLE certificate details from KMS`)
}

func printJSON(data interface{}) {
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Fatalf("Failed to format JSON: %v", err)
	}
	fmt.Println(string(out))
}

func handleResponse(resp *http.Response, body []byte, err error) {
	if err != nil {
		log.Fatalf("Request failed: %v", err)
	}

	var result interface{}
	if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
		// Not JSON, print raw
		fmt.Printf("HTTP %d\n%s\n", resp.StatusCode, string(body))
		return
	}

	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "Error (HTTP %d):\n", resp.StatusCode)
		printJSON(result)
		os.Exit(1)
	}

	fmt.Printf("Success (HTTP %d):\n", resp.StatusCode)
	printJSON(result)
}

// ── Commands ─────────────────────────────────────────────────────────

func cmdProducts() {
	path := fmt.Sprintf("/notification-subscriptions/v2/products/%s", merchantID)
	resp, body, err := client.SendRequest("GET", path, nil)
	handleResponse(resp, body, err)
}

func cmdCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	webhookURL := fs.String("url", "", "Webhook delivery URL (required)")
	healthURL := fs.String("health-url", "", "Health check URL (optional)")
	name := fs.String("name", "UC Webhook", "Subscription name")
	fs.Parse(args)

	if *webhookURL == "" {
		log.Fatal("Missing required flag: -url")
	}

	// Derive health URL from webhook URL if not provided
	if *healthURL == "" {
		// Extract base from webhook URL (clean base URL without query parameters)
		parts := strings.SplitN(*webhookURL, "/api/", 2)
		if len(parts) > 0 {
			*healthURL = parts[0] + "/"
		}
	}

	payload := map[string]interface{}{
		"name":           *name,
		"description":    "Managed by unified-checkout CLI",
		"organizationId": merchantID,
		"webhookUrl":     *webhookURL,
		"healthCheckUrl": *healthURL,
		"securityPolicy": map[string]interface{}{
			"securityType": "key",
			"proxyType":    "external",
		},
		"products": []map[string]interface{}{
			{
				"productId":  "unifiedCheckout",
				"eventTypes": []string{"uc.orders.transactionresults"},
			},
		},
		"notificationScope": "SELF",
		"retryPolicy": map[string]interface{}{
			"algorithm":           "ARITHMETIC",
			"firstRetry":          1,
			"interval":            1,
			"numberOfRetries":     3,
			"deactivateFlag":      "true",
			"repeatSequenceCount": 0,
		},
	}

	path := "/notification-subscriptions/v2/webhooks"
	resp, body, err := client.SendRequest("POST", path, payload)
	handleResponse(resp, body, err)
}

func cmdList() {
	path := fmt.Sprintf("/notification-subscriptions/v2/webhooks?organizationId=%s", merchantID)
	resp, body, err := client.SendRequest("GET", path, nil)
	handleResponse(resp, body, err)
}

func cmdGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	id := fs.String("id", "", "Webhook subscription ID (required)")
	fs.Parse(args)

	if *id == "" {
		log.Fatal("Missing required flag: -id")
	}

	path := fmt.Sprintf("/notification-subscriptions/v2/webhooks/%s", *id)
	resp, body, err := client.SendRequest("GET", path, nil)
	handleResponse(resp, body, err)
}

func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	id := fs.String("id", "", "Webhook subscription ID (required)")
	webhookURL := fs.String("url", "", "New webhook URL")
	healthURL := fs.String("health-url", "", "New health check URL")
	status := fs.String("status", "", "New status (ACTIVE or INACTIVE)")
	fs.Parse(args)

	if *id == "" {
		log.Fatal("Missing required flag: -id")
	}

	payload := map[string]interface{}{
		"organizationId": merchantID,
	}
	if *webhookURL != "" {
		payload["webhookUrl"] = *webhookURL
	}
	if *healthURL != "" {
		payload["healthCheckUrl"] = *healthURL
	}
	if *status != "" {
		payload["status"] = strings.ToUpper(*status)
	}

	if len(payload) == 1 {
		log.Fatal("At least one of -url, -health-url, or -status must be provided")
	}

	path := fmt.Sprintf("/notification-subscriptions/v2/webhooks/%s", *id)
	resp, body, err := client.SendRequest("PATCH", path, payload)
	handleResponse(resp, body, err)
}

func cmdDelete(args []string) {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	id := fs.String("id", "", "Webhook subscription ID (required)")
	fs.Parse(args)

	if *id == "" {
		log.Fatal("Missing required flag: -id")
	}

	path := fmt.Sprintf("/notification-subscriptions/v2/webhooks/%s", *id)
	resp, body, err := client.SendRequest("DELETE", path, nil)
	if err != nil {
		log.Fatalf("Request failed: %v", err)
	}

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		fmt.Printf("Subscription %s deleted successfully.\n", *id)
	} else {
		var result interface{}
		_ = json.Unmarshal(body, &result)
		fmt.Fprintf(os.Stderr, "Error (HTTP %d):\n", resp.StatusCode)
		printJSON(result)
		os.Exit(1)
	}
}

func cmdGenerateKey() {
	path := "/kms/egress/v2/keys-sym"
	payload := map[string]interface{}{
		"clientRequestAction": "CREATE",
		"keyInformation": map[string]interface{}{
			"provider":       "nrtd",
			"tenant":         merchantID,
			"keyType":        "sharedSecret",
			"organizationId": merchantID,
		},
	}

	resp, body, err := client.SendRequest("POST", path, payload)
	if err != nil {
		log.Fatalf("Request failed: %v", err)
	}

	var result map[string]interface{}
	_ = json.Unmarshal(body, &result)

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		fmt.Println("Webhook Symmetric Key created successfully!")
		if keyInfo, ok := result["keyInformation"].(map[string]interface{}); ok {
			if keyID, ok := keyInfo["keyId"].(string); ok {
				fmt.Printf("  Key ID:     %s\n", keyID)
			}
			if key, ok := keyInfo["key"].(string); ok {
				fmt.Printf("  Secret Key: %s\n", key)
			}
		}
		fmt.Println("\nAdd these to your .env file:")
		if keyInfo, ok := result["keyInformation"].(map[string]interface{}); ok {
			fmt.Printf("CS_WEBHOOK_KEY_ID=%s\n", keyInfo["keyId"])
			fmt.Printf("CS_WEBHOOK_KEY=%s\n", keyInfo["key"])
		}
	} else {
		fmt.Fprintf(os.Stderr, "Error (HTTP %d):\n", resp.StatusCode)
		printJSON(result)
		os.Exit(1)
	}
}

func cmdGenerateMLEKey() {
	keyID := os.Getenv("CS_KEY_ID")
	privPEM, certPEM, certBase64, err := cybersource.GenerateMLEKeyPair(merchantID, keyID)
	if err != nil {
		log.Fatalf("Failed to generate MLE key pair: %v", err)
	}

	// Create certs directory
	certsDir := "certs"
	if err := os.MkdirAll(certsDir, 0700); err != nil {
		log.Fatalf("Failed to create certs directory: %v", err)
	}

	privPath := filepath.Join(certsDir, "mle_private.pem")
	certPath := filepath.Join(certsDir, "mle_certificate.pem")

	if err := os.WriteFile(privPath, privPEM, 0600); err != nil {
		log.Fatalf("Failed to save private key: %v", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		log.Fatalf("Failed to save certificate: %v", err)
	}
	fmt.Printf("Generated RSA-2048 private key: %s\n", privPath)
	fmt.Printf("Generated X.509 certificate:   %s\n", certPath)

	// Register with Cybersource KMS
	fmt.Println("Registering certificate with Cybersource KMS (POST /kms/egress/v2/keys-asym)...")
	res, err := client.RegisterAsymmetricKey(certBase64)
	if err != nil {
		log.Fatalf("KMS certificate registration failed: %v", err)
	}

	mleKeyID := res.KeyInformation.KeyID
	fmt.Println("\nMessage-Level Encryption (MLE) Certificate Registered Successfully!")
	fmt.Printf("  KMS Key ID:        %s\n", mleKeyID)
	fmt.Printf("  Key Type:          %s\n", res.KeyInformation.KeyType)
	fmt.Printf("  Status:            %s\n", res.KeyInformation.Status)
	fmt.Printf("  Expiration Date:   %s\n", res.KeyInformation.ExpirationDate)

	updateEnvMLEKey(mleKeyID, privPath)
}

func cmdGetMLECert(args []string) {
	fs := flag.NewFlagSet("get-mle-cert", flag.ExitOnError)
	id := fs.String("id", "", "KMS Key ID of the registered certificate (optional, defaults to CS_MLE_KEY_ID)")
	certFile := fs.String("cert", "certs/mle_certificate.pem", "Path to certificate file")
	fs.Parse(args)

	if *id == "" {
		*id = os.Getenv("CS_MLE_KEY_ID")
	}

	fmt.Println("Message-Level Encryption (MLE) Certificate Details:")
	if *id != "" {
		fmt.Printf("  Registered KMS Key ID: %s\n", *id)
	}

	// Read and parse certificate file
	certData, err := os.ReadFile(*certFile)
	if err != nil {
		*certFile = filepath.Join("backend", *certFile)
		certData, err = os.ReadFile(*certFile)
	}
	if err != nil {
		fmt.Printf("  Certificate file not found (%v).\n  To generate and register one, run: go run cmd/webhook/main.go generate-mle-key\n", err)
		return
	}

	block, _ := pem.Decode(certData)
	if block == nil {
		log.Fatalf("Failed to decode certificate PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		log.Fatalf("Failed to parse X.509 certificate: %v", err)
	}

	fmt.Printf("  Certificate File:      %s\n", *certFile)
	fmt.Printf("  Subject Common Name:   %s\n", cert.Subject.CommonName)
	fmt.Printf("  Subject Organization:  %v\n", cert.Subject.Organization)
	fmt.Printf("  Serial Number:         %s\n", cert.SerialNumber.String())
	fmt.Printf("  Valid From:            %s\n", cert.NotBefore.Format(time.RFC3339))
	fmt.Printf("  Valid Until:           %s\n", cert.NotAfter.Format(time.RFC3339))
	fmt.Printf("  Public Key Algorithm:  %s (RSA 2048)\n", cert.PublicKeyAlgorithm.String())
}

func updateEnvMLEKey(keyID, privPath string) {
	envPath := ".env"
	content, err := os.ReadFile(envPath)
	if err != nil {
		envPath = "backend/.env"
		content, err = os.ReadFile(envPath)
		if err != nil {
			fmt.Printf("\nCould not locate .env file to update automatically. Add manually:\nCS_MLE_KEY_ID=%s\nCS_MLE_KEY_PATH=%s\n", keyID, privPath)
			return
		}
	}

	lines := strings.Split(string(content), "\n")
	foundID := false
	foundPath := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "CS_MLE_KEY_ID=") {
			lines[i] = fmt.Sprintf("CS_MLE_KEY_ID=%s", keyID)
			foundID = true
		} else if strings.HasPrefix(trimmed, "CS_MLE_KEY_PATH=") {
			lines[i] = fmt.Sprintf("CS_MLE_KEY_PATH=%s", privPath)
			foundPath = true
		}
	}

	if !foundID {
		lines = append(lines, fmt.Sprintf("CS_MLE_KEY_ID=%s", keyID))
	}
	if !foundPath {
		lines = append(lines, fmt.Sprintf("CS_MLE_KEY_PATH=%s", privPath))
	}

	_ = os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0644)
	fmt.Printf("\nUpdated %s with CS_MLE_KEY_ID and CS_MLE_KEY_PATH\n", envPath)
}
