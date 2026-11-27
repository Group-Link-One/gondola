#!/bin/bash

# Local smoke test for deploy-api.
# Exercises the three auth principals introduced by the auth refactor:
#   - dp_* user token  (day-to-day on /api/v1/*)
#   - DEPLOY_ROOT_KEY  (break-glass admin)
#   - DEPLOY_API_KEY   (CI ingress, /external/* only)

set -e

API_URL="${API_URL:-http://localhost:8080}"
CI_KEY="${DEPLOY_API_KEY:-test-key-local}"
ROOT_KEY="${DEPLOY_ROOT_KEY:-test-root-key-local}"

echo "=== Deploy API Local Test ==="
echo ""

if ! curl -s "$API_URL/health" > /dev/null 2>&1; then
    echo "❌ Server is not running. Start it with ./start-local.sh"
    exit 1
fi
echo "✅ Server is running"
echo ""

# ----------------------------------------------------------------------------
# 1. Auth boundary checks (the new policy)
# ----------------------------------------------------------------------------
echo "1. Auth policy spot-checks"

echo "   a) CI key on /api/v1/* must be 401"
code=$(curl -s -o /dev/null -w '%{http_code}' "$API_URL/api/v1/repos" -H "Authorization: Bearer $CI_KEY")
if [ "$code" != "401" ]; then
    echo "   ❌ expected 401, got $code"; exit 1
fi
echo "   ✅ rejected"

echo "   b) Root key on /api/v1/* must be 200"
code=$(curl -s -o /dev/null -w '%{http_code}' "$API_URL/api/v1/repos" -H "Authorization: Bearer $ROOT_KEY")
if [ "$code" != "200" ]; then
    echo "   ❌ expected 200, got $code"; exit 1
fi
echo "   ✅ admitted"

echo "   c) CI key on /external/builds must NOT be 401 (validation 400 ok)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API_URL/external/builds" \
    -H "Authorization: Bearer $CI_KEY" -H "Content-Type: application/json" -d '{}')
if [ "$code" = "401" ]; then
    echo "   ❌ unexpected 401 from CI key on /external/builds"; exit 1
fi
echo "   ✅ accepted (status $code)"
echo ""

