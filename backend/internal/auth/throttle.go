package auth

import (
	"context"
	"time"
)

// LoginThrottle bounds password-guessing attempts (ARCHITECTURE.md §8 #10).
//
// The counters are stored in SQLite rather than in memory so that restarting the
// process does not hand an attacker a fresh allowance.
type LoginThrottle struct {
	db          dbLike
	perMinute   int
	perHour     int
	lockoutBase time.Duration
}

// NewLoginThrottle builds a throttle. Both limits must already be >= 1;
// config.Load rejects anything smaller so abuse protection cannot be switched off.
func NewLoginThrottle(db dbLike, perMinute, perHour int) *LoginThrottle {
	return &LoginThrottle{
		db:          db,
		perMinute:   perMinute,
		perHour:     perHour,
		lockoutBase: 15 * time.Minute,
	}
}

// Key identifies a throttle bucket. Both the username and the IP matter: one
// stops a distributed attack on a known account, the other stops one host from
// spraying many accounts.
type Key struct {
	Username string
	IPHash   []byte
}

// Decision is the outcome of a throttle check.
type Decision struct {
	Allowed  bool
	RetryIn  time.Duration
	Reason   string
	Attempts int
}

// Check reports whether a login attempt may proceed, without recording it.
func (t *LoginThrottle) Check(ctx context.Context, key Key) (Decision, error) {
	now := time.Now().UTC()

	attempts, err := t.recentFailures(ctx, key, now.Add(-time.Hour))
	if err != nil {
		return Decision{}, err
	}

	if attempts >= t.perHour {
		// The window is anchored on the oldest failure in the hour so the
		// caller learns when it can retry.
		retry, err := t.retryAfter(ctx, key, now.Add(-time.Hour), time.Hour)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Allowed: false, RetryIn: retry, Reason: "too many attempts", Attempts: attempts}, nil
	}

	if attempts >= t.perMinute {
		retry, err := t.retryAfter(ctx, key, now.Add(-time.Minute), time.Minute)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Allowed: false, RetryIn: retry, Reason: "too many attempts", Attempts: attempts}, nil
	}

	return Decision{Allowed: true, Attempts: attempts}, nil
}

func (t *LoginThrottle) recentFailures(ctx context.Context, key Key, since time.Time) (int, error) {
	var n int
	err := t.db.QueryRowContext(ctx,
		`SELECT count(*) FROM login_attempt
		  WHERE username = ? AND ip_hash = ? AND ok = 0 AND created_at >= ?`,
		key.Username, key.IPHash, since.Format(time.RFC3339)).Scan(&n)
	return n, err
}

func (t *LoginThrottle) retryAfter(ctx context.Context, key Key, since time.Time, window time.Duration) (time.Duration, error) {
	var oldest string
	err := t.db.QueryRowContext(ctx,
		`SELECT min(created_at) FROM login_attempt
		  WHERE username = ? AND ip_hash = ? AND ok = 0 AND created_at >= ?`,
		key.Username, key.IPHash, since.Format(time.RFC3339)).Scan(&oldest)
	if err != nil {
		return 0, err
	}
	if oldest == "" {
		return window, nil
	}
	t0, err := time.Parse(time.RFC3339, oldest)
	if err != nil {
		return window, nil
	}
	retry := t0.Add(window).Sub(time.Now().UTC())
	if retry < time.Second {
		retry = time.Second
	}
	return retry, nil
}

// Record appends the outcome of a login attempt. Successes are recorded too so
// the audit trail shows them, but only failures count toward the limit.
func (t *LoginThrottle) Record(ctx context.Context, key Key, ok bool) error {
	v := 0
	if ok {
		v = 1
	}
	_, err := t.db.ExecContext(ctx,
		`INSERT INTO login_attempt (username, ip_hash, ok, created_at) VALUES (?, ?, ?, ?)`,
		key.Username, key.IPHash, v, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Reset clears the failure history for a key after a successful login.
func (t *LoginThrottle) Reset(ctx context.Context, key Key) error {
	_, err := t.db.ExecContext(ctx,
		`DELETE FROM login_attempt WHERE username = ? AND ip_hash = ? AND ok = 0`,
		key.Username, key.IPHash)
	return err
}

// Purge removes attempt rows older than the retention window.
func (t *LoginThrottle) Purge(ctx context.Context, olderThan time.Duration) error {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	_, err := t.db.ExecContext(ctx, `DELETE FROM login_attempt WHERE created_at < ?`, cutoff)
	return err
}
