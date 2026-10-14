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

func TestSessionPolicyMeetCeiling_FileTransferLeg(t *testing.T) {
	up, dl, bi, off := FileTransferUpload, FileTransferDownload, PolicyBidirectional, PolicyDisabled
	small, huge := int64(1024), int64(1<<40)
	const bound = int64(100 << 20)
	cases := []struct {
		name     string
		tpl      string
		ceil     store.SessionPolicyCeiling
		wantMode cdiv1.FileTransferPolicy
		wantMax  int64
	}{
		{"no row follows template, bound applies", "Bidirectional", store.SessionPolicyCeiling{}, cdiv1.FileTransferBidirectional, bound},
		{"template disabled stays disabled", "Disabled", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &bi}, cdiv1.FileTransferDisabled, 0},
		{"empty template leg is disabled", "", store.SessionPolicyCeiling{}, cdiv1.FileTransferDisabled, 0},
		{"ceiling narrows to upload", "Bidirectional", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &up}, cdiv1.FileTransferUpload, bound},
		{"incomparable directions meet to disabled", "Download", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &up}, cdiv1.FileTransferDisabled, 0},
		{"ceiling disabled", "Download", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &off}, cdiv1.FileTransferDisabled, 0},
		{"ceiling size below bound wins", "Download", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &dl, FileMaxBytes: &small}, cdiv1.FileTransferDownload, small},
		{"ceiling size above bound is clipped", "Download", store.SessionPolicyCeiling{HasRow: true, FileMaxBytes: &huge}, cdiv1.FileTransferDownload, bound},
	}
	for _, c := range cases {
		sp := meetCeiling(TemplateCatalogEntry{ClipboardPolicy: PolicyDisabled, FileTransferPolicy: c.tpl}, c.ceil, bound)
		if sp.FileTransfer != c.wantMode || sp.MaxFileBytes != c.wantMax {
			t.Errorf("%s: got %s/%d, want %s/%d", c.name, sp.FileTransfer, sp.MaxFileBytes, c.wantMode, c.wantMax)
		}
	}
}

func TestPolicyWithin(t *testing.T) {
	eff := SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 1000}
	for _, c := range []struct {
		applied SessionPolicy
		want    bool
	}{
		{SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 1000}, true},
		{SessionPolicy{Clipboard: PolicyDisabled, FileTransfer: cdiv1.FileTransferDisabled}, true},
		{SessionPolicy{Clipboard: PolicyBidirectional, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 1000}, false},
		{SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferBidirectional, MaxFileBytes: 1000}, false},
		{SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 2000}, false},
		{SessionPolicy{Clipboard: ClipboardReceive, FileTransfer: cdiv1.FileTransferDisabled}, false},
	} {
		if got := PolicyWithin(c.applied, eff); got != c.want {
			t.Errorf("PolicyWithin(%+v) = %v, want %v", c.applied, got, c.want)
		}
	}
}

func TestDirectionAllows(t *testing.T) {
	if !DirectionAllows(cdiv1.FileTransferBidirectional, "upload") || !DirectionAllows(cdiv1.FileTransferDownload, "download") {
		t.Fatal("allowed direction refused")
	}
	if DirectionAllows(cdiv1.FileTransferUpload, "download") || DirectionAllows(cdiv1.FileTransferDisabled, "upload") || DirectionAllows(cdiv1.FileTransferBidirectional, "sideways") {
		t.Fatal("refused direction allowed")
	}
}
