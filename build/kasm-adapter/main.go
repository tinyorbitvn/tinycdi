// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Command install-adapter is the tinycdi-kasm-adapter init image's entrypoint:
// it writes the kasm-adapter scripts (entrypoint.sh, healthcheck.sh,
// xstartup.sh) into a shared volume mounted at $TCDI_ADAPTER_DIR
// (default /opt/tcdi), which the runtime pod's desktop container then mounts
// read-only. The kasmweb/* image itself is never modified — the operator
// injects the adapter with initContainer semantics (docs/kasm-images.md).
package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed entrypoint.sh
var entrypoint []byte

//go:embed healthcheck.sh
var healthcheck []byte

//go:embed xstartup.sh
var xstartup []byte

func main() {
	dir := os.Getenv("TCDI_ADAPTER_DIR")
	if dir == "" {
		dir = "/opt/tcdi"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir %s: %v", dir, err)
	}
	scripts := []struct {
		name string
		data []byte
	}{
		{"entrypoint.sh", entrypoint},
		{"healthcheck.sh", healthcheck},
		{"xstartup.sh", xstartup},
	}
	for _, s := range scripts {
		p := filepath.Join(dir, s.name)
		if err := os.WriteFile(p, s.data, 0o755); err != nil {
			fatal("write %s: %v", p, err)
		}
	}
	fmt.Printf("install-adapter: wrote %d adapter scripts to %s\n", len(scripts), dir)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "install-adapter: "+format+"\n", args...)
	os.Exit(1)
}
