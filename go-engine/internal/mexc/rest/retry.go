package rest

import "errors"

// MEXC rate-limit and transient error codes.
//
// These are the codes worth RETRYING. Everything else — a rejected order, insufficient balance, an
// unknown instrument — is a decision the exchange has made and repeating it just wastes the rate
// budget that a genuinely retryable call needs.
//
// Deliberately matched on the STRUCTURED code, not by string-searching the message. CLAUDE.md §38.1
// records what the alternative costs: OKX's per-item failures were reported as an empty top-level
// message for two rounds of debugging because the structured detail had been discarded.
const (
	// codeRateLimited is MEXC's "too many requests".
	codeRateLimited = 510
	// codeSystemBusy is a transient server-side backpressure signal.
	codeSystemBusy = 500
	// codeInternalError is MEXC's generic server error, retryable for the same reason a 5xx is.
	codeInternalError = 501
)

// IsRetryableError reports whether err is worth retrying with backoff.
//
// Exported because cmd's gateway wiring needs it, and kept beside the client whose errors it
// classifies rather than in the gateway — the gateway must not need to know any exchange's error
// vocabulary, which is the whole reason it takes this as a function.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		// A non-API error is a transport failure (dial timeout, connection reset). Those are
		// exactly the transient case retrying exists for.
		return true
	}
	switch apiErr.Code {
	case codeRateLimited, codeSystemBusy, codeInternalError:
		return true
	}
	// An HTTP 429 that did not carry a recognised body code is still a rate limit.
	return apiErr.Status == 429
}
