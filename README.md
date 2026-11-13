# bookkeeper

`bookkeeper` sends supplier invoices from your mailbox to [Numina](https://numina.app), a registered
Danish bookkeeping system, so the books get their vouchers without anyone forwarding mail. It reads
the mailbox your suppliers already send to and uses no AI: every decision is a fixed rule. Numina's
own AI then matches each voucher to its bank line.

## Install with Helm

The chart runs it as a daily Kubernetes CronJob. Give it a Secret with the mailbox password and a
Numina API key, then install:

```bash
kubectl create secret generic bookkeeper \
  --from-file=mail-password=./mail-password \
  --from-file=numina-api-key=./numina-api-key
helm install bookkeeper oci://ghcr.io/devantler-tech/charts/bookkeeper \
  --set mail.url=https://mail.example.com \
  --set mail.user=you@example.com \
  --set since=2026-01-01 \
  --set existingSecret.name=bookkeeper
```

Each run looks at mail in the inbox, received on or after `since`, that has attachments and that it
has not handled before:

- **An invoice** — "invoice", "faktura", "receipt", "kvittering", "regning" or "bilag" in the
  subject or a file name. Its PDF, PNG or JPEG attachments go to Numina's inbox with OCR, and the
  mail gets a hidden `$bk-sent-to-numina` keyword so the next run skips it.
- **Anything else** — mail that is not an invoice, or has no PDF, PNG or JPEG voucher — gets a
  hidden `$bk-not-an-invoice` keyword so later runs skip it and reach newer mail.
- **If a download, upload or marking fails,** the mail stays unmarked and the next run tries again.
  Numina drops files it already has.

It never moves or deletes mail, and changes nothing on it but those two hidden keywords. The run
exits non-zero when a mail failed, so a failed Job shows up in your cluster's alerting. Its logs
carry counts only, never senders, subjects, file names or mail IDs. An invoice it misses is still
caught by Numina's own missing-voucher reminder.

## Inputs

| Value                  | Meaning                                                                                                        | Default                           |
|------------------------|----------------------------------------------------------------------------------------------------------------|-----------------------------------|
| `mail.url`             | Base URL of a [JMAP](https://jmap.io) server, HTTPS only                                                       | required                          |
| `mail.user`            | The mailbox your suppliers send invoices to                                                                    | required                          |
| `mail.folder`          | Folder to read; empty means the inbox                                                                          | `""`                              |
| `existingSecret.name`  | Secret holding the keys `mail-password` and `numina-api-key`                                                   | required                          |
| `schedule`, `timeZone` | When it runs                                                                                                   | `15 6 * * *`, `Europe/Copenhagen` |
| `since`                | Only mail received on or after this day (`YYYY-MM-DD`) is read; set it to the first day of the books in Numina | required                          |
| `limit`                | The most mails one run reads, 1–500                                                                            | `100`                             |
| `image.digest`         | Pin the exact image                                                                                            | `""`                              |

Without Kubernetes:

```bash
bookkeeper collect-vouchers --mail-url https://mail.example.com --mail-user you@example.com \
  --mail-password-file ./mail-password --numina-token-file ./numina-api-key --since 2026-01-01
```

## What it needs

- The mailbox's JMAP address and password.
- A Numina API key with upload rights (`documents:write`). It cannot book, change or delete anything
  in the books.
- Egress to the mail server and to `api.numina.app` on port 443.
