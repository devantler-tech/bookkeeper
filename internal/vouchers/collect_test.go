package vouchers_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devantler-tech/bookkeeper/internal/mail"
	"github.com/devantler-tech/bookkeeper/internal/numina"
	"github.com/devantler-tech/bookkeeper/internal/vouchers"
)

const invoiceSubject = "Invoice"

var (
	errDownloadFailed = errors.New("download failed")
	errTooLarge       = errors.New("too large")
	errRefused        = errors.New("refused")
	errFlagFailed     = errors.New("flag failed")
)

// pdfData is a small but real-looking PDF.
func pdfData() []byte {
	return append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 100)...)
}

// firstDay is the first day of the books the tests read from.
func firstDay() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

// pngData is a PNG large enough to be a receipt photo rather than a logo.
func pngData() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), 20<<10)...)
}

// fakeBox is a mailbox whose emails are oldest first. It answers a query like a JMAP server: at
// most limit mails, none flagged with a skipped keyword.
type fakeBox struct {
	folders   []mail.Mailbox
	emails    []mail.Email
	blobs     map[string][]byte
	failBlob  string
	failFlag  bool
	flagged   map[string]string
	queriedIn string
	since     time.Time
	skipped   []string
}

func newBox(emails ...mail.Email) *fakeBox {
	role := "inbox"

	return &fakeBox{
		folders:   []mail.Mailbox{{ID: "in", Name: "Inbox", ParentID: nil, Role: &role}},
		emails:    emails,
		blobs:     map[string][]byte{},
		failBlob:  "",
		failFlag:  false,
		flagged:   map[string]string{},
		queriedIn: "",
		since:     time.Time{},
		skipped:   nil,
	}
}

func (b *fakeBox) Connect(context.Context) error                     { return nil }
func (b *fakeBox) Mailboxes(context.Context) ([]mail.Mailbox, error) { return b.folders, nil }
func (b *fakeBox) Query(
	_ context.Context,
	mailboxID string,
	since time.Time,
	skip []string,
	limit int,
) ([]string, error) {
	b.queriedIn, b.since, b.skipped = mailboxID, since, skip

	ids := make([]string, 0, len(b.emails))
	for _, e := range b.emails {
		if len(ids) < limit && !slices.Contains(skip, b.flagged[e.ID]) {
			ids = append(ids, e.ID)
		}
	}

	return ids, nil
}

func (b *fakeBox) Emails(_ context.Context, ids []string) ([]mail.Email, error) {
	found := make([]mail.Email, 0, len(ids))
	for _, e := range b.emails {
		if slices.Contains(ids, e.ID) {
			found = append(found, e)
		}
	}

	return found, nil
}

func (b *fakeBox) Flag(_ context.Context, id, keyword string) error {
	if b.failFlag {
		return errFlagFailed
	}

	b.flagged[id] = keyword

	return nil
}

func (b *fakeBox) Download(_ context.Context, part mail.Part, limit int64) ([]byte, error) {
	if part.BlobID == b.failBlob {
		return nil, errDownloadFailed
	}

	data := b.blobs[part.BlobID]
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}

	return data, nil
}

type fakeInbox struct {
	uploads   []string
	fail      bool
	duplicate bool
}

func newInbox() *fakeInbox {
	return &fakeInbox{uploads: nil, fail: false, duplicate: false}
}

func (i *fakeInbox) UploadToInbox(
	_ context.Context,
	name, contentType string,
	_ []byte,
) (numina.UploadResult, error) {
	if i.fail {
		return numina.UploadResult{}, errRefused
	}

	i.uploads = append(i.uploads, name+"|"+contentType)

	return numina.UploadResult{AttachmentID: "doc-1", Duplicate: i.duplicate}, nil
}

func part(blob, contentType, name string, size int) mail.Part {
	return mail.Part{BlobID: blob, Type: contentType, Name: name, Size: int64(size)}
}

