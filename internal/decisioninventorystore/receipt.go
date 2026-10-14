// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventorystore

import outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"

// ReceiptDigest computes the byte-preserving commitment used by Publish without
// reading or changing inventory state. It validates structural binding only;
// callers must authenticate the original receipt and its published membership.
// A digest is neither source authorization nor a training grant.
func ReceiptDigest(binding Binding, intent Intent, receipt outcomes.Receipt) (string, error) {
	if !validBinding(binding) || !validIntent(intent) {
		return "", invalid()
	}
	return receiptDigest(binding, cloneIntent(intent), receipt)
}
