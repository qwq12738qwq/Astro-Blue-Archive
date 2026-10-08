// Package ratelimit provides fixed-window rate limits backed by SQLite.
//
// ARCHITECTURE.md §27: rate limiting is never fully disabled. The counters live
// in the database rather than in memory so a restart does not hand an attacker a
// fresh allowance, and there is no external service to run for a single-admin
// blog.
package ratelimit

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Limiter counts events per key within a window.
type Limiter struct {
	db     dbLike
	prefix string
	limit  int
	window time.Duration
}

type dbLike interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// New builds a Limiter.
func New(db dbLike, prefix string, limit int, window time.Duration) *Limiter {
	if limit < 1 {
		// Abuse protection must not be switchable off (ARCHITECTURE.md §27).
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{db: db, prefix: prefix, limit: limit, window: window}
}

// Decision is the result of a limit check.
type Decision struct {
	Allowed  bool
	Count    int
	Limit    int
	RetryIn  time.Duration
	WindowIn time.Duration
}

// Allow records an event and reports whether it is within the limit.
//
// The increment and the read are a single atomic statement using RETURNING, so
// the count each caller sees is its own. Reading the counter in a second
// statement would let a burst of concurrent requests all observe the final total
// and reject requests that were legitimately within the limit.
func (l *Limiter) Allow(ctx context.Context, key string) (Decision, error) {
	now := time.Now().UTC()
	windowStart := now.Truncate(l.window).Unix()

	bucket := l.prefix + ":" + key

	var count int
	err := l.db.QueryRowContext(ctx,
		`INSERT INTO rate_limit (bucket, window_start, count) VALUES (?, ?, 1)
		 ON CONFLICT(bucket, window_start) DO UPDATE SET count = count + 1
		 RETURNING count`,
		bucket, windowStart).Scan(&count)
	if err != nil {
		return Decision{}, fmt.Errorf("record rate limit: %w", err)
	}

	retryIn := time.Duration(windowStart*int64(l.window/time.Second)+int64(l.window/time.Second)-now.Unix()) * time.Second
	if retryIn < time.Second {
		retryIn = time.Second
	}

	return Decision{
		Allowed:  count <= l.limit,
		Count:    count,
		Limit:    l.limit,
		RetryIn:  retryIn,
		WindowIn: l.window,
	}, nil
}

// Check reports whether another event would be allowed, without recording it.
func (l *Limiter) Check(ctx context.Context, key string) (Decision, error) {
	now := time.Now().UTC()
	windowStart := now.Truncate(l.window).Unix()

	var count int
	err := l.db.QueryRowContext(ctx,
		`SELECT count FROM rate_limit WHERE bucket = ? AND window_start = ?`,
		l.prefix+":"+key, windowStart).Scan(&count)
	if err != nil && err != sql.ErrNoRows {
		return Decision{}, err
	}

	return Decision{
		Allowed: count < l.limit,
		Count:   count,
		Limit:   l.limit,
		RetryIn: l.window,
	}, nil
}

// Purge removes counters older than the retention window.
func (l *Limiter) Purge(ctx context.Context, retention time.Duration) error {
	cutoff := time.Now().UTC().Add(-retention).Unix()
	_, err := l.db.ExecContext(ctx, `DELETE FROM rate_limit WHERE window_start < ?`, cutoff)
	return err
}
