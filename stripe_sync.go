package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/devantler-tech/bookkeeper/internal/numina"
	"github.com/devantler-tech/bookkeeper/internal/stripefees"
)

const (
	stripeDay       = 24 * time.Hour
	maxStripeDays   = 31
	maxStripeWindow = maxStripeDays * stripeDay
)

var (
	companyIDFormat = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
	)
	errStripeFlags = errors.New(
		"provide a stripe key file and a reporting window of up to 31 days",
	)
	errStripeDrafts = errors.New(
		"draft writes require a Numina token file, company ID and two different account numbers",
	)
)

type stripeOptions struct {
	keyFile        string
	tokenFile      string
	company        string
	balanceAccount int
	feeAccount     int
	since          time.Time
	until          time.Time
	live           bool
	write          bool
}

type stripeSummary struct {
	Eligible int `json:"eligible"`
	Deferred int `json:"deferred"`
	Drafted  int `json:"drafted"`
	Existing int `json:"existing"`
}

func runStripeFees(
	ctx context.Context,
	args []string,
	output io.Writer,
	client *http.Client,
	log *slog.Logger,
) int {
	opts, err := stripeFlags(args, output)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}

	if err != nil {
		log.Error("stripe sync refused", "error", err.Error())

		return 1
	}

	key, err := readSecret(opts.keyFile)
	if err != nil || !matchesStripeMode(key, opts.live) {
		log.Error("stripe sync refused: use a restricted Stripe key for the selected mode")

		return 1
	}

	result, err := stripefees.Read(ctx, client, key, opts.since, opts.until)
	if err != nil {
		log.Error("stripe sync stopped", "error", err.Error())

		return 1
	}

	summary := stripeSummary{
		Eligible: len(result.Fees),
		Deferred: result.Deferred,
		Drafted:  0,
		Existing: 0,
	}

	if opts.write {
		err = writeStripeFees(ctx, client, opts, result.Fees, &summary)
		if err != nil {
			log.Error("stripe sync stopped", "error", err.Error(), "summary", summary)

			return 1
		}
	}

	log.Info("stripe sync finished", "summary", summary)

	return 0
}

func stripeFlags(args []string, output io.Writer) (stripeOptions, error) {
	var opts stripeOptions

	var since, until string

	var lookback int

	flags := flag.NewFlagSet("sync-stripe-fees", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.keyFile, "stripe-key-file", "", "file holding a restricted Stripe key")
	flags.StringVar(&since, "since", "", "first Stripe ledger date, inclusive (YYYY-MM-DD UTC)")
	flags.StringVar(&until, "until", "", "last Stripe ledger date, exclusive (YYYY-MM-DD UTC)")
	flags.IntVar(&lookback, "lookback-days", 0, "rolling UTC review window ending tomorrow")
	flags.BoolVar(
		&opts.live,
		"live",
		false,
		"read live Stripe activity instead of sandbox activity",
	)
	flags.BoolVar(&opts.write, "write-drafts", false, "create Numina fee drafts")
	flags.StringVar(
		&opts.tokenFile,
		"numina-token-file",
		"",
		"file holding the registered Numina integration's OAuth access token",
	)
	flags.StringVar(&opts.company, "numina-company-id", "", "expected Numina company ID")
	flags.IntVar(
		&opts.balanceAccount,
		"stripe-balance-account",
		0,
		"Numina Stripe clearing account",
	)
	flags.IntVar(&opts.feeAccount, "stripe-fee-account", 0, "Numina Stripe fee expense account")

	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return opts, flag.ErrHelp
	}

	if err != nil || flags.NArg() != 0 || opts.keyFile == "" {
		return opts, errStripeFlags
	}

	opts.since, opts.until, err = stripeWindow(since, until, lookback, time.Now())
	if err != nil {
		return opts, err
	}

	if opts.write && !opts.hasDraftDestination() {
		return opts, errStripeDrafts
	}

	return opts, nil
}

func stripeWindow(since, until string, days int, now time.Time) (time.Time, time.Time, error) {
	if days != 0 {
		if days < 1 || days > maxStripeDays || since != "" || until != "" {
			return time.Time{}, time.Time{}, errStripeFlags
		}

		end := now.UTC().Truncate(stripeDay).AddDate(0, 0, 1)

		return end.AddDate(0, 0, -days), end, nil
	}

	start, startErr := time.Parse(dayLayout, since)
	end, endErr := time.Parse(dayLayout, until)

	if startErr != nil || endErr != nil || !end.After(start) || end.Sub(start) > maxStripeWindow {
		return time.Time{}, time.Time{}, errStripeFlags
	}

	return start, end, nil
}

func (opts stripeOptions) hasDraftDestination() bool {
	return opts.tokenFile != "" && companyIDFormat.MatchString(opts.company) &&
		opts.balanceAccount > 0 && opts.feeAccount > 0 && opts.balanceAccount != opts.feeAccount
}

func matchesStripeMode(key string, live bool) bool {
	if live {
		return strings.HasPrefix(key, "rk_live_") && len(key) > len("rk_live_")
	}

	return strings.HasPrefix(key, "rk_test_") && len(key) > len("rk_test_")
}

func writeStripeFees(
	ctx context.Context,
	client *http.Client,
	opts stripeOptions,
	fees []stripefees.Fee,
	summary *stripeSummary,
) error {
	accessToken, err := readSecret(opts.tokenFile)
	if err != nil {
		return err
	}

	journal := numina.New(numina.DefaultBaseURL, accessToken, client)

	err = journal.CheckDraftAccess(ctx, opts.company)
	if err != nil {
		return fmt.Errorf("check numina draft access: %w", err)
	}

	for _, fee := range fees {
		existing, writeErr := journal.CreateStripeFeeDraft(ctx, numina.StripeFeeDraft{
			TransactionID:  fee.TransactionID,
			Date:           fee.Date,
			Amount:         fee.Amount,
			BalanceAccount: opts.balanceAccount,
			FeeAccount:     opts.feeAccount,
		})
		if writeErr != nil {
			return fmt.Errorf("create stripe fee draft: %w", writeErr)
		}

		if existing {
			summary.Existing++
		} else {
			summary.Drafted++
		}
	}

	return nil
}
