package approval

import "errors"

// ErrOwnershipUnsupported means private-file ownership cannot be established
// on the platform. Such files cannot grant trust or cached approval.
var ErrOwnershipUnsupported = errors.New("approval: private file ownership verification is unavailable on this platform")
