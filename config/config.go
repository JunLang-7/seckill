package config

import (
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server ServerConfig
	MySQL  MySQLConfig
	Redis  RedisConfig
	Queue  QueueConfig
}

type ServerConfig struct {
	Port           string
	RequestTimeout time.Duration
}

type MySQLConfig struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type QueueConfig struct {
	AmqpURL     string
	WorkerCount int
}

func Load() (*Config, error) {
	viper.SetDefault("SERVER_PORT", "8080")
	viper.SetDefault("REQUEST_TIMEOUT", "3s")
	viper.SetDefault("MYSQL_DSN", "root:password@tcp(127.0.0.1:3306)/seckill?charset=utf8mb4&parseTime=True&loc=Local")
	viper.SetDefault("MYSQL_MAX_OPEN_CONNS", 50)
	viper.SetDefault("MYSQL_MAX_IDLE_CONNS", 10)
	viper.SetDefault("MYSQL_CONN_MAX_LIFE_TIME", time.Hour*1)
	viper.SetDefault("REDIS_ADDR", "127.0.0.1:6379")
	viper.SetDefault("REDIS_PASSWORD", "")
	viper.SetDefault("REDIS_DB", 0)
	viper.SetDefault("QUEUE_AMQP_URL", "amqp://guest:guest@localhost:5672/")
	viper.SetDefault("QUEUE_WORKERS", 10)

	viper.AutomaticEnv()

	requestTimeout, err := time.ParseDuration(viper.GetString("REQUEST_TIMEOUT"))
	if err != nil {
		return nil, err
	}
	connMaxLifetime, err := time.ParseDuration(viper.GetString("MYSQL_CONN_MAX_LIFE_TIME"))
	if err != nil {
		return nil, err
	}

	return &Config{
		Server: ServerConfig{
			Port:           viper.GetString("SERVER_PORT"),
			RequestTimeout: requestTimeout,
		},
		MySQL: MySQLConfig{
			DSN:             viper.GetString("MYSQL_DSN"),
			MaxOpenConns:    viper.GetInt("MYSQL_MAX_OPEN_CONNS"),
			MaxIdleConns:    viper.GetInt("MYSQL_MAX_IDLE_CONNS"),
			ConnMaxLifetime: connMaxLifetime,
		},
		Redis: RedisConfig{
			Addr:     viper.GetString("REDIS_ADDR"),
			Password: viper.GetString("REDIS_PASSWORD"),
			DB:       viper.GetInt("REDIS_DB"),
		},
		Queue: QueueConfig{
			AmqpURL:     viper.GetString("QUEUE_AMQP_URL"),
			WorkerCount: viper.GetInt("QUEUE_WORKERS"),
		},
	}, nil
}
