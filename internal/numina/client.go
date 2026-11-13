// Package numina uploads documents to a company's inbox in Numina (https://numina.app), a
// registered Danish bookkeeping system. A Client has no path that books, changes or deletes
// anything in the books.
package numina

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Numina's production API.
const DefaultBaseURL = "https://api.numina.app"

const (
	// maxAnswerBytes caps how much of an answer is read.
	maxAnswerBytes = 1 << 20
	// maxRateLimitRetries is how often a rate-limited upload is tried again.
	maxRateLimitRetries = 3
	// maxRetryAfterSeconds is the longest Retry-After wait honoured, in whole seconds.
	maxRetryAfterSeconds = 60
	// defaultRetryAfter is the wait when Retry-After is missing, invalid or too long.
	defaultRetryAfter = 5 * time.Second
)

var (
	errUnreachable = errors.New("numina could not be reached")
	errUnreadable  = errors.New("numina's answer could not be read")
	errRefused     = errors.New("numina refused the upload")
	errNoDocument  = errors.New("numina's answer named no document")
	errBadRequest  = errors.New("the upload request could not be built")
)

// UploadResult names the document Numina holds for an uploaded file.
type UploadResult struct {
	AttachmentID string
	Duplicate    bool // Numina already had this exact file
}

// Client calls Numina's REST API with one company's API key.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	sleep   func(time.Duration)
}

// answer is the part of one HTTP answer the upload needs.
type answer struct {
	status     int
	retryAfter string
	body       []byte
}

// New returns a Client for baseURL with the given API key.
func New(baseURL, token string, httpClient *http.Client) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    httpClient,
		sleep:   time.Sleep,
	}
}

// UploadToInbox sends one file to the company's inbox and asks Numina to read it (OCR). Numina
// answers a file it already has with the existing document, so a retry never creates a duplicate.
func (c *Client) UploadToInbox(
	ctx context.Context,
	fileName, contentType string,
	data []byte,
) (UploadResult, error) {
	body, formType, err := uploadForm(fileName, contentType, data)
	if err != nil {
		return UploadResult{}, err
	}

	for attempt := 0; ; attempt++ {
		got, err := c.post(ctx, body, formType)
		if err != nil {
			return UploadResult{}, err
		}

		if got.status == http.StatusTooManyRequests && attempt < maxRateLimitRetries {
			c.sleep(retryAfter(got.retryAfter))

			continue
		}

		return uploadResult(got)
	}
}

// post sends the upload form once and reads the answer.
func (c *Client) post(ctx context.Context, body []byte, formType string) (answer, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/v1/attachments",
		bytes.NewReader(body),
	)
	if err != nil {
		return answer{}, fmt.Errorf("%w: %w", errBadRequest, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", formType)

	resp, err := c.http.Do(req)
	if err != nil {
		return answer{}, errUnreachable
	}

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	_ = resp.Body.Close()

	if readErr != nil {
		return answer{}, errUnreadable
	}

	return answer{
		status:     resp.StatusCode,
		retryAfter: resp.Header.Get("Retry-After"),
		body:       data,
	}, nil
}

// uploadForm builds the multipart body: the file, and run_ocr=true.
func uploadForm(fileName, contentType string, data []byte) ([]byte, string, error) {
	var body bytes.Buffer

	form := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	header.Set("Content-Type", contentType)

	part, err := form.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", errBadRequest, err)
	}

	_, err = part.Write(data)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", errBadRequest, err)
	}

	err = form.WriteField("run_ocr", "true")
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", errBadRequest, err)
	}

	err = form.Close()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", errBadRequest, err)
	}

	return body.Bytes(), form.FormDataContentType(), nil
}

// uploadResult reads the document out of Numina's answer, or why Numina refused the upload.
func uploadResult(got answer) (UploadResult, error) {
	if got.status != http.StatusOK && got.status != http.StatusCreated {
		var refusal struct {
			Error string `json:"error"`
		}

		_ = json.Unmarshal(got.body, &refusal)

		return UploadResult{}, fmt.Errorf("%w with %d (%s)", errRefused, got.status, refusal.Error)
	}

	var uploaded struct {
		Attachment struct {
			ID string `json:"id"`
		} `json:"attachment"`
		Duplicate bool `json:"duplicate"`
	}

	err := json.Unmarshal(got.body, &uploaded)
	if err != nil || uploaded.Attachment.ID == "" {
		return UploadResult{}, errNoDocument
	}

	return UploadResult{
		AttachmentID: uploaded.Attachment.ID,
		Duplicate:    uploaded.Duplicate,
	}, nil
}

func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err == nil && seconds > 0 && seconds <= maxRetryAfterSeconds {
		return time.Duration(seconds) * time.Second
	}

	return defaultRetryAfter
}
