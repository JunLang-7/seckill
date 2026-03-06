#!/usr/bin/env bash
# vegeta_attack.sh — seckill endpoint load test using vegeta
#
# Prerequisites:
#   • vegeta installed: go install github.com/tsenart/vegeta/v12@latest
#   • python3 in PATH (for target generation)
#   • Server running:  go run .   (or docker-compose up)
#
# Usage:
#   bash loadtest/vegeta_attack.sh [RATE] [DURATION] [PRODUCT_ID] [MAX_USERS]
#
# Defaults:
#   RATE        = 5000   (requests per second; target design goal)
#   DURATION    = 30s
#   PRODUCT_ID  = 1
#   MAX_USERS   = 500000 (user_id drawn from [1, MAX_USERS])
#
# Examples:
#   bash loadtest/vegeta_attack.sh               # 5000 rps, 30 s
#   bash loadtest/vegeta_attack.sh 10000 60s     # 10 000 rps, 60 s
#   bash loadtest/vegeta_attack.sh 1000 30s 2    # 1000 rps against product 2

set -euo pipefail

RATE="${1:-5000}"
DURATION="${2:-30s}"
PRODUCT_ID="${3:-1}"
MAX_USERS="${4:-500000}"
BASE_URL="http://localhost:8080"
SECKILL_URL="${BASE_URL}/api/v1/seckill/${PRODUCT_ID}"
OUT_DIR="loadtest/results"

# ── helpers ──────────────────────────────────────────────────────────────────

check_deps() {
    if ! command -v vegeta &>/dev/null; then
        echo "ERROR: vegeta not found."
        echo "Install: go install github.com/tsenart/vegeta/v12@latest"
        exit 1
    fi
    if ! command -v python3 &>/dev/null; then
        echo "ERROR: python3 required for target generation."
        exit 1
    fi
}

check_server() {
    echo "→ Checking server health at ${BASE_URL} ..."
    if ! curl -sf "${BASE_URL}/api/v1/products" -o /dev/null; then
        echo "ERROR: server not reachable at ${BASE_URL}"
        echo "Start with: go run ."
        exit 1
    fi
    echo "  Server is up."
}

init_stock() {
    echo "→ Seeding Redis stock for product ${PRODUCT_ID} ..."
    STATUS=$(curl -sf -o /dev/null -w "%{http_code}" \
        -X POST "${BASE_URL}/admin/seckill/${PRODUCT_ID}")
    if [[ "$STATUS" != "200" ]]; then
        echo "  WARNING: admin/seckill returned HTTP ${STATUS} (product may not exist)"
    else
        echo "  Stock seeded (HTTP 200)."
    fi
}

generate_targets() {
    # Each target is a POST with a random user_id body.
    # We pre-generate TOTAL_REQUESTS targets so vegeta can round-robin them.
    # TOTAL_REQUESTS = RATE * (numeric part of DURATION) * 2  (generous buffer)
    local rate_num="${RATE}"
    local dur_sec
    dur_sec=$(echo "${DURATION}" | grep -oE '^[0-9]+')
    local total=$(( rate_num * dur_sec * 2 ))
    total=$(( total > 200000 ? 200000 : total ))  # cap at 200 k to keep file small

    # All diagnostic messages go to stderr so only JSON targets reach stdout
    # (this function is called from inside a pipe: generate_targets | vegeta attack).
    echo "→ Generating ${total} unique targets (user_id ∈ [1, ${MAX_USERS}]) ..." >&2
    python3 - <<PYEOF
import base64, json, random, sys
total   = ${total}
max_uid = ${MAX_USERS}
url     = "${SECKILL_URL}"
# Stream one JSON target per line directly to stdout — avoids building a list
# in memory and prevents a broken-pipe error if vegeta exits early.
for _ in range(total):
    uid  = random.randint(1, max_uid)
    body = '{{"user_id":{}}}'.format(uid)
    sys.stdout.write(json.dumps({
        "method": "POST",
        "url":    url,
        "body":   base64.b64encode(body.encode()).decode(),
        "header": {"Content-Type": ["application/json"]}
    }) + "\n")
    try:
        sys.stdout.flush()
    except BrokenPipeError:
        # vegeta has consumed enough targets and closed the pipe — stop quietly.
        sys.exit(0)
PYEOF
}

run_attack() {
    mkdir -p "${OUT_DIR}"
    local timestamp
    timestamp=$(date +%Y%m%d_%H%M%S)
    local results_bin="${OUT_DIR}/attack_${timestamp}.bin"
    local report_txt="${OUT_DIR}/report_${timestamp}.txt"
    local hist_txt="${OUT_DIR}/histogram_${timestamp}.txt"

    echo "→ Attacking ${SECKILL_URL}"
    echo "  rate=${RATE} rps  duration=${DURATION}"
    echo ""

    generate_targets \
        | vegeta attack \
            -rate="${RATE}" \
            -duration="${DURATION}" \
            -format=json \
            -timeout=5s \
        | tee "${results_bin}" \
        | vegeta report \
            -type=text \
        | tee "${report_txt}"

    echo ""
    echo "── Latency histogram ────────────────────────────────────────────────"
    vegeta report \
        -type="hist[0,1ms,2ms,5ms,10ms,20ms,50ms,100ms,200ms,500ms]" \
        "${results_bin}" \
        | tee "${hist_txt}"

    echo ""
    echo "── Status code breakdown ─────────────────────────────────────────────"
    vegeta report -type=json "${results_bin}" \
        | python3 -c "
import json, sys
data = json.load(sys.stdin)
codes = data.get('status_codes', {})
total = data.get('requests', 0)
print(f'  Total requests : {total}')
print(f'  Throughput     : {data[\"throughput\"]:.1f} req/s  (success = 2xx/3xx)')
print()
for code, cnt in sorted(codes.items()):
    label = {
        '202': '202 Accepted    (order queued)',
        '409': '409 Conflict    (duplicate purchase)',
        '429': '429 Too Many    (sold out)',
        '503': '503 Unavailable (Redis/queue error)',
        '400': '400 Bad Request',
        '500': '500 Server Error',
    }.get(code, f'{code} Other')
    pct = 100 * cnt / total if total else 0
    print(f'  {label}: {cnt} ({pct:.1f}%)')
"

    echo ""
    echo "Results saved to:"
    echo "  Binary : ${results_bin}"
    echo "  Report : ${report_txt}"
    echo "  Histogram: ${hist_txt}"
}

# ── main ──────────────────────────────────────────────────────────────────────

echo "╔══════════════════════════════════════════╗"
echo "║  seckill vegeta load test                ║"
echo "╚══════════════════════════════════════════╝"

check_deps
check_server
init_stock
run_attack

echo ""
echo "Done."
