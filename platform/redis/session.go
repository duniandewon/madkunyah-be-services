package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Session is the server-side record behind one refresh token. Rotated tokens
// are kept, marked Rotated, until they expire: that is what lets a replay be
// recognised as reuse rather than as an unknown token.
type Session struct {
	SubjectID       string
	SubjectType     string
	Identifier      string
	DeviceID        string
	OutletID        string
	FamilyID        string
	IssuedAt        time.Time
	FamilyStartedAt time.Time
	Rotated         bool
}

func refreshKey(hash string) string    { return "refresh:" + hash }
func familyKey(familyID string) string { return "refresh:family:" + familyID }
func successorKey(hash string) string  { return "refresh:successor:" + hash }
func subjectKey(typ, id string) string { return "refresh:subject:" + typ + ":" + id }
func revokedKey(jti string) string     { return "revoked:" + jti }

func (s *Session) fields() []any {
	return []any{
		"sub", s.SubjectID, "styp", s.SubjectType, "idn", s.Identifier,
		"dev", s.DeviceID, "out", s.OutletID, "fam", s.FamilyID,
		"iat", s.IssuedAt.Unix(), "fst", s.FamilyStartedAt.Unix(), "rot", "0",
	}
}

// createSessionScript stores the token record, adds it to its family, and
// indexes the family under its subject so all sessions can be revoked.
var createSessionScript = goredis.NewScript(`
redis.call('HSET', KEYS[1], unpack(ARGV, 4))
redis.call('PEXPIRE', KEYS[1], ARGV[1])
redis.call('SADD', KEYS[2], ARGV[2])
redis.call('PEXPIRE', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[3], ARGV[3])
if redis.call('PTTL', KEYS[3]) < tonumber(ARGV[1]) then
  redis.call('PEXPIRE', KEYS[3], ARGV[1])
end
return 1
`)

func (c *Client) CreateSession(ctx context.Context, hash string, s *Session, ttl time.Duration) error {
	keys := []string{refreshKey(hash), familyKey(s.FamilyID), subjectKey(s.SubjectType, s.SubjectID)}
	args := append([]any{ttl.Milliseconds(), hash, s.FamilyID}, s.fields()...)
	if err := createSessionScript.Run(ctx, c.rdb, keys, args...).Err(); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (c *Client) GetSession(ctx context.Context, hash string) (*Session, error) {
	m, err := c.rdb.HGetAll(ctx, refreshKey(hash)).Result()
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if len(m) == 0 {
		return nil, ErrNotFound
	}
	iat, _ := strconv.ParseInt(m["iat"], 10, 64)
	fst, _ := strconv.ParseInt(m["fst"], 10, 64)
	return &Session{
		SubjectID:       m["sub"],
		SubjectType:     m["styp"],
		Identifier:      m["idn"],
		DeviceID:        m["dev"],
		OutletID:        m["out"],
		FamilyID:        m["fam"],
		IssuedAt:        time.Unix(iat, 0),
		FamilyStartedAt: time.Unix(fst, 0),
		Rotated:         m["rot"] == "1",
	}, nil
}

// rotateSessionScript is a compare-and-set: it succeeds only if the old token
// has not been rotated yet, so two concurrent refreshes cannot both mint a
// successor.
var rotateSessionScript = goredis.NewScript(`
if redis.call('HGET', KEYS[1], 'rot') ~= '0' then return 0 end
redis.call('HSET', KEYS[1], 'rot', '1')
redis.call('HSET', KEYS[2], unpack(ARGV, 5))
redis.call('PEXPIRE', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[3], ARGV[2])
redis.call('PEXPIRE', KEYS[3], ARGV[1])
redis.call('SET', KEYS[4], ARGV[4], 'PX', ARGV[3])
if redis.call('PTTL', KEYS[5]) < tonumber(ARGV[1]) then
  redis.call('PEXPIRE', KEYS[5], ARGV[1])
end
return 1
`)

// RotateSession marks oldHash rotated, stores the successor record, and keeps
// the successor payload for the grace window. It returns false if oldHash was
// already rotated by a concurrent call.
func (c *Client) RotateSession(ctx context.Context, oldHash, newHash string, s *Session, ttl, grace time.Duration, successor []byte) (bool, error) {
	keys := []string{
		refreshKey(oldHash), refreshKey(newHash), familyKey(s.FamilyID),
		successorKey(oldHash), subjectKey(s.SubjectType, s.SubjectID),
	}
	args := append([]any{ttl.Milliseconds(), newHash, grace.Milliseconds(), successor}, s.fields()...)
	n, err := rotateSessionScript.Run(ctx, c.rdb, keys, args...).Int()
	if err != nil {
		return false, fmt.Errorf("rotate session: %w", err)
	}
	return n == 1, nil
}

// GetSuccessor returns the payload stored for a recently rotated token, or
// ErrNotFound once the grace window has passed.
func (c *Client) GetSuccessor(ctx context.Context, oldHash string) ([]byte, error) {
	b, err := c.rdb.Get(ctx, successorKey(oldHash)).Bytes()
	if err != nil {
		if wrapNil(err) == ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get successor: %w", err)
	}
	return b, nil
}

var revokeFamilyScript = goredis.NewScript(`
for _, h in ipairs(redis.call('SMEMBERS', KEYS[1])) do
  redis.call('DEL', 'refresh:' .. h, 'refresh:successor:' .. h)
end
redis.call('DEL', KEYS[1])
return 1
`)

// RevokeFamily deletes every token in a family, including rotated ones.
func (c *Client) RevokeFamily(ctx context.Context, familyID string) error {
	if err := revokeFamilyScript.Run(ctx, c.rdb, []string{familyKey(familyID)}).Err(); err != nil {
		return fmt.Errorf("revoke family: %w", err)
	}
	return nil
}

// RevokeSubject revokes every family belonging to a subject.
func (c *Client) RevokeSubject(ctx context.Context, subjectType, subjectID string) error {
	key := subjectKey(subjectType, subjectID)
	families, err := c.rdb.SMembers(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("list subject families: %w", err)
	}
	for _, fam := range families {
		if err := c.RevokeFamily(ctx, fam); err != nil {
			return err
		}
	}
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("delete subject index: %w", err)
	}
	return nil
}

// DenyToken blocks an access token until it would have expired anyway.
func (c *Client) DenyToken(ctx context.Context, jti string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	if err := c.rdb.Set(ctx, revokedKey(jti), 1, ttl).Err(); err != nil {
		return fmt.Errorf("deny token: %w", err)
	}
	return nil
}

func (c *Client) IsTokenDenied(ctx context.Context, jti string) (bool, error) {
	n, err := c.rdb.Exists(ctx, revokedKey(jti)).Result()
	if err != nil {
		return false, fmt.Errorf("check denied token: %w", err)
	}
	return n == 1, nil
}
