//go:build !unix

package approval

import "os"

// validateFileOwner fails closed until a native owner/ACL implementation is
// available. Cross-compilation does not imply private config execution support.
func validateFileOwner(os.FileInfo, string) error { return ErrOwnershipUnsupported }
