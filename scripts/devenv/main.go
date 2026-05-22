// Package main is the dev .env generator.
//
// Writes a docker/.env-shaped file to stdout — a JWT signing key
// (32-byte cryptographic random, hex-encoded) + a stable per-host
// key id + the canonical SuperAdmin dev credentials.
//
// Why a Go binary, not a shell heredoc: Windows shells often lack
// `openssl` / `date` / coreutils when Task drops into mvdan/sh, so
// `$(openssl rand ...)` substitution silently produces empty values.
// The whole project depends on a working Go toolchain anyway, so
// the same primitives crypto/rand + encoding/hex + time give a
// portable, deterministic generator.
//
// Invoked by `task docker:env`. Output is redirected to docker/.env
// (Taskfile owns the file path; this binary is stdout-only).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

const envTemplate = `# Auto-minted by ` + "`go run ./scripts/devenv`" + ` for local dev. Throwaway creds.
# Rotate via the platform admin UI before any non-dev use.
# DO NOT commit — covered by the root .gitignore '.env' rule.
LEADKART_JWT__KEY_ID=local-dev-%d
LEADKART_JWT__SIGNING_KEY=%s
LEADKART_SUPERADMIN__EMAIL=superadmin@leadkart.local
LEADKART_SUPERADMIN__PASSWORD=LeadKart!Dev2026
LEADKART_SUPERADMIN__FIRST_NAME=Platform
LEADKART_SUPERADMIN__LAST_NAME=SuperAdmin
`

func main() {
	// 32 bytes = 256 bits — RFC 7519 / RFC 7518 minimum for HS256
	// (HMAC-SHA-256). Application's own validator
	// (internal/common/config/validate.go) requires ≥ 32 bytes.
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		fmt.Fprintln(os.Stderr, "devenv: crypto/rand:", err)
		os.Exit(1)
	}
	fmt.Printf(envTemplate, time.Now().Unix(), hex.EncodeToString(keyBytes))
}