# ----------------------------------------------------------------------------
# 2. Login as admin → dp_* token (the canonical day-to-day flow)
# ----------------------------------------------------------------------------
echo "2. Login as admin and obtain a dp_* token"
LOGIN_RESPONSE=$(curl -s -X POST "$API_URL/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"admin123"}')
ADMIN_TOKEN=$(echo "$LOGIN_RESPONSE" | jq -r '.token // empty')
if [ -z "$ADMIN_TOKEN" ]; then
    echo "❌ Login failed: $LOGIN_RESPONSE"
    exit 1
fi
echo "✅ Token: ${ADMIN_TOKEN:0:12}..."
echo ""

# Convenience header for the rest of the script.
AUTH=(-H "Authorization: Bearer $ADMIN_TOKEN")

# ----------------------------------------------------------------------------
# 3. Build / deploy / list (as admin user — same path real ops will use)
# ----------------------------------------------------------------------------
echo "3. Register a build"
BUILD_RESPONSE=$(curl -s -X POST "$API_URL/api/v1/builds" "${AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d '{
        "app": "core-api",
        "source_repo": "app-core-api",
        "image_tag": "v1.0.0-'$(date +%s)'",
        "image_uri": "registry.example.com/core-api:v1.0.0",
        "commit_sha": "29791b8",
        "branch": "main",
        "built_by": "local-test"
    }')
echo "$BUILD_RESPONSE" | jq .
BUILD_ID=$(echo "$BUILD_RESPONSE" | jq -r '.data.id // empty')
if [ -z "$BUILD_ID" ]; then
    echo "❌ build registration failed"; exit 1
fi
echo "✅ Build ID: $BUILD_ID"
echo ""

echo "4. List builds"
curl -s "$API_URL/api/v1/builds?app=core-api&limit=5" "${AUTH[@]}" | jq '.data | length as $n | "got \($n) build(s)"'
echo ""

echo "5. Changelog between two commits"
curl -s "$API_URL/api/v1/changelog?repo=app-core-api&from=977638a&to=29791b8" "${AUTH[@]}" | jq '.data.commits | length as $n | "got \($n) commit(s)"'
echo ""

echo "6. Promote to staging"
curl -s -X POST "$API_URL/api/v1/deployments" "${AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"build_id\": $BUILD_ID, \"environment\": \"staging\"}" | jq '.data.id // .error'
echo ""

echo "7. List deployments"
curl -s "$API_URL/api/v1/deployments?app=core-api" "${AUTH[@]}" | jq '.data | length as $n | "got \($n) deployment(s)"'
echo ""

echo "8. List apps"
curl -s "$API_URL/api/v1/apps" "${AUTH[@]}" | jq '.data | length as $n | "got \($n) app(s)"'
echo ""

echo "9. Memory round-trip"
curl -s -X POST "$API_URL/api/v1/memory/app-core-api" "${AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d '{"content": "## smoke-test memory", "updated_by": "local-test"}' > /dev/null
curl -s "$API_URL/api/v1/memory/app-core-api" "${AUTH[@]}" | jq '.data.content // .error'
curl -s "$API_URL/api/v1/memory/app-core-api/versions" "${AUTH[@]}" | jq '.data | length as $n | "got \($n) version(s)"'
curl -s "$API_URL/api/v1/memory" "${AUTH[@]}" | jq '.data | length as $n | "got \($n) repo memory(ies)"'
echo ""

# ----------------------------------------------------------------------------
# 10. New auth-related endpoints / safeguards
# ----------------------------------------------------------------------------
echo "10. Self-downgrade must be blocked (400)"
ADMIN_ID=$(curl -s "$API_URL/api/v1/auth/me" "${AUTH[@]}" | jq -r '.id')
code=$(curl -s -o /tmp/deploy-api-selfdown.json -w '%{http_code}' \
    -X PUT "$API_URL/api/v1/auth/users/$ADMIN_ID" "${AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d '{"role": "deployer"}')
if [ "$code" != "400" ]; then
    echo "   ❌ expected 400, got $code: $(cat /tmp/deploy-api-selfdown.json)"; exit 1
fi
echo "   ✅ rejected: $(jq -r .error /tmp/deploy-api-selfdown.json)"
echo ""

echo "11. Change own password (round-trip back)"
NEW_PW="changed-$(date +%s)"
code=$(curl -s -o /tmp/deploy-api-pw.json -w '%{http_code}' \
    -X PUT "$API_URL/api/v1/auth/me/password" "${AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"current_password\": \"admin123\", \"new_password\": \"$NEW_PW\"}")
if [ "$code" != "200" ]; then
    echo "   ❌ change failed ($code): $(cat /tmp/deploy-api-pw.json)"; exit 1
fi
echo "   ✅ rotated"

# Rotate it back so subsequent runs of this script keep working.
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -X PUT "$API_URL/api/v1/auth/me/password" "${AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"current_password\": \"$NEW_PW\", \"new_password\": \"admin123\"}")
if [ "$code" != "200" ]; then
    echo "   ❌ revert failed ($code) — admin password is now '$NEW_PW'"; exit 1
fi
echo "   ✅ reverted to admin123 for next run"
echo ""

# ----------------------------------------------------------------------------
# 12. CI ingress smoke (the only place DEPLOY_API_KEY should be used)
# ----------------------------------------------------------------------------
echo "12. CI ingress: register a build via /external/builds with the CI key"
EXT_RESPONSE=$(curl -s -X POST "$API_URL/external/builds" \
    -H "Authorization: Bearer $CI_KEY" \
    -H "Content-Type: application/json" \
    -d '{
        "app": "core-api",
        "source_repo": "app-core-api",
        "image_tag": "ci-'$(date +%s)'",
        "image_uri": "registry.example.com/core-api:ci",
        "commit_sha": "29791b8",
        "branch": "main",
        "built_by": "ci-pipeline"
    }')
echo "$EXT_RESPONSE" | jq '.data.id // .error'
if ! echo "$EXT_RESPONSE" | jq -e '.data.id' > /dev/null 2>&1; then
    echo "   ❌ /external/builds with CI key failed"; exit 1
fi
echo "   ✅ build registered via CI key"
echo ""

echo "=== Test Complete ==="
