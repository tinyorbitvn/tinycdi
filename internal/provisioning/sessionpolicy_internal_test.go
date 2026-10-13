// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"testing"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// Meet matrix over EVERY template policy × ceiling combination (ADR 0010
// §1): Disabled is bottom, Bidirectional top, the single directions are
// incomparable (Send ∩ Receive = Disabled), FollowTemplate and an empty
// ceiling narrow nothing, and a ceiling can never widen a template.
// dirA/dirB stand for any direction pair; the clipboard run uses the real
// Send/Receive, the file-transfer run Upload/Download.
func TestSessionPolicyMeet_Matrix(t *testing.T) {
	templates := []string{PolicyDisabled, "Send", "Receive", PolicyBidirectional}
	ceilings := []string{"", PolicyFollowTemplate, PolicyDisabled, "Send", "Receive", PolicyBidirectional, "Bogus"}

	for _, tpl := range templates {
		for _, ceil := range ceilings {
			got := meet(tpl, ceil, ClipboardSend, ClipboardReceive)
			want := wantMeet(tpl, ceil)
			if got != want {
				t.Errorf("meet(%q, %q) = %q, want %q", tpl, ceil, got, want)
			}
		}
	}

	// The lattice is direction-agnostic: same matrix for file transfer.
	if got := meet(PolicyBidirectional, FileTransferUpload, FileTransferUpload, FileTransferDownload); got != FileTransferUpload {
		t.Errorf("meet(Bidirectional, Upload) = %q", got)
	}
	if got := meet(FileTransferUpload, FileTransferDownload, FileTransferUpload, FileTransferDownload); got != PolicyDisabled {
		t.Errorf("meet(Upload, Download) = %q", got)
	}
}

func wantMeet(tpl, ceil string) string {
	switch ceil {
	case "", PolicyFollowTemplate, PolicyBidirectional, "Bogus":
		return tpl
	case PolicyDisabled:
		return PolicyDisabled
	default: // Send or Receive
		if tpl == ceil || tpl == PolicyBidirectional {
			return ceil
		}
		return PolicyDisabled
	}
}

// A resolved vector carries the inert file-transfer channel and, while
// the catalog entry exposes no fileTransferPolicy leg (F4/T11 wires it),
// resolves it Disabled regardless of the stored ceiling row.
func TestSessionPolicyMeetCeiling_FileTransferInert(t *testing.T) {
	dl := FileTransferDownload
	max := int64(1024)
	sp := meetCeiling(
		TemplateCatalogEntry{ClipboardPolicy: PolicyBidirectional},
		store.SessionPolicyCeiling{
			HasRow:       true,
			FileTransfer: &dl,
			FileMaxBytes: &max,
		},
	)
	if sp.FileTransfer != cdiv1.FileTransferDisabled {
		t.Fatalf("fileTransfer resolved %q, want Disabled until F4/T11", sp.FileTransfer)
	}
}
