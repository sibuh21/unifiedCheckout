# Unified Checkout Backend & Webhook CLI Guide

This directory contains the Go backend server and the standalone Webhook & Message-Level Encryption (MLE) management CLI tool.

---

## 🚀 Quick CLI Guide

From this directory (`backend/`):

### **1. Generate & Register MLE Certificate (Steps 1 & 2)**
```bash
go run cmd/webhook/main.go generate-mle-key
```
*Generates an RSA-2048 private key and X.509 certificate, registers it with Cybersource KMS via `POST /kms/egress/v2/keys-asym`, and updates `.env`.*

### **2. Generate Webhook Digital Signature Key (Step 3)**
```bash
go run cmd/webhook/main.go generate-key
```
*Creates an HMAC-SHA256 symmetric secret via `POST /kms/egress/v2/keys-sym` and updates `.env`.*

### **3. List Subscribed Products & Events (Step 4)**
```bash
go run cmd/webhook/main.go products
```

### **4. Create Webhook Subscription (Step 5)**
```bash
go run cmd/webhook/main.go create -url "https://<your-tunnel-url>/api/cybersource/webhooks/events"
```

### **5. List & Manage Subscriptions (Step 6)**
```bash
# List all subscriptions
go run cmd/webhook/main.go list

# Get subscription details
go run cmd/webhook/main.go get -id <webhookId>

# Update subscription delivery URL
go run cmd/webhook/main.go update -id <webhookId> -url "https://<new-tunnel-url>/api/cybersource/webhooks/events"

# Reactivate suspended webhook
go run cmd/webhook/main.go activate -id <webhookId>

# Delete subscription
go run cmd/webhook/main.go delete -id <webhookId>
```

### **6. Inspect Registered MLE Certificate**
```bash
go run cmd/webhook/main.go get-mle-cert
```

---

## 🏃 Running the Backend Server

```bash
go run .
```

* Listens on `http://localhost:8080`.
* Creates Capture Context sessions (`POST /api/cybersource/capture-context`).
* Ingests and verifies Cybersource webhooks (`POST /api/cybersource/webhooks/events`).
* Decrypts MLE JWE payloads (`encData`) using `CS_MLE_KEY_PATH`.

