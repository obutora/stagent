package paths

import (
	"io/fs"

	"golang.org/x/sys/windows"
)

// SIDOwner names the account of sid for an OwnerError: DOMAIN\name when it
// resolves, else the SID string.
func SIDOwner(sid *windows.SID) string {
	if account, domain, _, err := sid.LookupAccount(""); err == nil && account != "" {
		if domain != "" {
			return domain + `\` + account
		}
		return account
	}
	return sid.String()
}

// foreignOwner: no stagent directory lies in a shared directory on Windows
// (see ensurePrivateDir), so none can belong to another user.
func foreignOwner(fs.FileInfo) (owner string, foreign bool) { return "", false }
