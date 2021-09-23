package gomysqllock

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DefaultRefreshInterval is the periodic duration with which a connection is refreshed/pinged
const DefaultRefreshInterval = time.Second

type lockerOpt func(locker *MysqlLocker)

// MysqlLocker is the client which provide APIs to obtain lock
type MysqlLocker struct {
	db              *sql.DB
	refreshInterval time.Duration
}

// NewMysqlLocker returns an instance of locker which can be used to obtain locks
func NewMysqlLocker(db *sql.DB, lockerOpts ...lockerOpt) *MysqlLocker {
	locker := &MysqlLocker{
		db:              db,
		refreshInterval: DefaultRefreshInterval,
	}

	for _, opt := range lockerOpts {
		opt(locker)
	}

	return locker
}

// WithRefreshInterval sets the duration for refresh interval for each obtained lock
func WithRefreshInterval(d time.Duration) lockerOpt {
	return func(l *MysqlLocker) { l.refreshInterval = d }
}

// Obtain tries to acquire lock (with no MySQL timeout) with background context. This call is expected to block is lock is already held
func (l MysqlLocker) Obtain(key string) (*Lock, error) {
	return l.ObtainTimeoutContext(context.Background(), key, -1)
}

// ObtainTimeout tries to acquire lock with background context and a MySQL timeout. This call is expected to block is lock is already held
func (l MysqlLocker) ObtainTimeout(key string, timeout int) (*Lock, error) {
	return l.ObtainTimeoutContext(context.Background(), key, timeout)
}

// ObtainContext tries to acquire lock and gives up when the given context is cancelled
func (l MysqlLocker) ObtainContext(ctx context.Context, key string) (*Lock, error) {
	return l.ObtainTimeoutContext(ctx, key, -1)
}

// ObtainTimeoutContext tries to acquire lock and gives up when the given context is cancelled
func (l MysqlLocker) ObtainTimeoutContext(ctx context.Context, key string, timeout int) (*Lock, error) {
	cancellableContext, cancelFunc := context.WithCancel(context.Background())

	dbConn, err := l.db.Conn(ctx)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("failed to get a db connection: %w", err)
	}

	row := dbConn.QueryRowContext(ctx, "SELECT COALESCE(GET_LOCK(?, ?), 2)", key, timeout)

	var res int
	err = row.Scan(&res)
	if err != nil {
		// mysql error does not tell if it was due to context closing, checking it manually
		select {
		case <-ctx.Done():
			cancelFunc()
			return nil, ErrGetLockContextCancelled
		default:
			break
		}
		cancelFunc()
		return nil, fmt.Errorf("could not read mysql response: %w", err)
	} else if res == 2 {
		// Internal MySQL error occurred, such as out-of-memory, thread killed or others (the doc is not clear)
		// Note: some MySQL/MariaDB versions (like MariaDB 10.1) does not support -1 as timeout parameters
		cancelFunc()
		return nil, ErrMySQLInternalError
	} else if res == 0 {
		// MySQL Timeout
		cancelFunc()
		return nil, ErrMySQLTimeout
	}

	lock := &Lock{
		key:             key,
		conn:            dbConn,
		unlocker:        make(chan struct{}, 1),
		lostLockContext: cancellableContext,
		cancelFunc:      cancelFunc,
	}
	go lock.refresher(l.refreshInterval, cancelFunc)

	return lock, nil
}

// ObtainTimeoutContext tries to acquire lock and gives up when the given context is cancelled
func (l MysqlLocker) IsLocked(key string) (bool, error) {
	return l.IsLockedContext(context.Background(), key)
}

func (l MysqlLocker) IsLockedContext(ctx context.Context, key string) (bool, error) {
	_, cancelFunc := context.WithCancel(context.Background())

	dbConn, err := l.db.Conn(ctx)
	if err != nil {
		cancelFunc()
		return false, fmt.Errorf("failed to get a db connection: %w", err)
	}

	row := dbConn.QueryRowContext(ctx, "SELECT COALESCE(IS_USED_LOCK(?), -1)", key)

	var res int
	err = row.Scan(&res)
	if err != nil {
		// mysql error does not tell if it was due to context closing, checking it manually
		select {
		case <-ctx.Done():
			cancelFunc()
			return false, ErrGetLockContextCancelled
		default:
			break
		}
		cancelFunc()
		return false, fmt.Errorf("could not read mysql response: %w", err)
	}
	return res != -1, nil
}
