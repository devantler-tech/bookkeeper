// Package stripefees reads charge fees from Stripe's balance ledger.
package stripefees

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const (
	apiURL     = "https://api.stripe.com/v1/balance_transactions"
	apiVersion = "2026-11-18.endive"
	maxPages   = 50
	pageSize   = 100
	maxReply   = 2 << 20
)

var (
	errStripe = errors.New("could not read the Stripe balance ledger")
	errPage   = errors.New("invalid page from the Stripe balance ledger")
	errLimit  = errors.New("too many stripe transactions; use a shorter reporting window")
	validID   = regexp.MustCompile(`^txn_[A-Za-z0-9_]{1,80}$`)
)

// Fee is a DKK charge fee in øre.
type Fee struct {
	TransactionID string
	Date          string
	Amount        int64
}

// Result separates eligible fees from rows this importer leaves for review.
type Result struct {
	Fees     []Fee
	Deferred int
}

//nolint:tagliatelle // The field names are defined by Stripe's API.
type transaction struct {
	ID           string       `json:"id"`
	Category     string       `json:"reporting_category"`
	Currency     string       `json:"currency"`
	Status       string       `json:"status"`
	ExchangeRate *json.Number `json:"exchange_rate"`
	Created      int64        `json:"created"`
	Amount       int64        `json:"amount"`
	Fee          int64        `json:"fee"`
	Net          int64        `json:"net"`
	FeeDetails   []feeDetail  `json:"fee_details"`
}

type feeDetail struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Type     string `json:"type"`
}

//nolint:tagliatelle // The field names are defined by Stripe's API.
type page struct {
	HasMore *bool         `json:"has_more"`
	Data    []transaction `json:"data"`
}

// Read walks the complete half-open reporting window before returning any fees.
func Read(
	ctx context.Context,
	client *http.Client,
	key string,
	since, until time.Time,
) (Result, error) {
	cursor := ""
	seen := make(map[string]struct{})

	var out Result

	for range maxPages {
		next, err := readPage(ctx, client, key, since, until, cursor)
		if err != nil {
			return Result{}, err
		}

		for _, txn := range next.Data {
			if !validID.MatchString(txn.ID) || txn.Created < since.Unix() ||
				txn.Created >= until.Unix() {
				return Result{}, errPage
			}

			if _, duplicate := seen[txn.ID]; duplicate {
				return Result{}, errPage
			}

			seen[txn.ID] = struct{}{}

			if txn.eligible() {
				out.Fees = append(out.Fees, Fee{
					TransactionID: txn.ID,
					Date:          time.Unix(txn.Created, 0).UTC().Format(time.DateOnly),
					Amount:        txn.Fee,
				})
			} else {
				out.Deferred++
			}
		}

		if !*next.HasMore {
			return out, nil
		}

		cursor = next.Data[len(next.Data)-1].ID
	}

	return Result{}, errLimit
}

func readPage(
	ctx context.Context,
	client *http.Client,
	key string,
	since, until time.Time,
	cursor string,
) (page, error) {
	query := url.Values{
		"created[gte]": {strconv.FormatInt(since.Unix(), 10)},
		"created[lt]":  {strconv.FormatInt(until.Unix(), 10)},
		"limit":        {strconv.Itoa(pageSize)},
	}
	if cursor != "" {
		query.Set("starting_after", cursor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"?"+query.Encode(), nil)
	if err != nil {
		return page{}, errStripe
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Stripe-Version", apiVersion)

	resp, err := client.Do(req)
	if err != nil {
		return page{}, errStripe
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return page{}, fmt.Errorf("%w: HTTP %d", errStripe, resp.StatusCode)
	}

	return decodePage(resp.Body)
}

func decodePage(response io.Reader) (page, error) {
	body, err := io.ReadAll(io.LimitReader(response, maxReply+1))
	if err != nil || len(body) > maxReply {
		return page{}, errPage
	}

	var next page
	if json.Unmarshal(body, &next) != nil || next.HasMore == nil || next.Data == nil ||
		len(next.Data) > pageSize || (*next.HasMore && len(next.Data) == 0) {
		return page{}, errPage
	}

	return next, nil
}

func (txn transaction) eligible() bool {
	if txn.Category != "charge" || txn.Currency != "dkk" || txn.Status != "available" ||
		txn.ExchangeRate != nil {
		return false
	}

	return txn.validFeeBreakdown()
}

func (txn transaction) validFeeBreakdown() bool {
	if txn.Fee <= 0 || txn.Amount <= txn.Fee || txn.Net != txn.Amount-txn.Fee {
		return false
	}

	remaining := txn.Fee
	for _, detail := range txn.FeeDetails {
		if detail.Type != "stripe_fee" || detail.Currency != "dkk" || detail.Amount <= 0 ||
			detail.Amount > remaining {
			return false
		}

		remaining -= detail.Amount
	}

	return remaining == 0
}
