package mysql

import (
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gtrindade/ultra-kiew/internal/config"
)

type Client struct {
	config   *config.Config
	dndTools *sql.DB
	srd      *sql.DB
}

func getDBConnection(dbConfig *config.DBConfig) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true",
		dbConfig.User, dbConfig.Password, dbConfig.Host, dbConfig.Port, dbConfig.Name)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	fmt.Printf("Successfully connected to the database %s\n", dbConfig.Name)

	return db, nil
}

// NewMySQLClient creates a new MySQL client
func NewMySQLClient(config *config.Config) (*Client, error) {
	dndTools, err := getDBConnection(config.DNDTools)
	if err != nil {
		return nil, err
	}

	srd, err := getDBConnection(config.SRD)
	if err != nil {
		return nil, err
	}

	return &Client{
		config:   config,
		dndTools: dndTools,
		srd:      srd,
	}, nil
}

// Close closes the database connection
func (c *Client) Close() error {
	err := c.dndTools.Close()
	if err != nil {
		return err
	}
	return c.srd.Close()
}
