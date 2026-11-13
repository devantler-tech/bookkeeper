package numina_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devantler-tech/bookkeeper/internal/numina"
)

// uploadRequest is what a fake Numina saw of one upload.
type uploadRequest struct {
	auth        string
	file        string
	contentType string
	ocr         string
}

// record answers an upload like Numina does and keeps what it saw in got.
func record(got *uploadRequest) http.HandlerFunc {
	return func(writer http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/attachments" {
			writer.WriteHeader(http.StatusNotFound)

			return
		}

		got.auth = req.Header.Get("Authorization")

		file, header, err := req.FormFile("file")
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)

			return
		}

		data, _ := io.ReadAll(file)
		got.file = header.Filename + "=" + string(data)
		got.contentType = header.Header.Get("Content-Type")
		got.ocr = req.FormValue("run_ocr")

		writer.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(writer, `{"attachment":{"id":"doc-7"},"duplicate":false}`)
	}
}

func TestUploadToInbox(t *testing.T) {
	t.Parallel()

	var seen uploadRequest

	srv := httptest.NewServer(record(&seen))
	defer srv.Close()

	got, err := numina.New(
		srv.URL,
		"numi_key",
		srv.Client(),
	).UploadToInbox(context.Background(), "invoice.pdf", "application/pdf", []byte("%PDF-x"))
	if err != nil || got.AttachmentID != "doc-7" || got.Duplicate {
		t.Fatalf("got %+v, %v", got, err)
	}

	if seen.auth != "Bearer numi_key" || seen.file != "invoice.pdf=%PDF-x" ||
		seen.contentType != "application/pdf" ||
		seen.ocr != "true" {
		t.Fatalf(
			"request: auth %q file %q type %q ocr %q",
			seen.auth,
			seen.file,
			seen.contentType,
			seen.ocr,
		)
	}
}

func TestUploadRetriesRateLimitAndReportsDuplicates(t *testing.T) {
	t.Parallel()

	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writer.Header().Set("Retry-After", "2")
			writer.WriteHeader(http.StatusTooManyRequests)

			return
		}

		_, _ = io.WriteString(writer, `{"attachment":{"id":"doc-1"},"duplicate":true}`)
	}))
	defer srv.Close()

	client := numina.New(srv.URL, "k", srv.Client())

	var slept time.Duration

	client.SetSleep(func(_ context.Context, d time.Duration) error {
		slept += d

		return nil
	})

	got, err := client.UploadToInbox(
		context.Background(),
		"a.pdf",
		"application/pdf",
		[]byte("%PDF-"),
	)
	if err != nil || !got.Duplicate || calls != 2 || slept != 2*time.Second {
		t.Fatalf("got %+v err %v calls %d slept %v", got, err, calls, slept)
	}
}

// roundTripFunc answers requests in-process, so a test controls exactly when its context ends.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCancelledContextEndsTheRateLimitWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	requests := 0
	// Numina asks for the longest wait honoured, and the run is cancelled as the answer arrives.
	rateLimited := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++

		cancel()

		answer := httptest.NewRecorder()
		answer.Header().Set("Retry-After", "60")
		answer.WriteHeader(http.StatusTooManyRequests)

		return answer.Result(), nil
	})
	httpClient := &http.Client{
		Transport:     rateLimited,
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}

	start := time.Now()

	_, err := numina.New("https://numina.example", "k", httpClient).
		UploadToInbox(ctx, "a.pdf", "application/pdf", []byte("%PDF-"))
	if !errors.Is(err, context.Canceled) || requests != 1 || time.Since(start) > 10*time.Second {
		t.Fatalf("err %v after %d requests and %v", err, requests, time.Since(start))
	}

	err = numina.Sleep(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatalf("a wait that runs out must not fail: %v", err)
	}
}

func TestUploadRefusals(t *testing.T) {
	t.Parallel()

	for name, answer := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"refused":  {http.StatusBadRequest, `{"error":"invalid_file"}`, "HTTP status 400"},
		"no scope": {http.StatusForbidden, `{"error":"insufficient_scope"}`, "HTTP status 403"},
		"no id":    {http.StatusCreated, `{"attachment":{}}`, "named no document"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(
				http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.WriteHeader(answer.status)
					_, _ = io.WriteString(writer, answer.body)
				}),
			)
			defer srv.Close()

			_, err := numina.New(
				srv.URL,
				"k",
				srv.Client(),
			).UploadToInbox(context.Background(), "a.pdf", "application/pdf", []byte("%PDF-"))
			if err == nil || !strings.Contains(err.Error(), answer.want) {
				t.Fatalf("error %v, want one saying %q", err, answer.want)
			}

			// Numina's free-text message never reaches the error, which the run logs.
			if strings.Contains(err.Error(), "invalid_file") ||
				strings.Contains(err.Error(), "insufficient_scope") {
				t.Fatalf("error %q carries Numina's own message", err)
			}
		})
	}
}

func TestRetryAfterKeepsTheServerDelay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	httpDate := func(after time.Duration) string { return now.Add(after).Format(http.TimeFormat) }

	for header, want := range map[string]struct {
		wait  time.Duration
		retry bool
	}{
		"2":                        {2 * time.Second, true},
		"60":                       {time.Minute, true},
		"120":                      {0, false},
		httpDate(30 * time.Second): {30 * time.Second, true},
		httpDate(2 * time.Minute):  {0, false},
		httpDate(-time.Minute):     {0, true},
		"":                         {5 * time.Second, true},
		"soon":                     {5 * time.Second, true},
		"-3":                       {5 * time.Second, true},
	} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()

			wait, retry := numina.RetryAfter(header, now)
			if wait != want.wait || retry != want.retry {
				t.Fatalf(
					"Retry-After %q: got %v %v, want %v %v",
					header,
					wait,
					retry,
					want.wait,
					want.retry,
				)
			}
		})
	}
}

func TestLongRetryAfterStopsInsteadOfRetryingEarly(t *testing.T) {
	t.Parallel()

	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls++

		writer.Header().Set("Retry-After", "120")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := numina.New(srv.URL, "k", srv.Client())
	client.SetSleep(func(context.Context, time.Duration) error {
		t.Error("a wait longer than one run honours must not be shortened")

		return nil
	})

	_, err := client.UploadToInbox(
		context.Background(),
		"a.pdf",
		"application/pdf",
		[]byte("%PDF-"),
	)
	if err == nil || !strings.Contains(err.Error(), "429") || calls != 1 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}
