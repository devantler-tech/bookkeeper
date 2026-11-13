// Package mail reads mail over JMAP (RFC 8620 and RFC 8621). A Client can query and read mail,
// download attachments and set a keyword on a mail. It never moves, changes or deletes mail.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	capabilityCore = "urn:ietf:params:jmap:core"
	capabilityMail = "urn:ietf:params:jmap:mail"

	// maxSessionBytes caps the JMAP session document.
	maxSessionBytes = 1 << 20
	// maxAnswerBytes caps one JMAP method answer.
	maxAnswerBytes = 16 << 20
	// methodResponseParts is the length of a JMAP invocation: name, arguments and call ID.
	methodResponseParts = 3
)

var (
	errSessionInvalid = errors.New("the mail session is not valid JSON")
	errNoMailAccount  = errors.New("the mail session has no mail account")
	errReadOnly       = errors.New("the mail account is read-only, so mail cannot be filed")
	errForeignSession = errors.New("the mail session points outside the configured server")
	errNotAllowed     = errors.New("is not allowed")
	errCreateDestroy  = errors.New("refusing to create or destroy mail")
	errEncodeCall     = errors.New("could not be encoded")
	errUnexpected     = errors.New("returned an unexpected answer")
	errMethodFailed   = errors.New("failed")
	errNotFlagged     = errors.New("the mail server did not flag the mail")
	errBadRequest     = errors.New("the mail request could not be built")
	errUnreachable    = errors.New("the mail server could not be reached")
	errStatus         = errors.New("the mail server answered")
	errUnreadable     = errors.New("the mail server's answer could not be read")
	errTooLarge       = errors.New("the mail server's answer is too large")
)

// Mailbox is a JMAP mailbox, which a mail program shows as a folder.
type Mailbox struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ParentID *string `json:"parentId"`
	Role     *string `json:"role"`
}

