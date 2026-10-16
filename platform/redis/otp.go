package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Limit rejection reasons returned by AllowOTPRequest.
const (
	LimitIP       = "ip"
	LimitCooldown = "cooldown"
	LimitQuota    = "quota"
)

type OTPLimits struct {
	IPLimit    int
	IPWindow   time.Duration
	Cooldown   time.Duration
	DailyQuota int
	QuotaTTL   time.Duration
}

func otpKey(purpose, identifier string) string {
	return fmt.Sprintf("otp:%s:%s", purpose, identifier)
}

func cooldownKey(identifier string) string { return "otp:cooldown:" + identifier }

func quotaKey(identifier, date string) string {
	return fmt.Sprintf("otp:quota:%s:%s", identifier, date)
}

// saveOTPScript replaces any existing record so the previous code stops
// working immediately and the attempt counter resets.
var saveOTPScript = goredis.NewScript(`
redis.call('DEL', KEYS[1])
redis.call('HSET', KEYS[1], 'mac', ARGV[1], 'attempts', 0, 'sent_at', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

func (c *Client) SaveOTP(ctx context.Context, purpose, identifier, mac string, sentAt time.Time, ttl time.Duration) error {
	err := saveOTPScript.Run(ctx, c.rdb, []string{otpKey(purpose, identifier)},
		mac, sentAt.Unix(), ttl.Milliseconds()).Err()
	if err != nil {
		return fmt.Errorf("save otp: %w", err)
	}
	return nil
}

// attemptOTPScript counts the attempt before the caller compares, so
// concurrent guesses cannot share one attempt slot.
var attemptOTPScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return false end
local attempts = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
return {redis.call('HGET', KEYS[1], 'mac'), attempts}
`)

// AttemptOTP increments the attempt counter and returns the stored MAC.
// It returns ErrNotFound when no code is outstanding.
func (c *Client) AttemptOTP(ctx context.Context, purpose, identifier string) (string, int, error) {
	res, err := attemptOTPScript.Run(ctx, c.rdb, []string{otpKey(purpose, identifier)}).Slice()
	if err != nil {
		if wrapNil(err) == ErrNotFound {
			return "", 0, ErrNotFound
		}
		return "", 0, fmt.Errorf("attempt otp: %w", err)
	}
	mac, _ := res[0].(string)
	attempts, _ := res[1].(int64)
	return mac, int(attempts), nil
}

// deleteOTPScript deletes the record only if it still holds the given MAC,
// so a code issued after the read is never removed by mistake. Exactly one
// of several concurrent callers sees 1.
var deleteOTPScript = goredis.NewScript(`
if redis.call('HGET', KEYS[1], 'mac') == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

func (c *Client) DeleteOTP(ctx context.Context, purpose, identifier, mac string) (bool, error) {
	n, err := deleteOTPScript.Run(ctx, c.rdb, []string{otpKey(purpose, identifier)}, mac).Int()
	if err != nil {
		return false, fmt.Errorf("delete otp: %w", err)
	}
	return n == 1, nil
}

// allowOTPScript applies the IP quota, cooldown and identifier quota in one
// atomic step. Returns {reason, retry_after_ms}; an empty reason means allowed.
var allowOTPScript = goredis.NewScript(`
local ip = redis.call('INCR', KEYS[1])
if ip == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
if ip > tonumber(ARGV[1]) then return {'ip', redis.call('PTTL', KEYS[1])} end

local cd = redis.call('PTTL', KEYS[2])
if cd > 0 then return {'cooldown', cd} end

local used = tonumber(redis.call('GET', KEYS[3]) or '0')
if used >= tonumber(ARGV[4]) then return {'quota', 0} end

redis.call('INCR', KEYS[3])
redis.call('PEXPIRE', KEYS[3], ARGV[5])
redis.call('SET', KEYS[2], 1, 'PX', ARGV[3])
return {'', 0}
`)

// AllowOTPRequest returns an empty reason when the request may proceed, or a
// Limit* reason and how long the caller should wait. date is the quota day.
func (c *Client) AllowOTPRequest(ctx context.Context, identifier, ip, date string, l OTPLimits) (string, time.Duration, error) {
	keys := []string{"otp:quota:ip:" + ip, cooldownKey(identifier), quotaKey(identifier, date)}
	res, err := allowOTPScript.Run(ctx, c.rdb, keys,
		l.IPLimit, l.IPWindow.Milliseconds(), l.Cooldown.Milliseconds(),
		l.DailyQuota, l.QuotaTTL.Milliseconds()).Slice()
	if err != nil {
		return "", 0, fmt.Errorf("check otp limits: %w", err)
	}
	reason, _ := res[0].(string)
	ms, _ := res[1].(int64)
	return reason, time.Duration(ms) * time.Millisecond, nil
}

var refundQuotaScript = goredis.NewScript(`
if tonumber(redis.call('GET', KEYS[1]) or '0') > 0 then
  return redis.call('DECR', KEYS[1])
end
return 0
`)

// RefundOTPQuota returns a daily-quota slot after a permanently failed
// dispatch. The cooldown is deliberately left in place.
func (c *Client) RefundOTPQuota(ctx context.Context, identifier, date string) error {
	if err := refundQuotaScript.Run(ctx, c.rdb, []string{quotaKey(identifier, date)}).Err(); err != nil {
		return fmt.Errorf("refund otp quota: %w", err)
	}
	return nil
}

var fixedWindowScript = goredis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
if n > tonumber(ARGV[2]) then return redis.call('PTTL', KEYS[1]) end
return 0
`)

// AllowOTPVerify counts verify calls per IP. It returns 0 when allowed, or
// the time until the window resets.
func (c *Client) AllowOTPVerify(ctx context.Context, ip string, limit int, window time.Duration) (time.Duration, error) {
	ms, err := fixedWindowScript.Run(ctx, c.rdb, []string{"otp:verify:ip:" + ip},
		window.Milliseconds(), limit).Int64()
	if err != nil {
		return 0, fmt.Errorf("check verify limit: %w", err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}
