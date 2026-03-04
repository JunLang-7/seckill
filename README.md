# seckill 基于 Go 的高并发秒杀系统

> 一个生产可用的高并发秒杀系统，采用 Redis 原子扣减 + Channel 异步削峰 + 双重防超卖机制，单机可抗万级 QPS。

## 技术栈

| 层级 | 技术 |
|------|------|
| Web 框架 | [Gin](https://github.com/gin-gonic/gin) v1.9.1 |
| ORM | [GORM](https://gorm.io) v1.31.1 + MySQL 驱动 v1.6.0 |
| 缓存 | [go-redis/v9](https://github.com/redis/go-redis) v9.18.0 |
| 消息队列 | Go 原生 `channel`（可替换 RabbitMQ） |
| 配置管理 | [Viper](https://github.com/spf13/viper) v1.21.0 |
| 数据库 | MySQL 8 |
| Go 版本 | 1.25.6 |

---

## 核心设计亮点

### 1. Redis 原子扣减 — 彻底杜绝超卖

摒弃传统悲观锁方案，使用 Redis `DECR` 进行毫秒级前置拦截。每次请求到达时，Redis 原子地将库存减 1，无需加锁，天然支持万级并发读写。

```
Redis DECR seckill:stock:{id}
  ├─ result >= 0  → 秒杀成功，进入队列
  └─ result <  0  → 立即返回 429，INCR 回滚，库存精准归零
```

同时通过 `SETNX seckill:bought:{product_id}:{user_id}` 实现幂等性校验，同一用户对同一商品只能购买一次。

### 2. 异步削峰 — Channel 消息队列

Gin 接口在完成 Redis 扣减后立即返回 `202 Accepted`，**不等待数据库写入**。实际的订单落库由后台 Worker Goroutine Pool 异步完成，彻底隔离峰值流量对 MySQL 的冲击。

```
HTTP 请求 → Redis DECR → channel.Push() → 立即返回 202
                                ↓
                    [Worker Pool × 10 goroutines]
                                ↓
                    MySQL 原子事务写入（异步）
```

### 3. 优雅降级 — Redis 宕机硬阻断

Redis 出错时，系统**不会透传到 MySQL**，而是立即返回 `503 Service Unavailable`，防止雪崩效应：

```go
remaining, err := rdb.Decr(ctx, stockKey).Result()
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

系统采用三步有序停机，确保内存 Channel 中积压的订单在进程退出前**全部落库**，杜绝"用户付了钱、进程重启后找不到订单"的问题。

**错误做法（已修复前）：** Worker 监听 `ctx.Done()`，SIGTERM 一到立刻退出，Channel 里的待处理订单随进程消失。

**三步停机顺序：**

```
① srv.Shutdown()        关闭 HTTP Server，停止接收新请求
         ↓
② close(orderDeque.Ch)  关闭 Channel，告知 Worker 不再有新订单
         ↓
③ wg.Wait()             死等全部 Worker 把 Channel 消费清空后，进程才退出
```

Worker 使用 `for msg := range ch` 消费队列，`range` 在 Channel 关闭且清空后自动退出，无需外部 Context 干预。

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
| Redis 层 | `DECR` 原子操作 | 毫秒级拦截超卖，承载峰值流量 |
| MySQL 层 | `UPDATE ... WHERE stock > 0` | 最后防线，RowsAffected=0 时 ROLLBACK |
| 唯一索引 | `UNIQUE(user_id, product_id)` | 防止极端情况下的重复落库 |

---

## 项目结构

```
mkt-system/
├── main.go                      # 程序入口：依赖注入、启动 Server 与 Worker Pool
├── config/
│   └── config.go                # 配置结构体 + Viper 环境变量加载
├── handler/
│   ├── product.go               # 商品 CRUD + 管理员初始化 Redis 库存
│   └── seckill.go               # 秒杀接口（快速路径，返回 202）+ 订单查询
├── model/
│   ├── product.go               # Product GORM 模型
│   └── seckill.go               # SeckillRecord 模型 + 状态常量
├── queue/
│   └── queue.go                 # 有界 Channel 队列 + 非阻塞 Push
├── repo/
│   ├── product.go               # 商品查询 + Redis 库存初始化
│   └── seckill.go               # 原子事务：UPDATE stock + INSERT record
├── router/
│   └── router.go                # Gin 路由注册
├── service/
│   └── seckill_service.go       # 秒杀核心逻辑：Redis DECR + 回滚纪律
├── worker/
│   └── worker.go                # Worker Pool：range 消费队列 → 写入 DB，channel 关闭后自动退出
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
| `429 Too Many Requests` | 库存售罄 |
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
| `QUEUE_BUFFER_SIZE` | `10000` | Channel 队列容量 |
| `QUEUE_WORKER_COUNT` | `10` | Worker Goroutine 数量 |

---

## 快速开始

### 前置依赖

- Go 1.21+
- Docker

### 1. 启动依赖服务

```bash
docker run -d \
  --name mkt-mysql \
  -p 3307:3306 \
  -e MYSQL_ROOT_PASSWORD=password \
  -e MYSQL_DATABASE=seckill \
  mysql:8

docker run -d \
  --name mkt-redis \
  -p 6379:6379 \
  redis:latest

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

## 停止服务

```bash
# 停止服务器（Ctrl+C 或 kill）
kill $(lsof -ti:8080) 2>/dev/null && echo "server stopped"

# 停止并删除 Docker 容器
docker rm -f mkt-mysql mkt-redis && echo "containers removed"
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
