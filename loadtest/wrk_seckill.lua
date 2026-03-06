-- wrk Lua script for seckill POST endpoint load test
--
-- Prerequisites:
--   1. Start server:        go run . (or docker-compose up)
--   2. Seed Redis stock:    curl -X POST http://localhost:8080/admin/seckill/1
--
-- Usage examples:
--   # 4 threads, 100 connections, 30 s
--   wrk -t4 -c100 -d30s -s loadtest/wrk_seckill.lua http://localhost:8080/api/v1/seckill/1
--
--   # 8 threads, 500 connections, 60 s (higher concurrency)
--   wrk -t8 -c500 -d60s -s loadtest/wrk_seckill.lua http://localhost:8080/api/v1/seckill/1
--
-- Each request uses a random user_id drawn from [1, MAX_USERS] so that most
-- requests are unique and exercise the full "buy" path rather than just the
-- dedup-rejection path.  Increase MAX_USERS if repeated user IDs become an issue
-- (e.g. when stock is large and many users succeed).

math.randomseed(os.time())

local MAX_USERS = 1000000

-- wrk calls request() once per HTTP request to build the request to send.
request = function()
    local uid = math.random(1, MAX_USERS)
    local body = string.format('{"user_id":%d}', uid)
    local headers = {}
    headers["Content-Type"] = "application/json"
    return wrk.format("POST", nil, headers, body)
end

-- wrk calls done() once after all threads finish; print a human-readable summary.
done = function(summary, latency, requests)
    local duration_s = summary.duration / 1e6   -- wrk reports duration in µs
    local qps        = summary.requests / duration_s
    io.write("\n╔══════════════════════════════════════╗\n")
    io.write("║      wrk Load Test Summary           ║\n")
    io.write("╚══════════════════════════════════════╝\n")
    io.write(string.format("Duration      : %.1f s\n",  duration_s))
    io.write(string.format("Total requests: %d\n",      summary.requests))
    io.write(string.format("QPS           : %.0f req/s\n", qps))
    io.write(string.format("Throughput    : %.2f MB/s\n",  summary.bytes / 1e6 / duration_s))
    io.write("\n-- Latency distribution --\n")
    io.write(string.format("  p50  : %.2f ms\n", latency:percentile(50)  / 1e3))
    io.write(string.format("  p90  : %.2f ms\n", latency:percentile(90)  / 1e3))
    io.write(string.format("  p99  : %.2f ms\n", latency:percentile(99)  / 1e3))
    io.write(string.format("  p99.9: %.2f ms\n", latency:percentile(99.9) / 1e3))
    io.write(string.format("  max  : %.2f ms\n", latency.max / 1e3))
    io.write("\n-- Errors --\n")
    io.write(string.format("  socket errors (connect): %d\n", summary.errors.connect))
    io.write(string.format("  socket errors (read)   : %d\n", summary.errors.read))
    io.write(string.format("  socket errors (write)  : %d\n", summary.errors.write))
    io.write(string.format("  socket errors (timeout): %d\n", summary.errors.timeout))
    io.write(string.format("  HTTP non-2xx/3xx       : %d\n", summary.errors.status))
    io.write("\nNote: 429 (sold-out) and 409 (duplicate) are counted in 'HTTP non-2xx'\n")
    io.write("      but are EXPECTED application responses, not errors.\n")
end