func collect(
	t *testing.T,
	box *fakeBox,
	inbox *fakeInbox,
	folder string,
) (vouchers.Summary, string) {
	t.Helper()

	opt := vouchers.Options{Folder: folder, Limit: 100, Since: firstDay()}

	return collectWith(t, box, inbox, opt)
}

func collectWith(
	t *testing.T,
	box *fakeBox,
	inbox *fakeInbox,
	opt vouchers.Options,
) (vouchers.Summary, string) {
	t.Helper()

	var logs bytes.Buffer

	sum, err := vouchers.Collect(
		context.Background(),
		box,
		inbox,
		opt,
		slog.New(slog.NewJSONHandler(&logs, nil)),
	)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	return sum, logs.String()
}

// checkLogsArePrivate fails when the logs carry any of the given mail details.
func checkLogsArePrivate(t *testing.T, logs string, details ...string) {
	t.Helper()

	for _, detail := range details {
		if strings.Contains(logs, detail) {
			t.Fatalf("logs carry %q: %s", detail, logs)
		}
	}
}

func TestInvoiceIsUploadedAndFlagged(t *testing.T) {
	t.Parallel()

	pdf := pdfData()
	box := newBox(
		mail.Email{
			ID:          "m1",
			Subject:     "Your receipt from Example",
			Attachments: []mail.Part{part("b1", "application/pdf", "Receipt-42.pdf", len(pdf))},
		},
	)
	box.blobs["b1"] = pdf
	inbox := newInbox()

	sum, _ := collect(t, box, inbox, "")
	if sum.Uploaded != 1 || sum.Invoices != 1 || sum.Failed != 0 {
		t.Fatalf("summary %+v", sum)
	}

	if strings.Join(inbox.uploads, ",") != "Receipt-42.pdf|application/pdf" ||
		box.flagged["m1"] != vouchers.SentKeyword {
		t.Fatalf("uploads %v flagged %v", inbox.uploads, box.flagged)
	}

	if box.queriedIn != "in" || !box.since.Equal(firstDay()) ||
		!slices.Equal(box.skipped, []string{vouchers.SentKeyword, vouchers.NotInvoiceKeyword}) {
		t.Fatalf("queried %q since %v skipping %q", box.queriedIn, box.since, box.skipped)
	}
}

func TestInvoiceWordInFileNameIsEnough(t *testing.T) {
	t.Parallel()

	pdf := pdfData()
	box := newBox(
		mail.Email{
			ID:      "m1",
			Subject: "Oktober",
			Attachments: []mail.Part{
				part("b1", "application/pdf", "Faktura 2026-10.pdf", len(pdf)),
			},
		},
	)
	box.blobs["b1"] = pdf

	inbox := newInbox()
	if sum, _ := collect(t, box, inbox, ""); sum.Uploaded != 1 {
		t.Fatalf("summary %+v", sum)
	}
}

func TestMailWithoutVouchersIsMarkedNotAnInvoice(t *testing.T) {
	t.Parallel()

	pdf := pdfData()
	box := newBox(
		mail.Email{
			ID:          "contract",
			Subject:     "Signed contract",
			Attachments: []mail.Part{part("b1", "application/pdf", "contract.pdf", len(pdf))},
		},
		mail.Email{
			ID:          "logo",
			Subject:     "Your receipt",
			Attachments: []mail.Part{part("b2", "image/png", "logo.png", 2000)},
		},
		mail.Email{
			ID:          "fake",
			Subject:     invoiceSubject,
			Attachments: []mail.Part{part("b3", "application/pdf", "x.pdf", 10)},
		},
		mail.Email{
			ID:          "docx",
			Subject:     invoiceSubject,
			Attachments: []mail.Part{part("b4", "application/msword", "a.doc", 500)},
		},
	)
	box.blobs["b1"], box.blobs["b3"] = pdf, []byte("<html>not a pdf")
	inbox := newInbox()

	sum, _ := collect(t, box, inbox, "")
	if len(inbox.uploads) != 0 || sum.Mails != 4 || sum.Invoices != 0 || sum.Failed != 0 {
		t.Fatalf("summary %+v uploads %v", sum, inbox.uploads)
	}

	for _, id := range []string{"contract", "logo", "fake", "docx"} {
		if box.flagged[id] != vouchers.NotInvoiceKeyword {
			t.Errorf("mail %s flagged %q, want %q", id, box.flagged[id], vouchers.NotInvoiceKeyword)
		}
	}
}

