package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	fixtureCompany = "00000000-0000-4000-8000-000000000025"
	fixtureRef     = "bookkeeper:stripe-fee:txn_fixture_1"
)

type stripeFeeTransport struct {
	target *url.URL
}

func (transport stripeFeeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	local := request.Clone(request.Context())
	local.URL.Scheme, local.URL.Host = transport.target.Scheme, transport.target.Host

	response, err := http.DefaultTransport.RoundTrip(local)
	if err != nil {
		return nil, fmt.Errorf("fixture transport: %w", err)
	}

	return response, nil
}

type stripeFeeFixture struct {
	client *http.Client
	reads  atomic.Int32
	writes atomic.Int32
}

func newStripeFeeFixture(t *testing.T) *stripeFeeFixture {
	t.Helper()

	var fixture stripeFeeFixture

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/balance_transactions", fixture.ledger(t))
	mux.HandleFunc("GET /v1/me", func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, `{"auth_type":"oauth","company":{"id":"`+fixtureCompany+
			`"},"scopes":["journal:read","journal:write"]}`)
	})
	mux.HandleFunc("POST /v1/journal-entries", fixture.createDraft(t))
	mux.HandleFunc(
		"GET /v1/journal-entries",
		func(response http.ResponseWriter, request *http.Request) {
			if request.URL.Query().Get("external_reference") != fixtureRef {
				t.Error("replay did not look up the original draft")
			}

			_, _ = io.WriteString(
				response,
				`{"has_more":false,"journal_entries":[{"id":"fixture-entry","external_reference":"`+
					fixtureRef+`"}]}`,
			)
		},
	)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	fixture.client = newHTTPClient()
	fixture.client.Transport = stripeFeeTransport{target: target}

	return &fixture
}

func (fixture *stripeFeeFixture) ledger(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(response http.ResponseWriter, request *http.Request) {
		fixture.reads.Add(1)

		query := request.URL.Query()
		if query.Get("created[gte]") != "1769904000" || query.Get("created[lt]") != "1772323200" {
			t.Error("Stripe request missed the explicit reporting window")
		}

		switch query.Get("starting_after") {
		case "":
			_, _ = io.WriteString(
				response,
				`{"has_more":true,"data":[{"id":"txn_fixture_1","reporting_category":"charge",
				"currency":"dkk","status":"available","created":1769990400,"amount":20000,"fee":359,"net":19641,
				"fee_details":[{"type":"stripe_fee","currency":"dkk","amount":359}]}]}`,
			)
		case "txn_fixture_1":
			_, _ = io.WriteString(
				response,
				`{"has_more":false,"data":[{"id":"txn_fixture_2","reporting_category":"payout",
				"currency":"dkk","status":"available","created":1769990400,"amount":-19641,"fee":0,"net":-19641}]}`,
			)
		default:
			t.Error("Stripe request did not continue from the previous page")
			response.WriteHeader(http.StatusBadRequest)
		}
	}
}

func (fixture *stripeFeeFixture) createDraft(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(response http.ResponseWriter, request *http.Request) {
		checkFeeDraft(t, request)

		attempt := fixture.writes.Add(1)
		if attempt > 2 {
			response.WriteHeader(http.StatusConflict)

			return
		}

		if attempt == 2 {
			response.Header().Set("Idempotent-Replayed", "true")
		}

		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(
			response,
			`{"id":"fixture-entry","status":"open","external_reference":"`+fixtureRef+`"}`,
		)
	}
}

func TestStripeFeeSyncDryRunAndReplay(t *testing.T) {
	t.Parallel()

	fixture := newStripeFeeFixture(t)
	args := feeReadArgs(t)

	assertFeeRun(t, fixture.client, args, 0, `"eligible":1`)

	if fixture.writes.Load() != 0 || fixture.reads.Load() != 2 {
		t.Fatal("dry run must read both pages without writing")
	}

	args = append(args, feeWriteArgs(t, fixtureCompany)...)
	assertFeeRun(t, fixture.client, args, 0, `"drafted":1`)
	assertFeeRun(t, fixture.client, args, 0, `"existing":1`)
	assertFeeRun(t, fixture.client, args, 0, `"existing":1`)

	if fixture.writes.Load() != 3 {
		t.Fatal("the test did not exercise both replay responses")
	}
}

func TestStripeFeeSyncRejectsWrongCompany(t *testing.T) {
	t.Parallel()

	fixture := newStripeFeeFixture(t)
	args := append(feeReadArgs(t), feeWriteArgs(t, "00000000-0000-4000-8000-000000000026")...)
	assertFeeRun(t, fixture.client, args, 1, "configured company")

	if fixture.writes.Load() != 0 {
		t.Fatal("a draft was sent to the wrong company")
	}
}

func assertFeeRun(t *testing.T, client *http.Client, args []string, expected int, message string) {
	t.Helper()

	var output strings.Builder

	exit := run(t.Context(), args, &output, client)
	if exit != expected || !strings.Contains(output.String(), message) {
		t.Fatalf(
			"wanted status %d and %q, got status %d: %s",
			expected,
			message,
			exit,
			output.String(),
		)
	}
}

func feeReadArgs(t *testing.T) []string {
	t.Helper()

	stripeKey := feeSecretFile(t, "stripe", "rk_"+"test_"+"fixture")

	return []string{
		"sync-stripe-fees",
		"--stripe-key-file",
		stripeKey,
		"--since",
		"2026-02-01",
		"--until",
		"2026-03-01",
	}
}

func feeWriteArgs(t *testing.T, company string) []string {
	t.Helper()

	return []string{
		"--write-drafts",
		"--numina-token-file",
		feeSecretFile(t, "numina", "fixture-access-token"),
		"--numina-company-id",
		company,
		"--stripe-balance-account",
		"6600",
		"--stripe-fee-account",
		"3330",
	}
}

func feeSecretFile(t *testing.T, name, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)

	err := os.WriteFile(path, []byte(token+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func checkFeeDraft(t *testing.T, request *http.Request) {
	t.Helper()

	var draft struct {
		Date      string `json:"date"`
		Reference string `json:"external_reference"` //nolint:tagliatelle // Numina's wire name.
		Lines     []struct {
			Account int         `json:"account_number"` //nolint:tagliatelle // Numina's wire name.
			Amount  json.Number `json:"amount"`
		} `json:"lines"`
	}

	err := json.NewDecoder(request.Body).Decode(&draft)
	if err != nil {
		t.Errorf("could not read the draft: %v", err)

		return
	}

	if draft.Date != "2026-02-02" || draft.Reference != fixtureRef ||
		request.Header.Get("Idempotency-Key") != fixtureRef {
		t.Error("draft did not carry the original date and stable identity")
	}

	if len(draft.Lines) != 2 || draft.Lines[0].Account != 3330 || draft.Lines[0].Amount != "3.59" ||
		draft.Lines[1].Account != 6600 || draft.Lines[1].Amount != "-3.59" {
		t.Error("draft did not debit fees and credit clearing for the exact amount")
	}
}
