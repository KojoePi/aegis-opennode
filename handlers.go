package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// -----------------------------------------------------------------------------
// Previous Bitvora webhook types.
// These are kept as comments for migration reference only. They are no longer
// compiled or used by the OpenNode payment implementation.They will be removed soon.
//
//	type WebhookPayload struct {
//		Event string `json:"event"`
//		Data  struct {
//			AmountSats         int       `json:"amount_sats"`
//			ChainTxID          *string   `json:"chain_tx_id"`
//			CreatedAt          time.Time `json:"created_at"`
//			FeeSats            float64   `json:"fee_sats"`
//			ID                 string    `json:"id"`
//			LightningInvoiceID string    `json:"lightning_invoice_id"`
//			Metadata           *Metadata `json:"metadata"`
//			NetworkType        string    `json:"network_type"`
//			RailType           string    `json:"rail_type"`
//			Recipient          string    `json:"recipient"`
//			Status             string    `json:"status"`
//			UpdatedAt          time.Time `json:"updated_at"`
//		} `json:"data"`
//	}
//
//	type Metadata struct {
//		Npub string `json:"npub"`
//	}
//
// OpenNodeChargeResponse contains the fields Aegis needs from a charge
// creation response. The Lightning BOLT11 invoice is in
// data.lightning_invoice.payreq.
type OpenNodeChargeResponse struct {
	Data struct {
		ID                string `json:"id"`
		Status            string `json:"status"`
		OrderID           string `json:"order_id"`
		HostedCheckoutURL string `json:"hosted_checkout_url"`
		LightningInvoice  struct {
			Payreq    string `json:"payreq"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"lightning_invoice"`
	} `json:"data"`
}

// OpenNodeWebhook contains the form fields required to validate a charge
// webhook and activate the corresponding Aegis subscription.
type OpenNodeWebhook struct {
	ID          string
	Status      string
	OrderID     string
	HashedOrder string
}

type Response struct {
	Status  int         `json:"status"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type PageData struct {
	Header      string
	Description string
	Price       string
}

func JSONResponse(w http.ResponseWriter, status int, message string, data interface{}) {
	response := Response{
		Status:  status,
		Message: message,
		Data:    data,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleGenerateInvoice(w http.ResponseWriter, r *http.Request) {
	// -------------------------------------------------------------------------
	// Original Bitvora implementation (kept here as comments for reference):
	//
	// bitvoraApiKey := os.Getenv("BITVORA_API_KEY")
	// pricePerYearStr := os.Getenv("PRICE_PER_YEAR")
	// pricePerYearFloat, err := strconv.ParseFloat(pricePerYearStr, 64)
	// ...
	// metadata := map[string]string{"npub": npub}
	// bitvoraClient := bitvora.NewBitvoraClient(bitvora.Mainnet, bitvoraApiKey)
	// invoice, err := bitvoraClient.CreateLightningInvoice(
	//     pricePerYearFloat,
	//     string(bitvora.SATS),
	//     "1 year subscription",
	//     3600,
	//     metadata,
	// )
	// response.Invoice = invoice.Data.PaymentRequest
	//
	// The code above has been replaced by the OpenNode REST API call below.
	// -------------------------------------------------------------------------

	if r.Method != http.MethodPost {
		JSONResponse(w, http.StatusMethodNotAllowed, "Invalid request method", nil)
		return
	}

	openNodeAPIKey := strings.TrimSpace(os.Getenv("OPENNODE_API_KEY"))
	if openNodeAPIKey == "" {
		log.Println("OPENNODE_API_KEY is not configured")
		JSONResponse(w, http.StatusInternalServerError, "Payment service is not configured", nil)
		return
	}

	pricePerYearStr := strings.TrimSpace(os.Getenv("PRICE_PER_YEAR"))
	pricePerYear, err := strconv.ParseInt(pricePerYearStr, 10, 32)
	if err != nil || pricePerYear <= 0 {
		log.Printf("Invalid PRICE_PER_YEAR: %q", pricePerYearStr)
		JSONResponse(w, http.StatusInternalServerError, "Invalid price per year", nil)
		return
	}

	// Parse the JSON body sent by the existing frontend.
	var requestData struct {
		Npub string `json:"npub"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestData); err != nil || strings.TrimSpace(requestData.Npub) == "" {
		JSONResponse(w, http.StatusBadRequest, "Missing or invalid npub", nil)
		return
	}

	npub := strings.TrimSpace(requestData.Npub)

	// OpenNode's "order_id" is used to carry the npub through the payment
	// lifecycle. This avoids relying on undocumented/custom metadata fields.
	// The existing subscription functions continue to use the npub exactly as
	// before.
	callbackURL := strings.TrimSpace(os.Getenv("OPENNODE_WEBHOOK_URL"))
	if callbackURL == "" {
		JSONResponse(w, http.StatusInternalServerError, "OpenNode webhook URL is not configured", nil)
		return
	}

	chargeRequest := struct {
		Amount      int64  `json:"amount"`
		Description string `json:"description"`
		OrderID     string `json:"order_id"`
		CallbackURL string `json:"callback_url"`
	}{
		Amount:      pricePerYear,
		Description: "Relayted Premium Relay & Blossom Server - 1 year subscription",
		OrderID:     npub,
		CallbackURL: callbackURL,
	}

	requestBody, err := json.Marshal(chargeRequest)
	if err != nil {
		log.Printf("Failed to encode OpenNode charge request: %v", err)
		JSONResponse(w, http.StatusInternalServerError, "Error creating payment request", nil)
		return
	}

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		"https://api.opennode.com/v1/charges",
		strings.NewReader(string(requestBody)),
	)
	if err != nil {
		log.Printf("Failed to create OpenNode request: %v", err)
		JSONResponse(w, http.StatusInternalServerError, "Error creating payment request", nil)
		return
	}

	req.Header.Set("Authorization", openNodeAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("OpenNode API request failed: %v", err)
		JSONResponse(w, http.StatusBadGateway, "Payment provider unavailable", nil)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read OpenNode response: %v", err)
		JSONResponse(w, http.StatusBadGateway, "Invalid payment provider response", nil)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not expose the OpenNode API response to the browser. It can contain
		// provider-specific details that are useful only in server logs.
		log.Printf("OpenNode returned HTTP %d: %s", resp.StatusCode, string(respBody))
		JSONResponse(w, http.StatusBadGateway, "Error creating invoice", nil)
		return
	}

	var charge OpenNodeChargeResponse
	if err := json.Unmarshal(respBody, &charge); err != nil {
		log.Printf("Failed to parse OpenNode response: %v", err)
		JSONResponse(w, http.StatusBadGateway, "Invalid payment provider response", nil)
		return
	}

	if charge.Data.LightningInvoice.Payreq == "" {
		log.Printf("OpenNode response did not contain a Lightning payment request: %s", string(respBody))
		JSONResponse(w, http.StatusBadGateway, "No Lightning invoice returned", nil)
		return
	}

	// Preserve the existing Aegis subscription flow: create the pending
	// subscription before payment, then mark it paid from the webhook.
	createSubscription(npub)

	var response struct {
		Invoice string `json:"invoice"`
	}
	response.Invoice = charge.Data.LightningInvoice.Payreq

	JSONResponse(w, http.StatusOK, "Invoice generated", response)
}

