package vouchers

import "github.com/devantler-tech/bookkeeper/internal/mail"

// VoucherType exposes voucherType to the external tests.
func VoucherType(part mail.Part) (string, bool) {
	return voucherType(part)
}

// FileName exposes fileName to the external tests.
func FileName(name, contentType string) string {
	return fileName(name, contentType)
}