func TestNewerInvoiceIsReachedPastMoreThanLimitOtherMails(t *testing.T) {
	t.Parallel()

	const limit = 3

	pdf := pdfData()
	emails := make([]mail.Email, 0, limit+2)

	for i := range limit + 1 {
		emails = append(emails, mail.Email{
			ID:          "newsletter-" + strconv.Itoa(i),
			Subject:     "Newsletter",
			Attachments: []mail.Part{part("news", "application/pdf", "news.pdf", len(pdf))},
		})
	}

	box := newBox(append(emails, mail.Email{
		ID:          "invoice",
		Subject:     invoiceSubject,
		Attachments: []mail.Part{part("b1", "application/pdf", "a.pdf", len(pdf))},
	})...)
	box.blobs["news"], box.blobs["b1"] = pdf, pdf
	inbox := newInbox()
	opt := vouchers.Options{Folder: "", Limit: limit, Since: firstDay()}

	// The first run fills its window with other mail; the second must still reach the invoice.
	first, _ := collectWith(t, box, inbox, opt)
	second, _ := collectWith(t, box, inbox, opt)

	if first.Mails != limit || first.Invoices != 0 || second.Mails != 2 || second.Invoices != 1 ||
		box.flagged["invoice"] != vouchers.SentKeyword ||
		strings.Join(inbox.uploads, ",") != "a.pdf|application/pdf" {
		t.Fatalf("runs %+v, %+v uploads %v flagged %v", first, second, inbox.uploads, box.flagged)
	}
}

func TestFailedNotAnInvoiceMarkCountsAsFailedMail(t *testing.T) {
	t.Parallel()

	box := newBox(mail.Email{ID: "mail-1", Subject: "Newsletter", Attachments: nil})
	box.failFlag = true

	sum, logs := collect(t, box, newInbox(), "")
	if sum.Failed != 1 || sum.Mails != 1 {
		t.Fatalf("summary %+v", sum)
	}

	checkLogsArePrivate(t, logs, "mail-", "Newsletter")
}

func TestReceiptImageIsUploadedButLogoIsSkipped(t *testing.T) {
	t.Parallel()

	png := pngData()
	box := newBox(mail.Email{ID: "m1", Subject: "Kvittering", Attachments: []mail.Part{
		part("logo", "image/png", "logo.png", 900),
		part("photo", "image/png", "photo", len(png)),
	}})
	box.blobs["photo"] = png

	inbox := newInbox()
	if sum, _ := collect(
		t,
		box,
		inbox,
		"",
	); sum.Uploaded != 1 ||
		strings.Join(inbox.uploads, ",") != "photo.png|image/png" {
		t.Fatalf("summary %+v uploads %v", sum, inbox.uploads)
	}
}

func TestFailureLeavesMailUnflaggedForTheNextRun(t *testing.T) {
	t.Parallel()

	for name, setup := range map[string]func(*fakeBox, *fakeInbox){
		"upload":   func(_ *fakeBox, i *fakeInbox) { i.fail = true },
		"download": func(b *fakeBox, _ *fakeInbox) { b.failBlob = "blob-2" },
		"flag":     func(b *fakeBox, _ *fakeInbox) { b.failFlag = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pdf := pdfData()
			box := newBox(mail.Email{
				ID:      "mail-1",
				Subject: invoiceSubject,
				Attachments: []mail.Part{
					part("blob-1", "application/pdf", "a.pdf", len(pdf)),
					part("blob-2", "application/pdf", "b.pdf", len(pdf)),
				},
			})
			box.blobs["blob-1"], box.blobs["blob-2"] = pdf, pdf
			inbox := newInbox()
			setup(box, inbox)

			sum, logs := collect(t, box, inbox, "")
			if sum.Failed != 1 || len(box.flagged) != 0 {
				t.Fatalf("summary %+v flagged %v", sum, box.flagged)
			}

			checkLogsArePrivate(t, logs, "mail-", "blob-", "a.pdf", "b.pdf", invoiceSubject)
		})
	}
}

