package app

import (
	"context"
	"errors"

	"github.com/AGenUI/agenui-studio/harness/internal/redisstore"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type recordingMySQLBackend struct {
	checkReadyCalls int
	checkReadyErr   error
	migrateCalls    int
	migrateErr      error
	stores          storage.Stores
}

func (b *recordingMySQLBackend) CheckReady(context.Context) error {
	b.checkReadyCalls++
	return b.checkReadyErr
}

func (b *recordingMySQLBackend) Migrate(context.Context) error {
	b.migrateCalls++
	return b.migrateErr
}

func (b *recordingMySQLBackend) Stores() storage.Stores { return b.stores }

type observingRedisClient struct {
	redisstore.Client
	beforeClose func()
	closeErr    error
}

func (c *observingRedisClient) Close() error {
	if c.beforeClose != nil {
		c.beforeClose()
	}
	if c.Client == nil {
		return c.closeErr
	}
	return errors.Join(c.closeErr, c.Client.Close())
}
