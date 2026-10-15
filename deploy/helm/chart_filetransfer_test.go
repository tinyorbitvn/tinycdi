package chart_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFileTransferBoundDefaultAndCustom(t *testing.T) {
	if args := backendArgsWith(t); !hasArg(args, "-file-transfer-max-bytes=104857600") {
		t.Errorf("default render lacks the 100 MiB bound: %v", args)
	}
	if args := backendArgsWith(t, "backend.fileTransfer.maxFileBytes=1048576"); !hasArg(args, "-file-transfer-max-bytes=1048576") {
		t.Errorf("custom bound not rendered: %v", args)
	}
	out := renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.fileTransfer.maxFileBytes=0")
	if !strings.Contains(out, "maxFileBytes") && !strings.Contains(out, "minimum") {
		t.Errorf("maxFileBytes=0 must be rejected by the schema: %s", out)
	}
}
