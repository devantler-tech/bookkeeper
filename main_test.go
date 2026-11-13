package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mailServer(t *testing.T, moved *[]string) *httptest.Server {
	t.Helper()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/jmap/session":
			writeJSON(writer, map[string]any{
				"apiUrl":          srv.URL + "/jmap",
				"downloadUrl":     srv.URL + "/dl/{accountId}/{blobId}/{name}",
				"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "a"},
				"accounts":        map[string]any{"a": map[string]any{"isReadOnly": false}},
			})
		case req.URL.Path == "/jmap":
			var call struct {
				MethodCalls [][]json.RawMessage `json:"methodCalls"`
			}

			_ = json.NewDecoder(req.Body).Decode(&call)

			var method string

			_ = json.Unmarshal(call.MethodCalls[0][0], &method)
			result := jmapResult(method, moved)

			writeJSON(writer, map[string]any{"methodResponses": []any{[]any{method, result, "c"}}})
		case strings.HasPrefix(req.URL.Path, "/dl/"):
			_, _ = io.WriteString(writer, "%PDF-1.7 abc")
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// jmapResult answers one JMAP method about one invoice mail, recording in moved each mail flagged.
func jmapResult(method string, moved *[]string) map[string]any {
	result := map[string]any{}

	switch method {
	case "Mailbox/get":
		result["list"] = []any{
			map[string]any{"id": "in", "name": "Inbox", "role": "inbox"},
		}
	case "Email/query":
		result["ids"] = []string{"e1"}
	case "Email/get":
		result["list"] = []any{
			map[string]any{
				"id":      "e1",
				"subject": "Invoice 42",
				"attachments": []any{
					map[string]any{
						"blobId": "b",
						"type":   "application/pdf",
						"name":   "invoice.pdf",
						"size":   12,
					},
				},
			},
		}
	case "Email/set":
		*moved = append(*moved, "e1")
		result["updated"] = map[string]any{"e1": nil}
	}

	return result
}

// writeJSON answers with value, or with a server error the client under test then reports.
func writeJSON(writer http.ResponseWriter, value any) {
	err := json.NewEncoder(writer).Encode(value)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func numinaServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, `{"attachment":{"id":"d1"},"duplicate":false}`)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func secret(t *testing.T, value string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")

	err := os.WriteFile(path, []byte(value), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func args(mailURL, numinaURL, password, token string, extra ...string) []string {
	return append([]string{
		"collect-vouchers",
		"--mail-url",
		mailURL,
		"--mail-user",
		"bilag",
		"--mail-password-file",
		password,
		"--numina-url",
		numinaURL,
		"--numina-token-file",
		token,
	}, extra...)
}

func TestCollectVouchersEndToEnd(t *testing.T) {
	t.Parallel()

	var moved []string

	mailSrv, numinaSrv := mailServer(t, &moved), numinaServer(t, http.StatusCreated)

	var out bytes.Buffer

	code := run(
		context.Background(),
		args(mailSrv.URL, numinaSrv.URL, secret(t, "pw with spaces\n"), secret(t, "numi_key\n")),
		&out,
		newHTTPClient(),
	)
	if code != 0 || len(moved) != 1 || !strings.Contains(out.String(), `"invoicesSent":1`) {
		t.Fatalf("exit %d moved %v\n%s", code, moved, out.String())
	}

	if strings.Contains(out.String(), "invoice.pdf") || strings.Contains(out.String(), "numi_key") {
		t.Fatal("logs must carry neither file names nor credentials")
	}
}

func TestCollectVouchersFailsWhenNuminaRefuses(t *testing.T) {
	t.Parallel()

	var moved []string

	mailSrv, numinaSrv := mailServer(t, &moved), numinaServer(t, http.StatusInternalServerError)

	var out bytes.Buffer
	if code := run(
		context.Background(),
		args(mailSrv.URL, numinaSrv.URL, secret(t, "pw"), secret(t, "k")),
		&out,
		newHTTPClient(),
	); code != 1 ||
		len(moved) != 0 {
		t.Fatalf("exit %d moved %v", code, moved)
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()

	password, token := secret(t, "pw"), secret(t, "k")

	cases := map[string][]string{
		"no command":      {},
		"unknown command": {"book"},
		"plain HTTP": args(
			"http://mail.example.com",
			"https://api.numina.app",
			password,
			token,
		),
		"no mail URL":      args("", "https://api.numina.app", password, token),
		"limit too high":   args("https://m", "https://n", password, token, "--limit", "501"),
		"two-line secret":  args("https://m", "https://n", secret(t, "a\nb\n"), token),
		"token with space": args("https://m", "https://n", password, secret(t, "a b")),
		"missing file":     args("https://m", "https://n", "/nonexistent", token),
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if code := run(context.Background(), a, &bytes.Buffer{}, newHTTPClient()); code != 1 {
				t.Fatalf("exit %d", code)
			}
		})
	}
}