func handleHomePage(w http.ResponseWriter, r *http.Request) {
	// Define the variables to pass to the template
	data := PageData{
		Header:      os.Getenv("RELAY_NAME"),
		Description: os.Getenv("RELAY_DESCRIPTION"),
		Price:       os.Getenv("PRICE_PER_YEAR"),
	}

	// Parse the template file
	tmpl, err := template.ParseFiles("static/index.html")
	if err != nil {
		http.Error(w, "Unable to load template", http.StatusInternalServerError)
		return
	}

	// Render the template with the provided data
	err = tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, "Unable to render template", http.StatusInternalServerError)
	}
}

func handleOpenNodeWebhook(w http.ResponseWriter, r *http.Request) {
	// -------------------------------------------------------------------------
	// Original Bitvora webhook implementation (kept conceptually as comments):
	//
	// func handleBitvoraWebhook(...) {
	//     secret := os.Getenv("BITVORA_WEBHOOK_SECRET")
	//     signature := r.Header.Get("bitvora-signature")
	//     ...
	//     if webhookPayload.Event == "deposit.lightning.completed" {
	//         npub := webhookPayload.Data.Metadata.Npub
	//         setPaidSubscription(npub)
	//         loadWhitelist()
	//     }
	// }
	//
	// OpenNode uses a different webhook contract: POST form-urlencoded data
	// containing id, status, order_id and hashed_order. The signature is
	// HMAC-SHA256 using the OpenNode API key and the charge ID as the message.
	// -------------------------------------------------------------------------

	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	openNodeAPIKey := strings.TrimSpace(os.Getenv("OPENNODE_API_KEY"))
	if openNodeAPIKey == "" {
		log.Println("OPENNODE_API_KEY is not configured")
		http.Error(w, "Payment service is not configured", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid webhook payload", http.StatusBadRequest)
		return
	}

	webhook := OpenNodeWebhook{
		ID:          strings.TrimSpace(r.FormValue("id")),
		Status:      strings.TrimSpace(r.FormValue("status")),
		OrderID:     strings.TrimSpace(r.FormValue("order_id")),
		HashedOrder: strings.TrimSpace(r.FormValue("hashed_order")),
	}

	if webhook.ID == "" || webhook.HashedOrder == "" {
		log.Println("OpenNode webhook missing id or hashed_order")
		http.Error(w, "Invalid webhook payload", http.StatusBadRequest)
		return
	}

	// OpenNode documents the signature as:
	// HMAC-SHA256(API_KEY, charge.id)
	mac := hmac.New(sha256.New, []byte(openNodeAPIKey))
	_, _ = mac.Write([]byte(webhook.ID))
	expectedSignature := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expectedSignature), []byte(webhook.HashedOrder)) {
		log.Printf("Invalid OpenNode webhook signature for charge %s", webhook.ID)
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	log.Printf("Valid OpenNode webhook: charge=%s status=%s order_id=%s", webhook.ID, webhook.Status, webhook.OrderID)

	// Only a settled Lightning payment activates the subscription.
	// Other OpenNode charge states (underpaid, processing, expired, failed,
	// refunded) must not activate access.
	if webhook.Status == "paid" {
		if webhook.OrderID == "" {
			log.Printf("Paid OpenNode charge %s has no order_id", webhook.ID)
			http.Error(w, "Missing order_id", http.StatusBadRequest)
			return
		}

		npub := webhook.OrderID
		setPaidSubscription(npub)

		if _, err := loadWhitelist(); err != nil {
			log.Printf("Failed to reload whitelist after payment: %v", err)
		}
	}

	// OpenNode expects a successful HTTP response after receiving the webhook.
	w.WriteHeader(http.StatusOK)
}

func handlePollPayment(w http.ResponseWriter, r *http.Request) {
	// Ensure the request method is POST
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	// Parse the JSON body
	var requestData struct {
		Npub string `json:"npub"`
	}
	err := json.NewDecoder(r.Body).Decode(&requestData)
	if err != nil || requestData.Npub == "" {
		JSONResponse(w, http.StatusBadRequest, "Missing or invalid npub", nil)
		return
	}

	// Process the payment polling
	active := pollPayment(requestData.Npub)
	var response struct {
		Active bool `json:"active"`
	}

	response.Active = active
	JSONResponse(w, http.StatusOK, "Payment status", response)
}

func validateWebhookSignature(payload, signature, secret string) bool {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	expectedSignature := hex.EncodeToString(h.Sum(nil))
	return expectedSignature == signature
}
