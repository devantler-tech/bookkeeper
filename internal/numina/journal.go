package numina

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"
)

var (
	errDraftAccess = errors.New(
		"numina requires journal read and draft write access only, for the configured company",
	)
	errDraft = errors.New("numina could not confirm the stripe fee draft")
)

const orePerKrone = 100

// StripeFeeDraft specifies the two configured accounts and a positive DKK fee in øre.
type StripeFeeDraft struct {
	TransactionID  string
	Date           string
	Amount         int64
	BalanceAccount int
	FeeAccount     int
}

//nolint:tagliatelle // The field names are defined by Numina's API.
type draftLine struct {
	Account int         `json:"account_number"`
	Amount  json.Number `json:"amount"`
}

//nolint:tagliatelle // The field names are defined by Numina's API.
type draftPayload struct {
	Date      string      `json:"date"`
	Text      string      `json:"text"`
	Reference string      `json:"external_reference"`
	Lines     []draftLine `json:"lines"`
}

//nolint:tagliatelle // The field names are defined by Numina's API.
type entryConfirmation struct {
	ID        string `json:"id"`
	Reference string `json:"external_reference"`
	Status    string `json:"status"`
}

type journalAnswer struct {
	answer

	replayed bool
}

// CheckDraftAccess detects credentials for the wrong company or with extra scopes.
func (c *Client) CheckDraftAccess(ctx context.Context, company string) error {
	answer, err := c.journalRequest(ctx, http.MethodGet, "/v1/me", nil, "")
	if err != nil {
		return err
	}

	var credentials struct {
		Company struct {
			ID string `json:"id"`
		} `json:"company"`
		AuthType string   `json:"auth_type"` //nolint:tagliatelle // Numina's wire name.
		Scopes   []string `json:"scopes"`
	}

	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &credentials) != nil ||
		company == "" || credentials.Company.ID != company ||
		credentials.AuthType != "oauth" || len(credentials.Scopes) != 2 ||
		!slices.Contains(
			credentials.Scopes,
			"journal:read",
		) || !slices.Contains(credentials.Scopes, "journal:write") {
		return errDraftAccess
	}

	return nil
}

// CreateStripeFeeDraft submits one balanced draft. existing is true when Numina
// already holds the same external reference, including a draft later canceled.
func (c *Client) CreateStripeFeeDraft(
	ctx context.Context,
	fee StripeFeeDraft,
) (bool, error) {
	payload, err := fee.payload()
	if err != nil {
		return false, err
	}

	request, err := json.Marshal(payload)
	if err != nil {
		return false, errDraft
	}

	answer, err := c.journalRequest(
		ctx,
		http.MethodPost,
		"/v1/journal-entries",
		request,
		payload.Reference,
	)
	if err != nil {
		return false, err
	}

	switch answer.status {
	case http.StatusCreated:
		return confirmedDraft(answer, payload.Reference)
	case http.StatusConflict:
		return c.confirmExisting(ctx, payload.Reference)
	case http.StatusUnprocessableEntity:
		var refusal struct {
			Code string `json:"error"`
		}

		err = json.Unmarshal(answer.body, &refusal)
		if err == nil && refusal.Code == "idempotency_key_reused" {
			return c.confirmExisting(ctx, payload.Reference)
		}
	}

	return false, fmt.Errorf("%w: HTTP %d", errDraft, answer.status)
}

func confirmedDraft(answer journalAnswer, reference string) (bool, error) {
	var entry entryConfirmation

	if json.Unmarshal(answer.body, &entry) != nil || entry.ID == "" ||
		entry.Reference != reference || entry.Status != "open" {
		return false, errDraft
	}

	return answer.replayed, nil
}

func (fee StripeFeeDraft) payload() (draftPayload, error) {
	_, dateErr := time.Parse(time.DateOnly, fee.Date)
	if dateErr != nil || fee.Amount <= 0 || fee.FeeAccount <= 0 || fee.BalanceAccount <= 0 ||
		fee.FeeAccount == fee.BalanceAccount || fee.TransactionID == "" {
		return draftPayload{}, errDraft
	}

	amount := fmt.Sprintf("%d.%02d", fee.Amount/orePerKrone, fee.Amount%orePerKrone)

	return draftPayload{
		Date:      fee.Date,
		Text:      "Stripe processing fee",
		Reference: "bookkeeper:stripe-fee:" + fee.TransactionID,
		Lines: []draftLine{
			{Account: fee.FeeAccount, Amount: json.Number(amount)},
			{Account: fee.BalanceAccount, Amount: json.Number("-" + amount)},
		},
	}, nil
}

func (c *Client) confirmExisting(ctx context.Context, ref string) (bool, error) {
	query := url.Values{"external_reference": {ref}, "limit": {"2"}}

	answer, err := c.journalRequest(
		ctx,
		http.MethodGet,
		"/v1/journal-entries?"+query.Encode(),
		nil,
		"",
	)
	if err != nil {
		return false, err
	}

	var page struct {
		Entries []entryConfirmation `json:"journal_entries"` //nolint:tagliatelle // Numina's wire name.
		HasMore *bool               `json:"has_more"`        //nolint:tagliatelle // Numina's wire name.
	}

	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &page) != nil ||
		page.HasMore == nil || *page.HasMore || len(page.Entries) != 1 {
		return false, errDraft
	}

	if page.Entries[0].ID == "" || page.Entries[0].Reference != ref {
		return false, errDraft
	}

	return true, nil
}

func (c *Client) journalRequest(
	ctx context.Context,
	method, path string,
	body []byte,
	key string,
) (journalAnswer, error) {
	for attempt := 0; ; attempt++ {
		got, err := c.sendJournalRequest(ctx, method, path, body, key)
		if err != nil {
			return journalAnswer{}, err
		}

		wait, retry := retryAfter(got.retryAfter, time.Now())
		if got.status != http.StatusTooManyRequests || attempt >= maxRateLimitRetries || !retry {
			return got, nil
		}

		err = c.sleep(ctx, wait)
		if err != nil {
			return journalAnswer{}, err
		}
	}
}

func (c *Client) sendJournalRequest(
	ctx context.Context,
	method, path string,
	body []byte,
	key string,
) (journalAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return journalAnswer{}, errDraft
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return journalAnswer{}, errUnreachable
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil || len(data) > maxAnswerBytes {
		return journalAnswer{}, errUnreadable
	}

	return journalAnswer{
		answer: answer{
			status:     resp.StatusCode,
			retryAfter: resp.Header.Get("Retry-After"),
			body:       data,
		},
		replayed: resp.Header.Get("Idempotent-Replayed") == "true",
	}, nil
}
