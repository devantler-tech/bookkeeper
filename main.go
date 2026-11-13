// Command bookkeeper keeps a Numina company's books moving without AI tokens. Its
// collect-vouchers command sends supplier invoices from a JMAP mailbox to Numina's inbox.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/devantler-tech/bookkeeper/internal/mail"
	"github.com/devantler-tech/bookkeeper/internal/numina"
	"github.com/devantler-tech/bookkeeper/internal/vouchers"
)

const (
	maxSecretBytes = 4096
	// defaultLimit, minLimit and maxLimit bound how many mails one run reads.
	defaultLimit = 100
	minLimit     = 1
	maxLimit     = 500
	// httpTimeout bounds one HTTP exchange with the mail server or Numina.
	httpTimeout = 60 * time.Second
)

const usage = `Usage: bookkeeper collect-vouchers --mail-url <https://mail.example.com> --mail-user <user>
	--mail-password-file <path> --numina-token-file <path> [flags]`

var (
	errURLRequired      = errors.New("is required")
	errURLNoHost        = errors.New("must be a full URL such as https://mail.example.com")
	errURLNotHTTPS      = errors.New("must use HTTPS")
	errUserOrLimit      = errors.New("--mail-user is required and --limit must be 1-500")
	errNoSecretFile     = errors.New("no file given")
	errSecretUnreadable = errors.New("the file cannot be read")
	errSecretNotOneLine = errors.New("the file must hold exactly one line")
	errUnusableToken    = errors.New("no usable Numina API key")
)

// options are the collect-vouchers flags.
type options struct {
	mailURL          string
	mailUser         string
	mailPasswordFile string
	numinaURL        string
	numinaTokenFile  string
	folder           string
	limit            int
}

// credentials are the secrets read from the files the flags name.
type credentials struct {
	mailPassword string
	numinaToken  string
}

func run(ctx context.Context, args []string, output io.Writer, httpClient *http.Client) int {
	log := slog.New(slog.NewJSONHandler(output, nil))
	if len(args) == 0 || args[0] != "collect-vouchers" {
		_, _ = fmt.Fprintln(output, usage)

		return 1
	}

	opts, err := parseFlags(args[1:], output)
	if err != nil {
		return 1
	}

	err = opts.validate()
	if err != nil {
		log.Error("refused: " + err.Error())

		return 1
	}

	creds, err := readCredentials(opts, log)
	if err != nil {
		return 1
	}

	return collectVouchers(ctx, opts, creds, httpClient, log)
}

func parseFlags(args []string, output io.Writer) (options, error) {
	var opts options

	flags := flag.NewFlagSet("collect-vouchers", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.mailURL, "mail-url", "", "base URL of the JMAP server")
	flags.StringVar(&opts.mailUser, "mail-user", "", "mail account to read")
	flags.StringVar(
		&opts.mailPasswordFile,
		"mail-password-file",
		"",
		"file holding the mail account's password",
	)
	flags.StringVar(&opts.numinaURL, "numina-url", numina.DefaultBaseURL, "Numina API base URL")
	flags.StringVar(
		&opts.numinaTokenFile,
		"numina-token-file",
		"",
		"file holding the Numina API key",
	)
	flags.StringVar(&opts.folder, "folder", "", "folder to read; empty means the inbox")
	flags.IntVar(&opts.limit, "limit", defaultLimit, "the most mails one run reads (1-500)")

	err := flags.Parse(args)
	if err != nil {
		return opts, fmt.Errorf("parse flags: %w", err)
	}

	return opts, nil
}

// validate checks the flags that need no file read, in the order a user would fix them.
func (o options) validate() error {
	err := checkURL(o.mailURL)
	if err != nil {
		return fmt.Errorf("--mail-url %w", err)
	}

	err = checkURL(o.numinaURL)
	if err != nil {
		return fmt.Errorf("--numina-url %w", err)
	}

	if o.mailUser == "" || o.limit < minLimit || o.limit > maxLimit {
		return errUserOrLimit
	}

	return nil
}

// readCredentials reads both secret files, logging why a run is refused when one is unusable.
func readCredentials(opts options, log *slog.Logger) (credentials, error) {
	password, err := readSecret(opts.mailPasswordFile)
	if err != nil {
		log.Error("refused: no usable mail password", "error", err.Error())

		return credentials{}, err
	}

	token, err := readSecret(opts.numinaTokenFile)
	if err != nil || strings.ContainsAny(token, " \t") {
		log.Error("refused: no usable Numina API key")

		return credentials{}, errUnusableToken
	}

	return credentials{mailPassword: password, numinaToken: token}, nil
}

// collectVouchers runs one pass and logs its summary, which holds counts only.
func collectVouchers(
	ctx context.Context,
	opts options,
	creds credentials,
	httpClient *http.Client,
	log *slog.Logger,
) int {
	box := mail.New(opts.mailURL, opts.mailUser, creds.mailPassword, httpClient)
	inbox := numina.New(opts.numinaURL, creds.numinaToken, httpClient)
	opt := vouchers.Options{Folder: opts.folder, Limit: opts.limit}

	sum, err := vouchers.Collect(ctx, box, inbox, opt, log)
	if err != nil {
		log.Error("run stopped", "error", err.Error(), "summary", sum)

		return 1
	}

	log.Info("run finished", "summary", sum)

	if sum.Failed > 0 {
		return 1
	}

	return 0
}

// checkURL accepts HTTPS, and plain HTTP only to this machine for local testing. It compares the
// parsed host, because a prefix such as http://localhost also matches http://localhost.example.
func checkURL(raw string) error {
	if raw == "" {
		return errURLRequired
	}

	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		return errURLNoHost
	}

	switch target.Scheme {
	case "https":
		return nil
	case "http":
		if slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, target.Hostname()) {
			return nil
		}
	}

	return errURLNotHTTPS
}

func readSecret(path string) (string, error) {
	if path == "" {
		return "", errNoSecretFile
	}

	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", errSecretUnreadable
	}

	// The file is only read, so a failed close loses nothing.
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	if err != nil || len(data) > maxSecretBytes {
		return "", errSecretUnreadable
	}

	secret := strings.TrimRight(string(data), "\r\n")
	if secret == "" || strings.ContainsAny(secret, "\r\n") {
		return "", errSecretNotOneLine
	}

	return secret, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: nil, // http.DefaultTransport
		Jar:       nil, // no cookies are kept between requests
		Timeout:   httpTimeout,
		// A redirect could carry credentials to another host, so none is followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// execute runs the command line with an interrupt or SIGTERM cancelling the run.
func execute(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return run(ctx, args, os.Stdout, newHTTPClient())
}

func main() {
	os.Exit(execute(os.Args[1:]))
}
