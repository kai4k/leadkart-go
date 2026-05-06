#!/usr/bin/env bash
#
# scripts/smoke.sh — repeatable smoke test against a running leadkart-go
# stack. Designed for after `docker compose --profile full up -d`.
#
# Usage:
#   ./scripts/smoke.sh                      # localhost defaults (8080 + 9090)
#   API_URL=http://api.example.test \
#   ADMIN_URL=http://api.example.test:9090 \
#       ./scripts/smoke.sh                  # custom endpoints
#
# Exits 0 on full success, non-zero on the first failed assertion.

set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"
ADMIN_URL="${ADMIN_URL:-http://localhost:9090}"

# Per-run unique slug + email so the script is rerun-safe without
# wiping the database. Requires `bash` 5+ for $RANDOM behaviour.
RUN_ID="smoke-$(date +%s)-${RANDOM}"
TENANT_SLUG="${RUN_ID}-co"
ADMIN_EMAIL="${RUN_ID}@smoke.test"
ADMIN_PASSWORD="correct-horse-battery-staple-1234"

red()    { printf '\033[31m%s\033[0m\n' "$1" >&2; }
green()  { printf '\033[32m%s\033[0m\n' "$1"; }
yellow() { printf '\033[33m%s\033[0m\n' "$1"; }

# require — assert exit 0 from the previous step OR explain + die.
fail() {
    red "FAIL: $1"
    exit 1
}

# ------ Probes (admin listener) -------------------------------------------
yellow "[1/5] Admin probes — /alive, /ready, /health"

curl -fsS "${ADMIN_URL}/alive"  >/dev/null || fail "admin /alive not 200"
curl -fsS "${ADMIN_URL}/ready"  >/dev/null || fail "admin /ready not 200 (postgres reachable?)"
curl -fsS "${ADMIN_URL}/health" >/dev/null || fail "admin /health not 200"
green "    ✓ all three probes 200"

# ------ Tenant registration -----------------------------------------------
yellow "[2/5] Register tenant ${TENANT_SLUG}"

REG_BODY=$(cat <<EOF
{
  "slug": "${TENANT_SLUG}",
  "legal_name": "Smoke Test ${RUN_ID} Pvt Ltd",
  "display_name": "Smoke ${RUN_ID}",
  "admin_email": "${ADMIN_EMAIL}",
  "admin_password": "${ADMIN_PASSWORD}",
  "admin_first_name": "Smoke",
  "admin_last_name": "Tester"
}
EOF
)

REG_RESP=$(curl -fsS -X POST "${API_URL}/api/v1/tenants" \
    -H "Content-Type: application/json" \
    -d "${REG_BODY}") || fail "tenant registration failed"

TENANT_ID=$(echo "${REG_RESP}" | sed -n 's/.*"tenant_id":"\([^"]*\)".*/\1/p')
PERSON_ID=$(echo "${REG_RESP}" | sed -n 's/.*"person_id":"\([^"]*\)".*/\1/p')
[[ -n "${TENANT_ID}" ]]  || fail "tenant_id absent from registration response: ${REG_RESP}"
[[ -n "${PERSON_ID}" ]]  || fail "person_id absent from registration response: ${REG_RESP}"
green "    ✓ tenant_id=${TENANT_ID:0:12}… person_id=${PERSON_ID:0:12}…"

# ------ Login -------------------------------------------------------------
yellow "[3/5] Login as ${ADMIN_EMAIL}"

LOGIN_RESP=$(curl -fsS -X POST "${API_URL}/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PASSWORD}\",\"device_label\":\"smoke\"}") \
    || fail "login failed"

ACCESS_TOKEN=$(echo "${LOGIN_RESP}" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
REFRESH_TOKEN=$(echo "${LOGIN_RESP}" | sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p')
[[ -n "${ACCESS_TOKEN}" ]]  || fail "access_token absent from login response"
[[ -n "${REFRESH_TOKEN}" ]] || fail "refresh_token absent from login response"
green "    ✓ access_token issued (${#ACCESS_TOKEN} chars), refresh_token issued"

# ------ Refresh -----------------------------------------------------------
yellow "[4/5] Refresh — rotate token chain"

REFRESH_RESP=$(curl -fsS -X POST "${API_URL}/api/v1/auth/refresh" \
    -H "Content-Type: application/json" \
    -d "{\"refresh_token\":\"${REFRESH_TOKEN}\"}") \
    || fail "refresh failed"

NEW_ACCESS=$(echo "${REFRESH_RESP}" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
NEW_REFRESH=$(echo "${REFRESH_RESP}" | sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p')
[[ -n "${NEW_ACCESS}" ]]  || fail "access_token absent from refresh response"
[[ "${NEW_REFRESH}" != "${REFRESH_TOKEN}" ]] || fail "refresh did not rotate token (old==new)"
green "    ✓ refresh rotated: new access + new refresh"

# ------ Logout ------------------------------------------------------------
yellow "[5/5] Logout — revoke family"

LOGOUT_HTTP=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST "${API_URL}/api/v1/auth/logout" \
    -H "Content-Type: application/json" \
    -d "{\"refresh_token\":\"${NEW_REFRESH}\"}")
[[ "${LOGOUT_HTTP}" == "204" || "${LOGOUT_HTTP}" == "200" ]] \
    || fail "logout returned ${LOGOUT_HTTP} (expected 204/200)"
green "    ✓ logout ${LOGOUT_HTTP}"

# Reuse-detection: replaying the now-revoked token MUST 401
yellow "    extra: replay revoked refresh — expect 401"
REPLAY_HTTP=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST "${API_URL}/api/v1/auth/refresh" \
    -H "Content-Type: application/json" \
    -d "{\"refresh_token\":\"${NEW_REFRESH}\"}")
[[ "${REPLAY_HTTP}" == "401" ]] || fail "revoked-token replay returned ${REPLAY_HTTP} (expected 401)"
green "    ✓ revoked-token replay 401 (RFC 9700 §4.13 reuse detection)"

green ""
green "All smoke checks passed for ${RUN_ID}."
