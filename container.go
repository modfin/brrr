package brrr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"os"
	"path/filepath"

	"github.com/docker/go-connections/nat"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/log"
	"github.com/testcontainers/testcontainers-go/wait"
)

type Config struct {
	// Database user for connecting to the database instances created from the template database.
	User string
	// Password for the database user.
	Password string
	// The name of the database which will be used as the template database.
	Database string

	// Image to use for the test container. Defaults to "postgres:17.2"
	Image string

	// MaxConnections to the database. Defaults to 1000.
	MaxConnections int

	// Path to migrations/seeding directory. Will ignore if empty.
	MigrationsPath string

	// Path to seeding directory. Will ignore if empty.
	SeedPath string

	// Seed func to run after migrations. Will ignore if empty.
	SeedFunc func(db *sql.DB, connStr string) error

	// Logger for logging the test container's output. Useful for debugging. Default to testcontainer's noopLogger
	Logger *slog.Logger

	host string
	port int
}

type Container struct {
	cfg       Config
	container testcontainers.Container
	pool      *pgxpool.Pool
}

// NewContainer launches a postgres test container and sets up the template database.
func NewContainer(cfg Config) (*Container, error) {
	return setup(context.Background(), cfg)
}

// NewInstance clones the template database to setup a database scoped to a single test
func (c *Container) NewInstance(ctx context.Context) (*DatabaseInstance, error) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire connection: %w", err)
	}
	defer conn.Release()

	name := c.cfg.Database + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	_, err = conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, c.cfg.Database))
	if err != nil {
		return nil, fmt.Errorf("failed to create database from template: %w", err)
	}

	instanceConn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", c.cfg.User, c.cfg.Password, c.cfg.host, c.cfg.port, name))
	if err != nil {
		return nil, err
	}

	return &DatabaseInstance{
		Connection: instanceConn,
		Name:       name,
	}, nil
}

type DatabaseInstance struct {
	// Connection to the database for the single test instance
	Connection *pgx.Conn

	// Name of the database for this single test instance
	Name string
}

// Close will close the connection to the database for the single test instance and drop the database
func (c *Container) CloseInstance(ctx context.Context, di *DatabaseInstance) error {
	err := di.Connection.Close(ctx)
	if err != nil {
		return fmt.Errorf("failed to close database connection: %w", err)
	}

	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", di.Name))
	return err
}

// Close will terminate the database and delete the test container image
func (c *Container) Close() error {
	return c.container.Terminate(context.Background())
}

func setup(ctx context.Context, cfg Config) (*Container, error) {
	db, err := setupPostgresTestContainer(ctx, cfg)
	if err != nil {
		return nil, err
	}

	port, err := db.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}

	cfg.host = "localhost"
	cfg.port = port.Int()

	pool, err := setupPgxPool(ctx, cfg)
	if err != nil {
		return nil, err
	}

	fmt.Println("Test container setup complete")

	if cfg.MigrationsPath != "" {
		fmt.Println("Starting migrations")
		if err := runMigrations(cfg, cfg.MigrationsPath); err != nil {
			return nil, err
		}
		fmt.Println("Database migrations complete")
	}

	if cfg.SeedPath != "" {
		fmt.Println("Starting seeding")
		if err := executeFiles(cfg, cfg.SeedPath); err != nil {
			return nil, err
		}
		fmt.Println("Database seeding complete")
	}

	if cfg.SeedFunc != nil {
		err = func() error {
			dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", cfg.host, cfg.port, cfg.User, cfg.Password, cfg.Database)
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				return fmt.Errorf("failed to open database connection: %w", err)
			}
			defer db.Close()
			if err = db.Ping(); err != nil {
				return fmt.Errorf("failed to ping database: %w", err)
			}

			connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.host, cfg.port, cfg.Database)
			return cfg.SeedFunc(db, connStr)
		}()
		if err != nil {
			return nil, err
		}
		fmt.Println("Database seed func complete")
	}

	c, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Release()

	if _, err := c.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s is_template=true", cfg.Database)); err != nil {
		return nil, err
	}

	fmt.Println("Database template setup complete")

	return &Container{
		cfg:       cfg,
		container: db,
		pool:      pool,
	}, nil
}

