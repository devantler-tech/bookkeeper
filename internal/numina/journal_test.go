package numina_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devantler-tech/bookkeeper/internal/numina"
)

func TestStripeDraftConfirmsChangedReplay(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name, code, reference string
		wantLookups           int32
		wantExisting          bool
	}{
		{"same reference", "idempotency_key_reused", "bookkeeper:stripe-fee:txn_fixture", 1, true},
		{"different reference", "idempotency_key_reused", "bookkeeper:stripe-fee:txn_other", 1, false},
		{"unrelated validation error", "validation_error", "", 0, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()

			var lookups atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(
				func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodPost {
						writer.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = fmt.Fprintf(writer, `{"error":%q}`, scenario.code)

						return
					}

					lookups.Add(1)

					reference := request.URL.Query().Get("external_reference")
					if request.Method != http.MethodGet || reference != fixtureReference {
						t.Error("lookup did not target the original reference")
					}

					_, _ = fmt.Fprintf(
						writer,
						`{"journal_entries":[{"id":"existing","external_reference":%q}],"has_more":false}`,
						scenario.reference,
					)
				},
			))
			defer server.Close()

			client := numina.New(server.URL, "fixture-only", server.Client())

			existing, err := client.CreateStripeFeeDraft(t.Context(), journalFee())
			if existing != scenario.wantExisting || (err == nil) != scenario.wantExisting ||
				lookups.Load() != scenario.wantLookups {
				t.Errorf("existing=%v error=%v lookups=%d", existing, err, lookups.Load())
			}
		})
	}
}

func TestStripeDraftRetriesIdenticalRateLimitedRequest(t *testing.T) {
	t.Parallel()

	sent := make(chan string, 2)

	var attempts atomic.Int32

	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
			}

			sent <- request.Header.Get("Idempotency-Key") + "|" + string(body)

			if attempts.Add(1) == 1 {
				writer.Header().Set("Retry-After", "2")
				writer.WriteHeader(http.StatusTooManyRequests)

				return
			}

			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(
				writer,
				`{"id":"created","external_reference":"bookkeeper:stripe-fee:txn_fixture","status":"open"}`,
			)
		}),
	)
	defer server.Close()

	client := numina.New(server.URL, "fixture-only", server.Client())

	var waits []time.Duration

	client.SetSleep(func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)

		return nil
	})

	existing, err := client.CreateStripeFeeDraft(t.Context(), journalFee())
	if err != nil || existing || attempts.Load() != 2 || len(waits) != 1 ||
		waits[0] != 2*time.Second {
		t.Fatalf("existing=%v error=%v attempts=%d waits=%v", existing, err, attempts.Load(), waits)
	}

	if first, second := <-sent, <-sent; first != second {
		t.Fatal("retry changed the idempotency key or body")
	}
}

const fixtureReference = "bookkeeper:stripe-fee:txn_fixture"

func journalFee() numina.StripeFeeDraft {
	return numina.StripeFeeDraft{
		TransactionID:  "txn_fixture",
		Date:           "2026-11-22",
		Amount:         100,
		BalanceAccount: 5810,
		FeeAccount:     3330,
	}
}
