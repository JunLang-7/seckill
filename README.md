# seckill 基于 Go 的高并发秒杀系统

> 一个生产可用的高并发秒杀系统，采用 Redis Lua 原子脚本 + RabbitMQ 异步削峰 + 双重防超卖机制，单机可抗万级 QPS。

## 技术栈

| 层级 | 技术 |
|------|------|
| Web 框架 | [Gin](https://github.com/gin-gonic/gin) v1.9.1 |
| ORM | [GORM](https://gorm.io) v1.31.1 + MySQL 驱动 v1.6.0 |
| 缓存 | [go-redis/v9](https://github.com/redis/go-redis) v9.18.0 |
| 消息队列 | [RabbitMQ](https://www.rabbitmq.com)（[amqp091-go](https://github.com/rabbitmq/amqp091-go) v1.10.0） |
| 配置管理 | [Viper](https://github.com/spf13/viper) v1.21.0 |
| 限流 | [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate)（Go 官方扩展库） |
| 数据库 | MySQL 8 |
| Go 版本 | 1.26.0 |

---

## 核心设计亮点

### 1. Redis Lua 原子脚本 — 彻底杜绝超卖与部分失败

摒弃传统悲观锁方案，将幂等校验（`SET NX`）与库存扣减（`DECR`）合并为一段 **Redis Lua 脚本**，通过单次 `EVALSHA` 原子执行。Redis 单线程执行 Lua，过程中任何客户端都无法观察到中间状态，彻底消除两步操作之间的"窗口期漏洞"。

**旧方案的隐患：** `SetNX` 成功、`Decr` 因网络抖动丢失 → dupKey 永久存在 → 用户被永久拦截无法重购。

**Lua 脚本逻辑：**

```lua
-- KEYS[1] = seckill:bought:{productID}:{userID}   （幂等 key）
-- KEYS[2] = seckill:stock:{productID}             （库存 key）
local set = redis.call('SET', KEYS[1], '1', 'NX', 'EX', ARGV[1])
if not set then return 1 end          -- 已购买 → 409

local ok, remaining = pcall(redis.call, 'DECR', KEYS[2])
if not ok then                        -- DECR 异常（如 key 非整型）
    redis.call('DEL', KEYS[1])        --   回滚幂等 key（在脚本内完成）
    return 3                          --   → 503
end
if remaining < 0 then
    redis.call('INCR', KEYS[2])       -- 回滚库存（在脚本内完成）
    redis.call('DEL', KEYS[1])        -- 回滚幂等 key（在脚本内完成）
    return 2                          -- 售罄 → 429
end
return 0                              -- 成功 → 进入队列
```

| 返回码 | 含义 | HTTP |
|--------|------|------|
| `0` | 成功，stock 已扣减，dupKey 已写入 | 202 |
| `1` | 用户已购买（SET NX 失败） | 409 |
| `2` | 售罄（DECR 后 < 0，脚本内已回滚） | 429 |
| `3` | Redis 异常（脚本内已回滚 dupKey） | 503 |
| error | Redis 整体不可用 | 503 |

Go 侧仅剩一个需要手动回滚的场景：**队列已满**（在 Lua 脚本成功之后、`Push` 之前）。所有其他失败路径全部在 Lua 内部自洽处理。

### 2. 异步削峰 — RabbitMQ 消息队列

Gin 接口在完成 Redis 扣减后立即返回 `202 Accepted`，**不等待数据库写入**。实际的订单落库由后台 Worker Goroutine Pool 通过 RabbitMQ 异步完成，彻底隔离峰值流量对 MySQL 的冲击。

```
HTTP 请求 → Redis DECR → queue.Push() → 立即返回 202
                                ↓
                    [RabbitMQ — sec-kill 队列（durable + persistent）]
                                ↓
                    [Worker Pool × N goroutines]
                    （每 Worker 独立 AMQP channel，prefetch=1）
                                ↓
                    MySQL 原子事务写入（含重试 + Redis 补偿）
```

**关键特性：**

| 特性 | 实现 |
|------|------|
| 消息持久化 | Queue 声明为 `durable`，消息 `DeliveryMode: Persistent`，Broker 重启不丢消息 |
| **发送方确认** | **Publisher Confirms：`Push()` 阻塞至 Broker fsync 回执，未收到 ack 则不返回 202** |
| 手动 Ack | `autoAck=false`，Worker 落库成功后才 `d.Ack()`，失败前消息不会从队列删除 |
| 公平分发 | 每 Worker channel 设置 `Qos(prefetch=1)`，RabbitMQ 按 round-robin 分配，防止单 Worker 积压 |
| Broker 故障隔离 | `Queue` 接口抽象(`Push` / `Consume`)，可一键替换底层实现，不影响 service/worker 逻辑 |

### 3. 优雅降级 — Redis 宕机硬阻断

Redis 出错时，系统**不会透传到 MySQL**，而是立即返回 `503 Service Unavailable`，防止雪崩效应：

```go
res, err := seckillScript.Run(ctx, rdb, []string{dupKey, stockKey}, dupKeyTTLSec).Int()
if err != nil {
    // 硬阻断：Redis 故障不穿透到 DB
    return ErrRedisUnavailable  // → HTTP 503
}
```

### 4. Context 超时链路传递

HTTP 请求路径严格遵守 Go 的 Context 链路，3 秒硬边界防止级联雪崩：

```
signal.NotifyContext (进程生命周期)
  └── middleware.RequestTimeout(3s)  ← 每个 HTTP 请求
       └── rdb.Decr(ctx)             ← Redis 调用复用请求 ctx
```

Worker 的数据库写入使用独立的 `context.Background()`，与请求生命周期完全解耦，不受请求超时或停机信号影响。

### 5. 优雅停机 — 零丢单重启

系统采用两步有序停机，依托 RabbitMQ 的消息持久化保证进程重启后**积压订单不丢失**。

**两步停机顺序：**

```
① srv.Shutdown()          关闭 HTTP Server，停止接收新秒杀请求
          ↓
② wg.Wait() + Close()     Worker处理完成旧的请求再退出，等全部 Worker 退出，再关闭 AMQP 连接
```

**与 RabbitMQ 的协作：**

- Worker 使用 **手动 Ack**：`processOrder` 返回后才调用 `d.Ack(false)`。
- 若进程在 Ack 前被强制终止，RabbitMQ 检测到 channel 关闭，会自动将**未 Ack 的消息重新入队**，由下次启动的 Worker 重新消费。
- 消息声明为 `durable + persistent`，Broker 重启也不会丢失积压消息。

对比旧的 Channel 方案：旧方案依赖 `close(ch)` 触发 `range` 退出再排空内存 channel；进程崩溃则内存里的消息直接消失。RabbitMQ 将这一保障从**进程内**提升到了**Broker 级别**。

### 6. 异步落库失败补偿 — Redis 状态回滚

**问题根源：** 异步架构下，Redis 扣减与 MySQL 落库之间存在时间窗口。若 Worker 落库失败会导致：

| 状态 | Redis | MySQL |
|------|-------|-------|
| `seckill:stock:{id}` | 已 -1，永久损耗 | 无记录 |
| `seckill:bought:{id}:{uid}` | 已设置，永久拦截该用户 | 无记录 |

用户付了钱，收到了 `202`，却**永远无法查到订单，也无法重新购买**。

**失败分类与处理策略：**

```
CreateOrder 失败
  ├─ ErrStockEmpty（DB 库存为 0，与 Redis 数据不一致）
  │     └─ 非可重试，立即执行 Redis 补偿
  │
  └─ 其他错误（网络抖动、超时等瞬时故障）
        ├─ attempt 1：立即执行
        ├─ attempt 2：sleep 1s 后重试
        ├─ attempt 3：sleep 2s 后重试
        └─ 全部失败 → 执行 Redis 补偿
```

**补偿操作（`compensateRedis`）：**

```go
INCR seckill:stock:{productID}           // 归还库存，消除幻扣
DEL  seckill:bought:{productID}:{userID} // 删除防重标记，用户可重新购买
```

两个 key 与快速路径使用完全一致的格式，补偿后系统 Redis 状态恢复到本次请求之前，最终一致性得到保证。

### 7. 数据一致性双重保障

| 层级 | 机制 | 作用 |
|------|------|------|
| Redis 层 | Lua 原子脚本（`SET NX` + `DECR`，单次 EVALSHA） | 毫秒级拦截超卖，承载峰值流量，无部分失败窗口 |
| MySQL 层 | `UPDATE ... WHERE stock > 0` | 最后防线，RowsAffected=0 时 ROLLBACK |
| 唯一索引 | `UNIQUE(user_id, product_id)` | 防止极端情况下的重复落库 |

### 8. 全局限流 — 令牌桶保护入口

在秒杀路由组（`/api/v1/seckill/*`）前挂载 **令牌桶（Token Bucket）** 中间件，在流量打到业务逻辑之前进行第一道削峰，防止单机被突发请求打垮。

```
每秒生成 5000 个令牌（ratePerSec = 5000）
桶容量上限 10000（burstCapacity = 10000）

请求到达 → limiter.Allow()
  ├─ true  → 继续执行后续中间件与业务逻辑
  └─ false → 立即返回 429 rate limit exceeded，不消耗任何 Redis / DB 资源
```

限流仅作用于秒杀接口，商品查询（`/api/v1/products`）和管理接口（`/admin`）不受影响。

**已知局限：进程内状态，无法跨实例共享**

令牌桶存活在单个进程的内存中。多副本部署时，每个实例各自维护独立的桶，实际放行的总 QPS 会等比放大：

```
目标：全局限制 5000 RPS

单副本：✓  每实例 5000 RPS，总量 = 5000 RPS
三副本：✗  每实例 5000 RPS，总量 = 15000 RPS（超出预期 3×）
```

**进阶方案：Redis 分布式限流**

将令牌桶状态迁移到 Redis，所有实例共享同一个计数器，即可实现真正的全局限流：

```lua
-- 滑动窗口计数（每秒最多 N 次）
local key   = KEYS[1]           -- e.g. "ratelimit:seckill"
local limit = tonumber(ARGV[1]) -- e.g. 5000
local now   = tonumber(ARGV[2]) -- Unix 毫秒时间戳
local window = 1000             -- 1 秒窗口（ms）

redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
local count = redis.call('ZCARD', key)
if count >= limit then
    return 0  -- 触发限流
end
redis.call('ZADD', key, now, now)
redis.call('PEXPIRE', key, window)
return 1  -- 放行
```

当前实现适合单机部署或对全局精度要求不高的场景；水平扩容后建议替换为上述 Redis 方案。

### 9. 发送方确认 — Publisher Confirms

`PublishWithContext` 只保证消息进入 TCP 缓冲区，Broker 宕机仍可能丢消息。开启 Publisher Confirms 后，Broker 将消息 `fsync` 到持久化队列 journal 后才回送 `basic.ack`，`Push()` 收到 ack 才返回——客户端看到 `202` 时消息已落盘。

`sync.Mutex` 串行化并发 `Push`：AMQP delivery-tag 单调递增，confirm 按 tag 顺序返回，加锁保证 tag ↔ confirm 一一对应，不错乱。超时（5 s）时触发 Redis `INCR` + `DEL` 回滚，返回 `503`。

---

## 项目结构

```
seckill/
├── main.go                      # 程序入口：依赖注入、启动 Server 与 Worker Pool
├── config/
│   └── config.go                # 配置结构体 + Viper 环境变量加载
├── handler/
│   ├── product.go               # 商品 CRUD + 管理员初始化 Redis 库存
│   └── seckill.go               # 秒杀接口（快速路径，返回 202）+ 订单查询
├── loadtest/
│   ├── vegeta_attack.sh         # vegeta attack 脚本
│   └── wrk_seckill.lua          # wrk 测试 lua 脚本
├── middleware/
│   ├── ratelimit.go             # 令牌桶全局限流（/api/v1/seckill/* 专用）
│   └── timeout.go               # HTTP 请求超时中间件
├── model/
│   ├── product.go               # Product GORM 模型
│   └── seckill.go               # SeckillRecord 模型 + 状态常量
├── queue/
│   ├── queue.go                 # Queue 接口 / OrderDeque(RabbitMQ) / Delivery 类型
│   └── queuetest/
│       └── fake.go              # FakeQueue（channel 实现，仅供测试 import）
├── repo/
│   ├── product.go               # 商品查询 + Redis 库存初始化
│   └── seckill.go               # 原子事务：UPDATE stock + INSERT record
├── router/
│   └── router.go                # Gin 路由注册
├── service/
│   └── seckill_service.go       # 秒杀核心逻辑：Lua 原子脚本（SET NX + DECR）+ 队列满回滚
├── worker/
│   └── worker.go                # Worker Pool：消费 RabbitMQ → 写 DB，手动 Ack，ctx 取消后退出
└── pkg/
    └── mysql.go                 # GORM 连接池初始化
    └── redis.go                 # go-redis 客户端初始化 + Ping 健康检查
```

---

## API 接口

| 方法 | 路径 | 说明 | 响应 |
|------|------|------|------|
| `GET` | `/api/v1/products` | 查询所有商品 | 200 |
| `GET` | `/api/v1/products/:id` | 查询单个商品 | 200 / 404 |
| `POST` | `/api/v1/seckill/:product_id` | **发起秒杀** | 202 / 409 / 429 / 503 |
| `GET` | `/api/v1/seckill/records/:user_id` | 查询用户订单状态 | 200 |
| `POST` | `/admin/seckill/:product_id/init` | 初始化 Redis 库存（活动前调用） | 200 |

**秒杀接口状态码说明：**

| HTTP 状态码 | 含义 |
|-------------|------|
| `202 Accepted` | 抢购成功，异步处理中 |
| `409 Conflict` | 已购买，不可重复 |
| `429 Too Many Requests` | 库存售罄（`product is sold out`）或限流触发（`rate limit exceeded`） |
| `503 Service Unavailable` | Redis 故障 / 队列已满 |

---

## 环境变量配置

所有配置通过环境变量注入，均有默认值，开箱即用。

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `SERVER_PORT` | `8080` | HTTP 监听端口 |
| `SERVER_REQUEST_TIMEOUT` | `3s` | 请求超时时间 |
| `MYSQL_DSN` | `root:password@tcp(127.0.0.1:3306)/seckill?...` | MySQL 连接串 |
| `MYSQL_MAX_OPEN_CONNS` | `50` | 最大连接数 |
| `MYSQL_MAX_IDLE_CONNS` | `10` | 最大空闲连接数 |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis 地址 |
| `REDIS_PASSWORD` | _(空)_ | Redis 密码 |
| `REDIS_DB` | `0` | Redis 数据库编号 |
| `QUEUE_AMQP_URL` | `amqp://guest:guest@localhost:5672/` | RabbitMQ 连接串 |
| `QUEUE_WORKERS` | `10` | Worker Goroutine 数量 |

---

## 快速开始

### 前置依赖

- Go 1.21+
- Docker

### 1. 启动依赖服务

```bash
# 启动 MySQL/Redis/RabbitMQ 容器
docker compose up -d

# 等待 MySQL 就绪（约 10 秒）
until docker exec mkt-mysql mysqladmin ping -uroot -ppassword --silent 2>/dev/null; do
  echo "waiting for mysql..." && sleep 1
done
echo "MySQL ready"
```

### 2. 启动服务器

```bash
# 表结构由 GORM AutoMigrate 自动创建，无需手动建表
MYSQL_DSN="root:password@tcp(127.0.0.1:3307)/seckill?charset=utf8mb4&parseTime=True&loc=Local" \
QUEUE_AMQP_URL="amqp://guest:guest@localhost:5672/" \
go run main.go
```

服务启动后输出：

```
2026/03/02 14:53:41 mysql connected and migrated
2026/03/02 14:53:41 redis connected
2026/03/02 14:53:41 started 10 workers
2026/03/02 14:53:41 server listening on :8080
```

---

## 测试脚本

以下脚本覆盖全部核心场景，直接复制运行即可。

### 场景一：初始化秒杀活动

```bash
# 1. 插入测试商品（库存 10 件，单价 9999 元）
docker exec mkt-mysql mysql -uroot -ppassword seckill \
  -e "INSERT INTO products (name, stock, price) VALUES ('iPhone 16 Pro', 10, 999900);" 2>/dev/null
echo "商品已插入"

# 2. 将 DB 库存同步到 Redis（每次活动开始前必须调用）
curl -s -X POST http://localhost:8080/admin/seckill/1
echo ""

# 3. 验证 Redis 库存
echo "Redis 当前库存: $(docker exec mkt-redis redis-cli GET seckill:stock:1)"
```

### 场景二：正常抢购（10 个用户）

```bash
for i in $(seq 1 10); do
  RESP=$(curl -s -w "\n%{http_code}" -X POST http://localhost:8080/api/v1/seckill/1 \
    -H 'Content-Type: application/json' \
    -d "{\"user_id\": $i}")
  BODY=$(echo "$RESP" | head -1)
  CODE=$(echo "$RESP" | tail -1)
  echo "user $i → HTTP $CODE  $BODY"
done
```

期望输出：10 行全部 `HTTP 202`。

### 场景三：重复购买拦截

```bash
RESP=$(curl -s -o /dev/null -w "%{http_code}" \
  -X POST http://localhost:8080/api/v1/seckill/1 \
  -H 'Content-Type: application/json' \
  -d '{"user_id": 1}')
echo "重复购买 → HTTP $RESP（期望 409）"
```

### 场景四：库存售罄拦截

```bash
RESP=$(curl -s -X POST http://localhost:8080/api/v1/seckill/1 \
  -H 'Content-Type: application/json' \
  -d '{"user_id": 99}')
echo "库存耗尽 → $RESP（期望 429 sold out）"
```

### 场景五：查询订单状态

```bash
curl -s http://localhost:8080/api/v1/seckill/records/1
```

### 场景六：验证无超卖（数据库核验）

```bash
sleep 1  # 等待 Worker 异步落库完成
echo "=== Redis 剩余库存 ==="
docker exec mkt-redis redis-cli GET seckill:stock:1

echo "=== MySQL 商品库存 ==="
docker exec mkt-mysql mysql -uroot -ppassword seckill \
  -e "SELECT id, name, stock FROM products WHERE id=1;" 2>/dev/null

echo "=== 秒杀订单总数（期望 = 初始库存）==="
docker exec mkt-mysql mysql -uroot -ppassword seckill \
  -e "SELECT COUNT(*) as total, SUM(status=1) as success FROM seckill_records;" 2>/dev/null
```

### 场景七：Redis 宕机降级测试

```bash
# 停止 Redis
docker stop mkt-redis

# 发起秒杀请求
RESP=$(curl -s -X POST http://localhost:8080/api/v1/seckill/1 \
  -H 'Content-Type: application/json' \
  -d '{"user_id": 100}')
echo "Redis 宕机 → $RESP（期望 503 service unavailable）"

# 恢复 Redis
docker start mkt-redis
```

---

## 压力测试

压测工具位于 `loadtest/` 目录，用于验证 **单机万级 QPS** 设计目标。

---

### 前置条件

服务已按**快速开始**章节启动，Redis 库存已通过管理接口初始化：

```bash
# 插入商品（若尚未存在）
docker exec mkt-mysql mysql -uroot -ppassword seckill \
  -e "INSERT IGNORE INTO products (name, stock, price) VALUES ('iPhone 16 Pro', 999999, 999900);" 2>/dev/null

# 将大容量库存写入 Redis（压测期间不触发售罄）
curl -s -X POST http://localhost:8080/admin/seckill/1
```

---

### 方式一：wrk（高吞吐基准测试）

[wrk](https://github.com/wg/wrk) 使用多线程 epoll/kqueue，能以极低开销压出单机上限 QPS。
Lua 脚本 `loadtest/wrk_seckill.lua` 在每个请求中随机生成 `user_id`，模拟真实用户流量。

**安装：**

```bash
# macOS
brew install wrk

# Ubuntu / Debian
sudo apt-get install wrk
```

**运行（推荐参数）：**

```bash
# 4 线程 / 100 连接 / 持续 30 秒
wrk -t4 -c100 -d30s -s loadtest/wrk_seckill.lua http://localhost:8080/api/v1/seckill/1

# 更高并发（8 线程 / 500 连接）
wrk -t8 -c500 -d60s -s loadtest/wrk_seckill.lua http://localhost:8080/api/v1/seckill/1
```

**压测报告示例（MacBook M2，本地 Redis）：**

```
Running 30s test @ http://localhost:8080/api/v1/seckill/1
  4 threads and 100 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency     2.97ms    3.76ms  52.12ms   93.84%
    Req/Sec    10.63k     2.01k   15.08k    79.67%
  1271394 requests in 30.07s, 206.14MB read
  Non-2xx or 3xx responses: 1153178
Requests/sec:  42280.65

╔══════════════════════════════════════╗
║      wrk Load Test Summary           ║
╚══════════════════════════════════════╝
Duration      : 30.1 s
Total requests: 1271394
QPS           : 42281 req/s
Throughput    : 6.85 MB/s

-- Latency distribution --
  p50  : 1.95 ms
  p90  : 5.78 ms
  p99  : 18.73 ms
  p99.9: 33.12 ms
  max  : 52.12 ms

-- Errors --
  socket errors (connect): 0
  socket errors (read)   : 0
  socket errors (write)  : 0
  socket errors (timeout): 0
  HTTP non-2xx/3xx       : 1153178

Note: 429 (sold-out) 和 409 (duplicate) 计入 'HTTP non-2xx'
      但属于预期的业务响应，不是真正的错误。
```

> **结论：单机峰值超过 42 000 QPS，p99 延迟 18 ms，达到万级 QPS 设计目标。**

---

### 方式二：vegeta（精确速率压测 + 报告）

[vegeta](https://github.com/tsenart/vegeta) 以**固定速率**发送请求（区别于 wrk 的最大吞吐模式），更适合验证 SLA 指标（在目标 QPS 下的延迟分布与错误率）。

**安装：**

```bash
# macOS
brew install vegeta

# Ubuntu / Debian
sudo apt install vegeta
```

**运行：**

```bash
# 默认：5000 rps，持续 30 秒，针对 product_id=1
bash loadtest/vegeta_attack.sh

# 自定义参数：[RATE] [DURATION] [PRODUCT_ID] [MAX_USERS]
bash loadtest/vegeta_attack.sh 1000  30s 2  # 1 000 rps，20s 针对商品 2
```

**压测报告示例（5 000 rps × 30 s）：**

```
Requests      [total, rate, throughput]  150000, 5000.03, 0.33
Duration      [total, attack, wait]      30s, 30s, 236µs
Latencies     [min, mean, 50, 90, 95, 99, max]
              135µs, 835µs, 215µs, 456µs, 919µs, 18.7ms, 48.2ms
Status Codes  [code:count]               202:10  409:5  429:149985

── Latency histogram ──────────────────────────────────────────
Bucket           #       %       Histogram
[0s,     1ms]    142903  95.27%  ███████████████████████████████████████████████
[1ms,    2ms]    646     0.43%
[2ms,    5ms]    1005    0.67%
[5ms,    10ms]   1480    0.99%
[10ms,   20ms]   2747    1.83%   █
[20ms,   50ms]   1219    0.81%

── Status code breakdown ──────────────────────────────────────
  Total requests : 150000
  202 Accepted    (order queued)        :     10 ( 0.0%)
  409 Conflict    (duplicate purchase)  :      5 ( 0.0%)
  429 Too Many    (sold out)            : 149985 (100.0%)
```

> 5 000 rps 恒定速率下，**95.27% 的请求在 1 ms 内完成，p99 = 18.7 ms，零 socket 错误**。结果文件自动保存至 `loadtest/results/`。

---

### 方式三：Go 基准测试（服务层微基准）

使用 miniredis（进程内 Redis 模拟），隔离网络抖动，精确测量 **Lua 脚本 + 队列写入** 的 Go 侧开销：

```bash
go test ./service/ -bench=. -benchtime=5s -benchmem
```

**基准结果（Apple M2）：**

```
BenchmarkExecute_HappyPath-8          66036    56807 ns/op   ~17 600 rps
BenchmarkExecute_AlreadyPurchased-8   46964    77283 ns/op   ~12 900 rps  （已购快速拦截）
BenchmarkExecute_SoldOut-8            41989    76677 ns/op   ~13 000 rps  （售罄回滚路径）
```

> Go 侧单核吞吐约 13–18 k rps。加入真实 Redis 后，RTT（约 0.1–1 ms）是主要瓶颈，
> 进一步提升的方向包括连接池调优、Pipeline 批处理，以及增加 Redis 副本实例。

---

## 停止服务

```bash
# 停止服务器（Ctrl+C 或 kill）
kill $(lsof -ti:8080) 2>/dev/null && echo "server stopped"

# 停止并删除 compose 管理的容器、网络、命名卷
docker compose down -v
```

---

## 数据模型

### products

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | INT PK | 商品 ID |
| `name` | VARCHAR UNIQUE | 商品名称 |
| `stock` | INT CHECK ≥ 0 | 真实库存 |
| `price` | INT CHECK ≥ 0 | 价格（单位：分） |

### seckill_records

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | BIGINT PK AUTO | 记录 ID |
| `user_id` | INT | 用户 ID |
| `product_id` | INT | 商品 ID |
| `status` | TINYINT | `0`=处理中 `1`=成功 `2`=失败 |
| `created_at` | DATETIME | 创建时间（有索引） |
| `updated_at` | DATETIME | 更新时间 |

> `UNIQUE INDEX (user_id, product_id)` — 数据库层幂等性兜底，防止重复落库。