// runMigrations runs sql files from the specified path using go migrate file includings its file notations using sequences and up/down.
func runMigrations(cfg Config, path string) error {
	absPath := path
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		absPath = filepath.Join(wd, path)
	}

	fmt.Printf("Executing files from: %s\n", absPath)

	m, err := migrate.New("file://"+absPath, fmt.Sprintf("pgx5://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.host, cfg.port, cfg.Database))
	if err != nil {
		return err
	}

	if err = m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}

	sErr, mErr := m.Close()
	if sErr != nil {
		return sErr
	}
	if mErr != nil {
		return mErr
	}

	return nil
}

// executeFiles reads and executes SQL files from a directory, ordered by filename.
func executeFiles(cfg Config, path string) error {
	absPath := path
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		absPath = filepath.Join(wd, path)
	}

	fmt.Printf("Executing files from: %s\n", absPath)

	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", cfg.host, cfg.port, cfg.User, cfg.Password, cfg.Database)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("failed to open database connection: %w", err)
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	files, err := os.ReadDir(absPath)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	var sqlFiles []fs.DirEntry
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".sql") {
			sqlFiles = append(sqlFiles, file)
		}
	}

	// Sort by file name, ascending
	sort.Slice(sqlFiles, func(i, j int) bool {
		return sqlFiles[i].Name() < sqlFiles[j].Name()
	})

	for _, file := range sqlFiles {
		filePath := filepath.Join(absPath, file.Name())
		fmt.Printf("  -> Executing: %s\n", file.Name())

		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", file.Name(), err)
		}

		// If switching from pgx, make sure replacement has multi-statement support
		if _, err = db.Exec(string(content)); err != nil {
			return fmt.Errorf("failed to execute SQL in %s: %w", file.Name(), err)
		}
	}

	return nil
}

func setupPgxPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	conn := fmt.Sprintf("postgres://%s:%s@%s:%d/postgres?sslmode=disable", cfg.User, cfg.Password, cfg.host, cfg.port)

	conf, err := pgxpool.ParseConfig(conn)
	if err != nil {
		return nil, err
	}

	// Limit to 1 connection because of create database from template approach. Will fail if multiple connections, since template requires exclusive access when creating.
	conf.MaxConns = 1

	return pgxpool.NewWithConfig(ctx, conf)
}

func setupPostgresTestContainer(ctx context.Context, cfg Config) (testcontainers.Container, error) {
	port := "5432/tcp"

	img := "postgres:17.2"
	if cfg.Image != "" {
		img = cfg.Image
	}

	maxConnections := 1000
	if cfg.MaxConnections != 0 {
		maxConnections = cfg.MaxConnections
	}

	req := testcontainers.ContainerRequest{
		Image:        img,
		ExposedPorts: []string{port},
		Env: map[string]string{
			"POSTGRES_DB":       cfg.Database,
			"POSTGRES_PASSWORD": cfg.Password,
			"PGDATA":            "/var/lib/pg/data",
		},
		Cmd: []string{"postgres", "-c", fmt.Sprintf("max_connections=%d", maxConnections)},
		Tmpfs: map[string]string{
			"/var/lib/pg/data": "rw",
		},
		WaitingFor: wait.ForSQL(nat.Port(port), "pgx", func(host string, port nat.Port) string {
			return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, host, port.Int(), cfg.Database)
		}).WithStartupTimeout(10 * time.Second),
	}

	var logger log.Logger
	if cfg.Logger != nil {
		logger = &SlogAdapter{logger: cfg.Logger}
	}

	pgContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Logger:           logger,
		Started:          true,
	})
	if err != nil {
		return nil, err
	}

	return pgContainer, nil
}

type SlogAdapter struct {
	logger *slog.Logger
}

func (s *SlogAdapter) Printf(format string, v ...any) {
	s.logger.Info(fmt.Sprintf(format, v))
}