// Part is one attachment of a mail.
type Part struct {
	BlobID string `json:"blobId"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
}

// Email is a mail with the properties this program reads.
type Email struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Attachments []Part `json:"attachments"`
}

// Client talks to one JMAP server as one user.
type Client struct {
	base        string
	user        string
	password    string
	http        *http.Client
	accountID   string
	apiURL      string
	downloadURL string
}

// New returns a Client for the JMAP server at base, for example https://mail.example.com.
// The HTTP client should refuse redirects, so the password cannot follow one.
func New(base, user, password string, httpClient *http.Client) *Client {
	return &Client{
		base:     strings.TrimRight(base, "/"),
		user:     user,
		password: password,
		http:     httpClient,
		// Connect fills these in from the JMAP session.
		accountID:   "",
		apiURL:      "",
		downloadURL: "",
	}
}

// Connect reads the JMAP session and selects the user's mail account.
func (c *Client) Connect(ctx context.Context) error {
	data, err := c.send(ctx, http.MethodGet, c.base+"/jmap/session", "", nil, maxSessionBytes)
	if err != nil {
		return err
	}

	var session struct {
		APIURL          string            `json:"apiUrl"`
		DownloadURL     string            `json:"downloadUrl"`
		PrimaryAccounts map[string]string `json:"primaryAccounts"`
		Accounts        map[string]struct {
			IsReadOnly bool `json:"isReadOnly"`
		} `json:"accounts"`
	}

	err = json.Unmarshal(data, &session)
	if err != nil {
		return errSessionInvalid
	}

	account := session.PrimaryAccounts[capabilityMail]

	entry, ok := session.Accounts[account]
	if account == "" || !ok {
		return errNoMailAccount
	}

	if entry.IsReadOnly {
		return errReadOnly
	}

	if !sameOrigin(c.base, session.APIURL) || !sameOrigin(c.base, session.DownloadURL) {
		return errForeignSession
	}

	c.accountID, c.apiURL, c.downloadURL = account, session.APIURL, session.DownloadURL

	return nil
}

// Mailboxes lists every mailbox of the account.
func (c *Client) Mailboxes(ctx context.Context) ([]Mailbox, error) {
	var out struct {
		List []Mailbox `json:"list"`
	}

	err := c.call(
		ctx,
		"Mailbox/get",
		map[string]any{"ids": nil, "properties": []string{"id", "name", "parentId", "role"}},
		&out,
	)

	return out.List, err
}

// Query returns the IDs of the oldest mails in a mailbox that have attachments and do not carry
// skipKeyword yet, at most limit of them.
func (c *Client) Query(
	ctx context.Context,
	mailboxID, skipKeyword string,
	limit int,
) ([]string, error) {
	var out struct {
		IDs []string `json:"ids"`
	}

	err := c.call(ctx, "Email/query", map[string]any{
		"filter": map[string]any{
			"inMailbox":     mailboxID,
			"hasAttachment": true,
			"notKeyword":    skipKeyword,
		},
		"sort":  []any{map[string]any{"property": "receivedAt", "isAscending": true}},
		"limit": limit,
	}, &out)

	return out.IDs, err
}

// Flag sets a keyword on one mail. Nothing else about the mail changes.
func (c *Client) Flag(ctx context.Context, emailID, keyword string) error {
	var out struct {
		Updated map[string]any `json:"updated"`
	}

	err := c.call(
		ctx,
		"Email/set",
		map[string]any{
			"update": map[string]any{emailID: map[string]any{"keywords/" + keyword: true}},
		},
		&out,
	)
	if err != nil {
		return err
	}

	if _, ok := out.Updated[emailID]; !ok {
		return errNotFlagged
	}

	return nil
}

// Emails reads the subject and attachments of the given mails.
func (c *Client) Emails(ctx context.Context, ids []string) ([]Email, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var out struct {
		List []Email `json:"list"`
	}

	err := c.call(ctx, "Email/get", map[string]any{
		"ids":            ids,
		"properties":     []string{"id", "subject", "attachments"},
		"bodyProperties": []string{"blobId", "type", "name", "size"},
	}, &out)

	return out.List, err
}

// Download returns an attachment's bytes, refusing one larger than limit.
func (c *Client) Download(ctx context.Context, part Part, limit int64) ([]byte, error) {
	target := strings.NewReplacer(
		"{accountId}", url.PathEscape(c.accountID),
		"{blobId}", url.PathEscape(part.BlobID),
		"{name}", url.PathEscape(part.Name),
		"{type}", url.QueryEscape(part.Type),
	).Replace(c.downloadURL)

	return c.send(ctx, http.MethodGet, target, "", nil, limit)
}

// call runs one JMAP method on the selected account and decodes its arguments into out.
func (c *Client) call(ctx context.Context, method string, args map[string]any, out any) error {
	err := checkCall(method, args)
	if err != nil {
		return err
	}

	args["accountId"] = c.accountID

	body, err := json.Marshal(map[string]any{
		"using":       []string{capabilityCore, capabilityMail},
		"methodCalls": []any{[]any{method, args, "c"}},
	})
	if err != nil {
		return fmt.Errorf("JMAP %s %w: %w", method, errEncodeCall, err)
	}

	data, err := c.send(ctx, http.MethodPost, c.apiURL, "application/json", body, maxAnswerBytes)
	if err != nil {
		return err
	}

	return decodeAnswer(method, data, out)
}

func (c *Client) send(
	ctx context.Context,
	method, target, contentType string,
	body []byte,
	limit int64,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		// The URL names the attachment file, so it never reaches the log.
		return nil, errBadRequest
	}

	req.SetBasicAuth(c.user, c.password)

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errUnreachable
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w %d", errStatus, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errUnreadable
	}

	if int64(len(data)) > limit {
		return nil, errTooLarge
	}

	return data, nil
}

// sameOrigin keeps the password on the configured server even if a session names another host.
func sameOrigin(base, target string) bool {
	baseURL, baseErr := url.Parse(base)
	targetURL, targetErr := url.Parse(
		strings.NewReplacer("{accountId}", "a", "{blobId}", "b", "{name}", "n", "{type}", "t").
			Replace(target),
	)

	return baseErr == nil && targetErr == nil &&
		targetURL.Scheme == baseURL.Scheme && targetURL.Host == baseURL.Host
}

// allowedMethod names the only JMAP methods a Client calls: reads, and Email/set for flags.
func allowedMethod(method string) bool {
	switch method {
	case "Mailbox/get", "Email/query", "Email/get", "Email/set":
		return true
	default:
		return false
	}
}

// checkCall refuses any method outside allowedMethod and any create or destroy argument.
func checkCall(method string, args map[string]any) error {
	if !allowedMethod(method) {
		return fmt.Errorf("JMAP method %s %w", method, errNotAllowed)
	}

	for _, key := range []string{"create", "destroy"} {
		if _, ok := args[key]; ok {
			return errCreateDestroy
		}
	}

	return nil
}

// decodeAnswer checks a JMAP answer holds exactly one response to method and decodes it into out.
func decodeAnswer(method string, data []byte, out any) error {
	var envelope struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}

	err := json.Unmarshal(data, &envelope)
	if err != nil || len(envelope.MethodResponses) != 1 ||
		len(envelope.MethodResponses[0]) != methodResponseParts {
		return fmt.Errorf("JMAP %s %w", method, errUnexpected)
	}

	var name string

	err = json.Unmarshal(envelope.MethodResponses[0][0], &name)
	if err != nil || name != method {
		return fmt.Errorf("JMAP %s %w", method, errMethodFailed)
	}

	err = json.Unmarshal(envelope.MethodResponses[0][1], out)
	if err != nil {
		return fmt.Errorf("JMAP %s %w: %w", method, errUnexpected, err)
	}

	return nil
}
