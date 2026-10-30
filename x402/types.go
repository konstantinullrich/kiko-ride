package x402

import "encoding/json"

// Version is the x402 protocol version this package speaks.
const Version = 2

// HTTP header names from the x402 HTTP transport.
const (
	HeaderPaymentRequired  = "PAYMENT-REQUIRED"
	HeaderPaymentSignature = "PAYMENT-SIGNATURE"
	HeaderPaymentResponse  = "PAYMENT-RESPONSE"
)

// Network is a Cardano x402 network identifier.
type Network string

const (
	Mainnet Network = "cardano:mainnet"
	Preprod Network = "cardano:preprod"
	Preview Network = "cardano:preview"
)

// Lovelace is the asset id for ADA payments.
const Lovelace = "lovelace"

// ResourceInfo describes the protected resource.
type ResourceInfo struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// PaymentRequirements is one acceptable way to pay.
type PaymentRequirements struct {
	Scheme            string         `json:"scheme"`
	Network           Network        `json:"network"`
	Amount            string         `json:"amount"`
	Asset             string         `json:"asset"`
	PayTo             string         `json:"payTo"`
	MaxTimeoutSeconds int            `json:"maxTimeoutSeconds"`
	Extra             map[string]any `json:"extra,omitempty"`
}

// PaymentRequired is sent with a 402 response.
type PaymentRequired struct {
	X402Version int                   `json:"x402Version"`
	Error       string                `json:"error,omitempty"`
	Resource    ResourceInfo          `json:"resource"`
	Accepts     []PaymentRequirements `json:"accepts"`
}

// CardanoPayload is the scheme payload for exact on Cardano.
type CardanoPayload struct {
	// Transaction is the signed, unbroadcast transaction (base64 CBOR).
	Transaction string `json:"transaction"`
	// Nonce is a UTxO reference (txHash#index) spent by the transaction.
	Nonce string `json:"nonce"`
}

// PaymentPayload is what the client sends in PAYMENT-SIGNATURE.
type PaymentPayload struct {
	X402Version int                 `json:"x402Version"`
	Resource    *ResourceInfo       `json:"resource,omitempty"`
	Accepted    PaymentRequirements `json:"accepted"`
	Payload     CardanoPayload      `json:"payload"`
	Extensions  json.RawMessage     `json:"extensions,omitempty"`
}

// VerifyResponse is the facilitator's /verify answer.
type VerifyResponse struct {
	IsValid       bool   `json:"isValid"`
	InvalidReason string `json:"invalidReason,omitempty"`
	Payer         string `json:"payer,omitempty"`
}

// SettleResponse is the facilitator's /settle answer, also returned to the
// client in PAYMENT-RESPONSE.
type SettleResponse struct {
	Success     bool           `json:"success"`
	ErrorReason string         `json:"errorReason,omitempty"`
	Payer       string         `json:"payer,omitempty"`
	Transaction string         `json:"transaction"`
	Network     Network        `json:"network"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// Error reasons used by this package; facilitators may return others.
const (
	ReasonInvalidPayload      = "invalid_payload"
	ReasonRequirementsChanged = "invalid_payment_requirements"
	ReasonSettlementPending   = "settlement_pending"
	ReasonDuplicateSettlement = "duplicate_settlement"
	ReasonUnexpectedVerify    = "unexpected_verify_error"
	ReasonUnexpectedSettle    = "unexpected_settle_error"
)
