package mysql

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"
)

type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
}

type Client struct {
	config *DBConfig
	db     *sql.DB
}

func GetDBConfigFromEnv() (*DBConfig, error) {
	config := &DBConfig{
		Host:     os.Getenv("DB_HOST"),
		Port:     os.Getenv("DB_PORT"),
		User:     os.Getenv("DB_USER"),
		Password: os.Getenv("DB_PASS"),
		DBName:   os.Getenv("DB_NAME"),
	}

	if config.Port == "" {
		config.Port = "3306"
	}

	if config.Host == "" {
		config.Host = "localhost"
	}

	if config.User == "" || config.Password == "" || config.DBName == "" {
		return nil, fmt.Errorf("database credentials (DB_USER, DB_PASS, DB_NAME) must be set in environment variables")
	}

	return config, nil
}

// NewMySQLClient creates a new MySQL client
func NewMySQLClient(config *DBConfig) (*Client, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true",
		config.User, config.Password, config.Host, config.Port, config.DBName)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %v", err)
	}

	return &Client{
		config: config,
		db:     db,
	}, nil
}

// Close closes the database connection
func (c *Client) Close() error {
	return c.db.Close()
}

// Query executes a query that returns rows
func (c *Client) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return c.db.Query(query, args...)
}

// Execute runs a query that doesn't return rows
func (c *Client) Execute(query string, args ...interface{}) (sql.Result, error) {
	return c.db.Exec(query, args...)
}
