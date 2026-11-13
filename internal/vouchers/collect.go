// Package vouchers sends supplier invoices from a mailbox to Numina's inbox. It is deterministic:
// every decision is a fixed rule on words, file type and content, never a model.
package vouchers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/devantler-tech/bookkeeper/internal/mail"
	"github.com/devantler-tech/bookkeeper/internal/numina"
)

const (
	// MaxBytes is Numina's upload limit.
	MaxBytes = 10 << 20
	// minImageBytes skips smaller images, which are almost always signature logos, not receipts.
	minImageBytes = 10 << 10
	// SentKeyword marks a mail whose vouchers are in Numina, so the next run skips it.
	SentKeyword = "$bk-sent-to-numina"
	// NotInvoiceKeyword marks a mail that holds no voucher to send, so later runs skip it and
	// reach newer mail.
	NotInvoiceKeyword = "$bk-not-an-invoice"

	// pdfHeaderWindow is how far into a file the %PDF- marker may start.
	pdfHeaderWindow = 1024
	// maxNameRunes caps the length of an uploaded file's base name.
	maxNameRunes = 120

	typePDF  = "application/pdf"
	typePNG  = "image/png"
	typeJPEG = "image/jpeg"
)

var (
	errNoInbox  = errors.New("the mail account has no inbox")
	errNoFolder = errors.New("the mail account has no folder with the name --folder gives")
)

// Mailbox is what the collector needs from a mail server.
type Mailbox interface {
	Connect(ctx context.Context) error
	Mailboxes(ctx context.Context) ([]mail.Mailbox, error)
	Query(
		ctx context.Context,
		mailboxID string,
		since time.Time,
		skipKeywords []string,
		limit int,
	) ([]string, error)
	Emails(ctx context.Context, ids []string) ([]mail.Email, error)
	Flag(ctx context.Context, emailID, keyword string) error
	Download(ctx context.Context, part mail.Part, limit int64) ([]byte, error)
}

// Inbox is what the collector needs from the bookkeeping system.
type Inbox interface {
	UploadToInbox(
		ctx context.Context,
		fileName, contentType string,
		data []byte,
	) (numina.UploadResult, error)
}

// Options says where to read and how much.
type Options struct {
	Folder string    // the folder to read; empty means the inbox
	Limit  int       // the most mails one run reads
	Since  time.Time // only mail received at or after this time is read; zero means all of it
}

// Summary holds counts only: no sender, subject, file name or amount reaches the logs.
type Summary struct {
	Mails      int `json:"mailsRead"`
	Invoices   int `json:"invoicesSent"`
	Uploaded   int `json:"filesUploaded"`
	Duplicates int `json:"filesNuminaAlreadyHad"`
	Failed     int `json:"mailsFailed"`
}

// Collect runs one pass over the oldest unmarked mails with attachments. A mail that looks like an
// invoice has its vouchers uploaded and is then marked with SentKeyword; any other mail is marked
// with NotInvoiceKeyword, so later runs reach newer mail. Mail is never moved or changed otherwise.
// A mail whose download, upload or marking fails stays unmarked for the next run, and Numina drops
// files it already has.
func Collect(
	ctx context.Context,
	box Mailbox,
	inbox Inbox,
	opt Options,
	log *slog.Logger,
) (Summary, error) {
	var sum Summary

	messages, err := unhandledMail(ctx, box, opt)
	if err != nil {
		return sum, err
	}

	for _, message := range messages {
		sum.Mails++

		keyword, failed := handle(ctx, box, inbox, message, &sum, log)
		if failed {
			sum.Failed++

			continue
		}

		err := box.Flag(ctx, message.ID, keyword)
		if err != nil {
			sum.Failed++

			log.Error("could not mark mail as handled", "error", err.Error())

			continue
		}

		if keyword == SentKeyword {
			sum.Invoices++
		}
	}

	return sum, nil
}

// handle sends the vouchers of a mail that looks like an invoice. It returns the keyword that marks
// the mail as handled, and whether a download or upload failed, which keeps the mail for the next
// run. A mail with no voucher to send gets NotInvoiceKeyword.
func handle(
	ctx context.Context,
	box Mailbox,
	inbox Inbox,
	message mail.Email,
	sum *Summary,
	log *slog.Logger,
) (string, bool) {
	if !looksLikeInvoice(message) {
		return NotInvoiceKeyword, false
	}

	files, failed := sendVouchers(ctx, box, inbox, message, sum, log)
	if files == 0 {
		return NotInvoiceKeyword, failed
	}

	return SentKeyword, failed
}

