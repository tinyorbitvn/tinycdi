// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker

// WithTransferClaimHook installs a hook ClaimTransfer runs after it read
// the lease live and before its claim transaction, so a test can commit a
// release in that gap.
func (b *Broker) WithTransferClaimHook(h func()) { b.transferClaimHook = h }
