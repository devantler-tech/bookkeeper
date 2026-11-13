package mail_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devantler-tech/bookkeeper/internal/jmaptest"
	"github.com/devantler-tech/bookkeeper/internal/mail"
)

const (
	testUser     = "bilag@example.com"
	testPassword = "secret"
	testAccount  = "acc1"
)

type fakeServer struct {
	*httptest.Server

	readOnly        bool
	apiHost         string
	maxObjectsInGet any // the session's core maxObjectsInGet; nil sends null
	calls           []string
	lastArgs        map[string]any
	downloads       []string
}

func newServer(t *testing.T) *fakeServer {
	t.Helper()

	fake := &fakeServer{
		Server:          nil, // started below, once its handler can refer to fake
		readOnly:        false,
		apiHost:         "",
		maxObjectsInGet: nil,
		calls:           nil,
		lastArgs:        nil,
		downloads:       nil,
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)

	return fake
}

func (f *fakeServer) serve(writer http.ResponseWriter, req *http.Request) {
	if !authorized(req) {
		writer.WriteHeader(http.StatusUnauthorized)

		return
	}

	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/jmap/session":
		f.session(writer)
	case req.Method == http.MethodPost && req.URL.Path == "/jmap":
		f.jmap(writer, req)
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/jmap/download/"):
		f.downloads = append(f.downloads, req.URL.String())
		_, _ = io.WriteString(writer, "%PDF-1.7 content")
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func authorized(req *http.Request) bool {
	user, pass, ok := req.BasicAuth()

	return ok && user == testUser && pass == testPassword
}

func (f *fakeServer) session(writer http.ResponseWriter) {
	host := f.apiHost
	if host == "" {
		host = f.URL
	}

	jmaptest.WriteJSON(writer, map[string]any{
		"capabilities": map[string]any{
			"urn:ietf:params:jmap:core": map[string]any{"maxObjectsInGet": f.maxObjectsInGet},
		},
		"apiUrl":          host + "/jmap",
		"downloadUrl":     f.URL + "/jmap/download/{accountId}/{blobId}/{name}?accept={type}",
		"primaryAccounts": map[string]string{mail.CapabilityMail: testAccount},
		"accounts":        map[string]any{testAccount: map[string]any{"isReadOnly": f.readOnly}},
	})
}

func (f *fakeServer) jmap(writer http.ResponseWriter, req *http.Request) {
	method, args := jmaptest.Call(req)

	f.calls, f.lastArgs = append(f.calls, method), args
	result := map[string]any{"accountId": args["accountId"]}

	switch method {
	case "Mailbox/get":
		result["list"] = []any{
			map[string]any{"id": "in", "name": "Inbox", "role": "inbox", "parentId": nil},
		}
	case "Email/query":
		result["ids"] = []string{"e1", "e2"}
	case "Email/get":
		result["list"] = []any{
			map[string]any{
				"id": "e1",
				"attachments": []any{
					map[string]any{
						"blobId": "b1",
						"type":   "application/pdf",
						"name":   "a b.pdf",
						"size":   9,
					},
				},
			},
		}
	case "Email/set":
		result["updated"] = map[string]any{"e1": nil}
	}

	jmaptest.WriteJSON(writer, map[string]any{"methodResponses": []any{[]any{method, result, "c"}}})
}

func connected(t *testing.T, fake *fakeServer) *mail.Client {
	t.Helper()

	client := mail.New(fake.URL, testUser, testPassword, fake.Client())

	err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	return client
}

func TestConnectSelectsTheMailAccount(t *testing.T) {
	t.Parallel()

	client := connected(t, newServer(t))
	if client.AccountID() != testAccount {
		t.Fatalf("account %q", client.AccountID())
	}
}

func TestConnectRefusesUnsafeSessions(t *testing.T) {
	t.Parallel()

	fake := newServer(t)
	fake.readOnly = true

	err := mail.New(fake.URL, testUser, testPassword, fake.Client()).
		Connect(context.Background())
	if err == nil {
		t.Fatal("a read-only account cannot file mail")
	}

	fake.readOnly, fake.apiHost = false, "https://elsewhere.example"

	err = mail.New(fake.URL, testUser, testPassword, fake.Client()).Connect(context.Background())
	if err == nil {
		t.Fatal("the password must never go to another host")
	}

	err = mail.New(fake.URL, testUser, "wrong", fake.Client()).Connect(context.Background())
	if err == nil {
		t.Fatal("a refused sign-in must fail")
	}
}

func TestCallRefusesWritesOtherThanFlags(t *testing.T) {
	t.Parallel()

	fake := newServer(t)

	client := connected(t, fake)
	for _, args := range []map[string]any{{"destroy": []string{"e1"}}, {"create": map[string]any{"x": map[string]any{}}}} {
		err := client.Call(context.Background(), "Email/set", args, &struct{}{})
		if err == nil {
			t.Fatalf("%v must be refused", args)
		}
	}

	for _, method := range []string{"Email/import", "Mailbox/set"} {
		err := client.Call(context.Background(), method, map[string]any{}, &struct{}{})
		if err == nil {
			t.Fatalf("%s must be refused", method)
		}
	}

	if len(fake.calls) != 0 {
		t.Fatalf("refused calls reached the server: %v", fake.calls)
	}
}

func TestReadAndFlag(t *testing.T) {
	t.Parallel()

	fake := newServer(t)
	client := connected(t, fake)
	ctx := context.Background()

	checkMailboxes(ctx, t, client)
	checkQuery(ctx, t, fake, client)
	emails := checkEmails(ctx, t, client)
	checkFlag(ctx, t, fake, client)
	checkDownload(ctx, t, fake, client, emails[0].Attachments[0])
}

func checkMailboxes(ctx context.Context, t *testing.T, client *mail.Client) {
	t.Helper()

	boxes, err := client.Mailboxes(ctx)
	if err != nil || len(boxes) != 1 || *boxes[0].Role != "inbox" {
		t.Fatalf("Mailboxes: %v %v", boxes, err)
	}
}

func checkQuery(ctx context.Context, t *testing.T, fake *fakeServer, client *mail.Client) {
	t.Helper()

	ids, err := client.Query(ctx, "in", []string{"$bk-sent", "$bk-skip"}, 50)
	if err != nil || len(ids) != 2 || fake.lastArgs["limit"] != float64(50) {
		t.Fatalf("Query: %v %v %v", ids, err, fake.lastArgs)
	}

	// json.Marshal sorts map keys, so the filter has one exact form.
	filter, err := json.Marshal(fake.lastArgs["filter"])
	if err != nil || string(filter) != `{"conditions":[{"hasAttachment":true,"inMailbox":"in"},`+
		`{"notKeyword":"$bk-sent"},{"notKeyword":"$bk-skip"}],"operator":"AND"}` {
		t.Fatalf("Query filter %s", filter)
	}
}

func TestQueryNeverAsksForMoreThanOneEmailGetReads(t *testing.T) {
	t.Parallel()

	// The server's maxObjectsInGet caps the query; a missing or unusable value leaves it as asked.
	for limit, wants := range map[any]float64{
		2: 2, 500: 50, nil: 50, 0: 50, -1: 50, 2.5: 50, "2": 50,
	} {
		fake := newServer(t)
		fake.maxObjectsInGet = limit

		_, err := connected(t, fake).Query(context.Background(), "in", nil, 50)
		if err != nil || fake.lastArgs["limit"] != wants {
			t.Errorf("maxObjectsInGet %v: %v, want limit %v (%v)", limit, fake.lastArgs, wants, err)
		}
	}
}

func checkEmails(ctx context.Context, t *testing.T, client *mail.Client) []mail.Email {
	t.Helper()

	emails, err := client.Emails(ctx, []string{"e1"})
	if err != nil || len(emails) != 1 || emails[0].Attachments[0].BlobID != "b1" {
		t.Fatalf("Emails: %v %v", emails, err)
	}

	return emails
}

func checkFlag(ctx context.Context, t *testing.T, fake *fakeServer, client *mail.Client) {
	t.Helper()

	err := client.Flag(ctx, "e1", "$bk-sent")
	if err != nil {
		t.Fatalf("Flag: %v", err)
	}

	update, hasUpdate := fake.lastArgs["update"].(map[string]any)
	if !hasUpdate {
		t.Fatalf("flag call without an update: %v", fake.lastArgs)
	}

	patch, isPatch := update["e1"].(map[string]any)
	if !isPatch || patch["keywords/$bk-sent"] != true || len(patch) != 1 {
		t.Fatalf("flag patch %v", update["e1"])
	}
}

func checkDownload(
	ctx context.Context,
	t *testing.T,
	fake *fakeServer,
	client *mail.Client,
	part mail.Part,
) {
	t.Helper()

	data, err := client.Download(ctx, part, 1<<20)
	if err != nil || !strings.HasPrefix(string(data), "%PDF") {
		t.Fatalf("Download: %q %v", data, err)
	}

	if got := fake.downloads[0]; got != "/jmap/download/acc1/b1/a%20b.pdf?accept=application%2Fpdf" {
		t.Fatalf("download URL %q", got)
	}

	_, err = client.Download(ctx, part, 4)
	if err == nil {
		t.Fatal("an attachment over the limit must be refused")
	}
}