func TestDuplicateCountsAndFlags(t *testing.T) {
	t.Parallel()

	pdf := pdfData()
	box := newBox(
		mail.Email{
			ID:          "m1",
			Subject:     invoiceSubject,
			Attachments: []mail.Part{part("b1", "application/pdf; name=x", "x.pdf", len(pdf))},
		},
	)
	box.blobs["b1"] = pdf

	inbox := newInbox()
	inbox.duplicate = true

	if sum, _ := collect(
		t,
		box,
		inbox,
		"",
	); sum.Duplicates != 1 ||
		box.flagged["m1"] != vouchers.SentKeyword {
		t.Fatalf("summary %+v flagged %v", sum, box.flagged)
	}
}

// bilagFolder and bilagID name the folder the folder tests read.
const (
	bilagFolder = "Bilag"
	bilagID     = "bilag"
)

func TestNamedFolder(t *testing.T) {
	t.Parallel()

	box := newBox()
	box.folders = append(
		box.folders,
		mail.Mailbox{ID: bilagID, Name: bilagFolder, ParentID: nil, Role: nil},
	)
	collect(t, box, newInbox(), bilagFolder)

	if box.queriedIn != bilagID {
		t.Fatalf("queried %q", box.queriedIn)
	}

	_, err := vouchers.Collect(
		context.Background(),
		box,
		newInbox(),
		vouchers.Options{Folder: "Missing", Limit: 1, Since: firstDay()},
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	if err == nil {
		t.Fatal("a missing folder must stop the run")
	}
}

func TestAmbiguousFolderStopsTheRun(t *testing.T) {
	t.Parallel()

	parent := "archive"
	box := newBox()
	box.folders = append(
		box.folders,
		mail.Mailbox{ID: bilagID, Name: bilagFolder, ParentID: nil, Role: nil},
		mail.Mailbox{ID: "old-bilag", Name: bilagFolder, ParentID: &parent, Role: nil},
	)
	inbox := newInbox()

	_, err := vouchers.Collect(
		context.Background(),
		box,
		inbox,
		vouchers.Options{Folder: bilagFolder, Limit: 1, Since: firstDay()},
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	if err == nil || box.queriedIn != "" || len(inbox.uploads) != 0 {
		t.Fatalf(
			"two folders with one name must stop the run before reading mail: err %v queried %q",
			err,
			box.queriedIn,
		)
	}
}

func TestVoucherType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		part mail.Part
		want string
	}{
		{part("b", "application/PDF", "a", 5), "application/pdf"},
		{part("b", "image/jpg", "a", 20<<10), "image/jpeg"},
		{part("b", "image/jpeg", "a", 1<<10), ""},
		{part("b", "application/pdf", "a", vouchers.MaxBytes+1), ""},
		{part("b", "application/pdf", "a", 0), ""},
		{part("", "application/pdf", "a", 5), ""},
		{part("b", "text/html", "a", 5), ""},
	}
	for _, c := range cases {
		if got, _ := vouchers.VoucherType(c.part); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.part, got, c.want)
		}
	}
}

func TestFileName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Faktura 42.pdf":         "Faktura 42.pdf",
		"../../etc/passwd":       "passwd.pdf",
		`C:\Users\x\invoice.PDF`: "invoice.pdf",
		"":                       "voucher.pdf",
		"bad\"quote\n.pdf":       "badquote.pdf",
	}
	for in, want := range cases {
		if got := vouchers.FileName(in, "application/pdf"); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}
