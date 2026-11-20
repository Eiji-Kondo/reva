package dbutil

import (
	"context"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestConfigurePoolBoundsConnections pins the pool limits, because the default
// (unbounded) is what breaks PostgreSQL on small machines.
func TestConfigurePoolBoundsConnections(t *testing.T) {
	dsn := os.Getenv("REVA_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("REVA_TEST_POSTGRES not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ConfigurePool(db); err != nil {
		t.Fatalf("ConfigurePool: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	if got := sqlDB.Stats().MaxOpenConnections; got != MaxOpenConns {
		t.Errorf("MaxOpenConnections = %d, want %d", got, MaxOpenConns)
	}

	// Opening more handles than the cap must block rather than exceed it, so
	// each acquisition gets a short deadline and a timeout is the expected
	// outcome once the pool is saturated.
	ctx := t.Context()
	held := 0
	timeouts := 0
	for i := 0; i < MaxOpenConns*3; i++ {
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		conn, err := sqlDB.Conn(cctx)
		cancel()
		if err != nil {
			timeouts++
			continue
		}
		held++
		defer conn.Close()
	}
	if held > MaxOpenConns {
		t.Errorf("acquired %d connections, want <= %d", held, MaxOpenConns)
	}
	if timeouts == 0 {
		t.Error("no acquisition timed out, the pool cap was not enforced")
	}
	if open := sqlDB.Stats().OpenConnections; open > MaxOpenConns {
		t.Errorf("OpenConnections = %d, want <= %d", open, MaxOpenConns)
	}
}
