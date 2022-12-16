package itest

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/breez/lntest"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/jackc/pgx/v4/pgxpool"
)

type PostgresContainer struct {
	id            string
	password      string
	port          uint32
	cli           *client.Client
	logfile       string
	isInitialized bool
	isStarted     bool
	mtx           sync.Mutex
}

func NewPostgresContainer(logfile string) (*PostgresContainer, error) {
	port, err := lntest.GetPort()
	if err != nil {
		return nil, fmt.Errorf("could not get port: %w", err)
	}

	return &PostgresContainer{
		password: "pgpassword",
		port:     port,
	}, nil
}

func (c *PostgresContainer) Start(ctx context.Context) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	var err error
	if c.isStarted {
		return nil
	}

	c.cli, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return fmt.Errorf("could not create docker client: %w", err)
	}

	if !c.isInitialized {
		err := c.initialize(ctx)
		if err != nil {
			c.cli.Close()
			return err
		}
	}

	err = c.cli.ContainerStart(ctx, c.id, types.ContainerStartOptions{})
	if err != nil {
		c.cli.Close()
		return fmt.Errorf("failed to start docker container '%s': %w", c.id, err)
	}
	c.isStarted = true

HealthCheck:
	for {
		inspect, err := c.cli.ContainerInspect(ctx, c.id)
		if err != nil {
			c.cli.ContainerStop(ctx, c.id, nil)
			c.cli.Close()
			return fmt.Errorf("failed to inspect container '%s' during healthcheck: %w", c.id, err)
		}

		status := inspect.State.Health.Status
		switch status {
		case "unhealthy":
			c.cli.ContainerStop(ctx, c.id, nil)
			c.cli.Close()
			return fmt.Errorf("container '%s' unhealthy", c.id)
		case "healthy":
			for {
				pgxPool, err := pgxpool.Connect(ctx, c.ConnectionString())
				if err == nil {
					pgxPool.Close()
					break HealthCheck
				}

				<-time.After(50 * time.Millisecond)
			}
		default:
			<-time.After(200 * time.Millisecond)
		}
	}

	go c.monitorLogs(ctx)
	return nil
}

func (c *PostgresContainer) initialize(ctx context.Context) error {
	image := "postgres:15"
	_, _, err := c.cli.ImageInspectWithRaw(ctx, image)
	if err != nil {
		if !client.IsErrNotFound(err) {
			return fmt.Errorf("could not find docker image '%s': %w", image, err)
		}

		pullReader, err := c.cli.ImagePull(ctx, image, types.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("failed to pull docker image '%s': %w", image, err)
		}
		defer pullReader.Close()

		_, err = io.Copy(io.Discard, pullReader)
		if err != nil {
			return fmt.Errorf("failed to download docker image '%s': %w", image, err)
		}
	}

	createResp, err := c.cli.ContainerCreate(ctx, &container.Config{
		Image: image,
		Cmd: []string{
			"postgres",
			"-c",
			"log_statement=all",
		},
		Env: []string{
			"POSTGRES_DB=postgres",
			"POSTGRES_PASSWORD=pgpassword",
			"POSTGRES_USER=postgres",
		},
		Healthcheck: &container.HealthConfig{
			Test:     []string{"CMD-SHELL", "pg_isready -U postgres"},
			Interval: time.Second,
			Timeout:  time.Second,
			Retries:  10,
		},
	}, &container.HostConfig{
		PortBindings: nat.PortMap{
			"5432/tcp": []nat.PortBinding{
				{HostPort: strconv.FormatUint(uint64(c.port), 10)},
			},
		},
	},
		nil,
		nil,
		"",
	)

	if err != nil {
		return fmt.Errorf("failed to create docker container: %w", err)
	}

	c.id = createResp.ID
	c.isInitialized = true
	return nil
}

func (c *PostgresContainer) Stop(ctx context.Context) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	if !c.isStarted {
		return nil
	}

	defer c.cli.Close()
	err := c.cli.ContainerStop(ctx, c.id, nil)
	c.isStarted = false
	return err
}

func (c *PostgresContainer) Cleanup(ctx context.Context) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()
	return cli.ContainerRemove(ctx, c.id, types.ContainerRemoveOptions{
		Force: true,
	})
}

func (c *PostgresContainer) monitorLogs(ctx context.Context) {
	i, err := c.cli.ContainerLogs(ctx, c.id, types.ContainerLogsOptions{
		ShowStderr: true,
		ShowStdout: true,
		Timestamps: false,
		Follow:     true,
		Tail:       "40",
	})
	if err != nil {
		log.Printf("Could not get container logs: %v", err)
		return
	}
	defer i.Close()

	file, err := os.OpenFile(c.logfile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		log.Printf("Could not create container log file: %v", err)
		return
	}
	defer file.Close()

	hdr := make([]byte, 8)
	for {
		_, err := i.Read(hdr)
		if err != nil {
			return
		}
		count := binary.BigEndian.Uint32(hdr[4:])
		dat := make([]byte, count)
		_, err = i.Read(dat)
		if err != nil {
			return
		}
		_, err = file.Write(dat)
		if err != nil {
			return
		}
	}
}

func (c *PostgresContainer) ConnectionString() string {
	return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres", c.password, c.port)
}

func (c *PostgresContainer) RunMigrations(ctx context.Context, migrationDir string) error {
	filenames, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		return fmt.Errorf("failed to glob migration files: %w", err)
	}

	sort.Strings(filenames)

	pgxPool, err := pgxpool.Connect(ctx, c.ConnectionString())
	if err != nil {
		return fmt.Errorf("failed to connect to postgres: %w", err)
	}
	defer pgxPool.Close()

	for _, filename := range filenames {
		data, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("failed to read migration file '%s': %w", filename, err)
		}

		_, err = pgxPool.Exec(ctx, string(data))
		if err != nil {
			return fmt.Errorf("failed to execute migration file '%s': %w", filename, err)
		}
	}

	return nil
}