// unhandledMail reads the mails in the configured folder, received since opt.Since, that have
// attachments and carry neither SentKeyword nor NotInvoiceKeyword.
func unhandledMail(ctx context.Context, box Mailbox, opt Options) ([]mail.Email, error) {
	err := box.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to the mail server: %w", err)
	}

	folders, err := box.Mailboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}

	source, err := findFolder(folders, opt.Folder)
	if err != nil {
		return nil, err
	}

	ids, err := box.Query(
		ctx,
		source,
		opt.Since,
		[]string{SentKeyword, NotInvoiceKeyword},
		opt.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("find mail: %w", err)
	}

	messages, err := box.Emails(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read mail: %w", err)
	}

	return messages, nil
}

// sendVouchers uploads a mail's vouchers and counts them in sum. It returns how many files Numina
// now holds and whether any download or upload failed, which keeps the mail for the next run.
func sendVouchers(
	ctx context.Context,
	box Mailbox,
	inbox Inbox,
	message mail.Email,
	sum *Summary,
	log *slog.Logger,
) (int, bool) {
	files, failed := 0, false

	for _, part := range message.Attachments {
		contentType, ok := voucherType(part)
		if !ok {
			continue
		}

		data, err := box.Download(ctx, part, MaxBytes)
		if err != nil {
			failed = true

			log.Error("attachment download failed", "error", err.Error())

			continue
		}

		if !contentMatches(contentType, data) {
			continue
		}

		result, err := inbox.UploadToInbox(ctx, fileName(part.Name, contentType), contentType, data)
		if err != nil {
			failed = true

			log.Error("upload to Numina failed", "error", err.Error())

			continue
		}

		files++

		if result.Duplicate {
			sum.Duplicates++
		} else {
			sum.Uploaded++
		}
	}

	return files, failed
}

// looksLikeInvoice decides that a mail is an invoice when its subject or an attachment's file name
// contains an invoice word. Anything else in the mailbox is left alone.
func looksLikeInvoice(message mail.Email) bool {
	var text strings.Builder

	text.WriteString(strings.ToLower(message.Subject))

	for _, part := range message.Attachments {
		text.WriteString(" " + strings.ToLower(part.Name))
	}

	words := []string{"invoice", "faktura", "receipt", "kvittering", "regning", "bilag"}
	for _, word := range words {
		if strings.Contains(text.String(), word) {
			return true
		}
	}

	return false
}

func findFolder(folders []mail.Mailbox, name string) (string, error) {
	for _, folder := range folders {
		if name == "" && folder.Role != nil && *folder.Role == "inbox" {
			return folder.ID, nil
		}

		if name != "" && folder.Name == name {
			return folder.ID, nil
		}
	}

	if name == "" {
		return "", errNoInbox
	}

	// The folder's name can name a supplier, so it never reaches the log.
	return "", errNoFolder
}

// extension returns the extension a file of an accepted type gets, and false for any type
// Numina does not accept.
func extension(contentType string) (string, bool) {
	switch contentType {
	case typePDF:
		return ".pdf", true
	case typePNG:
		return ".png", true
	case typeJPEG:
		return ".jpg", true
	default:
		return "", false
	}
}

// voucherType returns the accepted type of an attachment, or false when it is not a voucher
// Numina takes: another type, empty, too large, or an image too small to be a receipt.
func voucherType(part mail.Part) (string, bool) {
	mediaType, _, _ := strings.Cut(part.Type, ";")

	contentType := strings.ToLower(strings.TrimSpace(mediaType))
	if contentType == "image/jpg" {
		contentType = typeJPEG
	}

	if _, ok := extension(contentType); !ok || part.BlobID == "" || part.Size <= 0 ||
		part.Size > MaxBytes {
		return "", false
	}

	if strings.HasPrefix(contentType, "image/") && part.Size < minImageBytes {
		return "", false
	}

	return contentType, true
}

// contentMatches checks a file's first bytes, because Numina refuses a file whose content does
// not match its declared type.
func contentMatches(contentType string, data []byte) bool {
	switch contentType {
	case typePDF:
		head := data
		if len(head) > pdfHeaderWindow {
			head = head[:pdfHeaderWindow]
		}

		return bytes.Contains(head, []byte("%PDF-"))
	case typePNG:
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case typeJPEG:
		return bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF})
	}

	return false
}

// fileName keeps a readable, safe base name with the extension of the file's real type.
func fileName(name, contentType string) string {
	base := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}

		return r
	}, path.Base(strings.ReplaceAll(name, "\\", "/")))

	base = strings.TrimSpace(strings.TrimSuffix(base, path.Ext(base)))
	if base == "" || base == "." || base == "/" {
		base = "voucher"
	}

	if runes := []rune(base); len(runes) > maxNameRunes {
		base = string(runes[:maxNameRunes])
	}

	ext, _ := extension(contentType)

	return base + ext
}
