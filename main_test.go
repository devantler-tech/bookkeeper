package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devantler-tech/bookkeeper/internal/jmaptest"
)

func mailServer(t *testing.T, moved *[]string) *httptest.Server {
	t.Helper()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/jmap/session":
			jmaptest.WriteJSON(writer, map[string]any{
				"apiUrl":          srv.URL + "/jmap",
				"downloadUrl":     srv.URL + "/dl/{accountId}/{blobId}/{name}",
				"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "a"},
				"accounts":        map[string]any{"a": map[string]any{"isReadOnly": false}},
			})
		case req.URL.Path == "/jmap":
			method, _ := jmaptest.Call(req)
			result := jmapResult(method, moved)

			jmaptest.WriteJSON(
				writer,
				map[string]any{"methodResponses": []any{[]any{method, result, "c"}}},
			)
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
	// Plain HTTP to a host that only starts like this machine, or a URL with no host.
	for _, bad := range []string{
		"http://localhost.attacker.example",
		"http://127.0.0.1.attacker.example",
		"http://localhost@attacker.example",
		"mail.example.com",
	} {
		cases["mail URL "+bad] = args(bad, "https://n", password, token)
		cases["Numina URL "+bad] = args("https://m", bad, password, token)
	}

	for name, cmdline := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// A refused run says so before it reaches any server.
			var out bytes.Buffer

			code := run(context.Background(), cmdline, &out, newHTTPClient())
			refused := strings.Contains(out.String(), "refused") ||
				strings.Contains(out.String(), "Usage")

			if code != 1 || !refused {
				t.Fatalf("exit %d\n%s", code, out.String())
			}
		})
	}
}

func TestCheckURLAllowsHTTPOnlyToThisMachine(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]error{
		"https://mail.example.com":          nil,
		"http://localhost:8080":             nil,
		"http://127.0.0.1:8080":             nil,
		"http://[::1]:8080":                 nil,
		"http://localhost.attacker.example": errURLNotHTTPS,
		"http://127.0.0.1.attacker.example": errURLNotHTTPS,
		"ftp://localhost":                   errURLNotHTTPS,
		"https://":                          errURLNoHost,
		"https://%zz":                       errURLNoHost,
	} {
		got := checkURL(raw)
		if !errors.Is(got, want) {
			t.Errorf("checkURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
