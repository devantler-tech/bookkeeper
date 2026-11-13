package numina_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

	client.SetSleep(func(d time.Duration) { slept += d })

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

func TestUploadRefusals(t *testing.T) {
	t.Parallel()

	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"refused":  {http.StatusBadRequest, `{"error":"invalid_file"}`},
		"no scope": {http.StatusForbidden, `{"error":"insufficient_scope"}`},
		"no id":    {http.StatusCreated, `{"attachment":{}}`},
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
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
