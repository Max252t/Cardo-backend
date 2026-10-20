package presence

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const (
	keyPrefix      = "presence:"
	profilesSuffix = ":users"
	DefaultKeyTTL  = 10 * time.Minute
	scoreExclusive = "("
	scoreNegInf    = "-inf"
	scorePosInf    = "+inf"
)

type RedisStore struct {
	client *redis.Client
	keyTTL time.Duration
}

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client, keyTTL: DefaultKeyTTL}
}

func scoreKey(boardID uuid.UUID) string {
	return keyPrefix + boardID.String()
}

func profilesKey(boardID uuid.UUID) string {
	return scoreKey(boardID) + profilesSuffix
}

func toScore(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

func (r *RedisStore) Put(ctx context.Context, boardID uuid.UUID, members []string, at time.Time) error {
	zs := make([]redis.Z, 0, len(members))
	for _, m := range members {
		zs = append(zs, redis.Z{Score: float64(at.UnixMilli()), Member: m})
	}
	_, err := r.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.ZAdd(ctx, scoreKey(boardID), zs...)
		p.Expire(ctx, scoreKey(boardID), r.keyTTL)
		return nil
	})
	return err
}

func (r *RedisStore) Remove(ctx context.Context, boardID uuid.UUID, members ...string) error {
	args := make([]any, 0, len(members))
	for _, m := range members {
		args = append(args, m)
	}
	return r.client.ZRem(ctx, scoreKey(boardID), args...).Err()
}

func (r *RedisStore) Fresh(ctx context.Context, boardID uuid.UUID, after time.Time) ([]string, error) {
	return r.client.ZRangeByScore(ctx, scoreKey(boardID), &redis.ZRangeBy{
		Min: scoreExclusive + toScore(after),
		Max: scorePosInf,
	}).Result()
}

func (r *RedisStore) Stale(ctx context.Context, boardID uuid.UUID, upTo time.Time) ([]string, error) {
	return r.client.ZRangeByScore(ctx, scoreKey(boardID), &redis.ZRangeBy{
		Min: scoreNegInf,
		Max: toScore(upTo),
	}).Result()
}

func (r *RedisStore) SaveProfiles(ctx context.Context, boardID uuid.UUID, users []protocol.PresenceUser) error {
	values := make([]any, 0, len(users)*2)
	for _, u := range users {
		data, err := json.Marshal(u)
		if err != nil {
			return fmt.Errorf("marshal presence profile: %w", err)
		}
		values = append(values, u.ID.String(), string(data))
	}
	_, err := r.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, profilesKey(boardID), values...)
		p.Expire(ctx, profilesKey(boardID), r.keyTTL)
		return nil
	})
	return err
}

func (r *RedisStore) Profiles(ctx context.Context, boardID uuid.UUID, ids []uuid.UUID) ([]protocol.PresenceUser, error) {
	fields := make([]string, 0, len(ids))
	for _, id := range ids {
		fields = append(fields, id.String())
	}
	raw, err := r.client.HMGet(ctx, profilesKey(boardID), fields...).Result()
	if err != nil {
		return nil, err
	}
	users := make([]protocol.PresenceUser, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		var u protocol.PresenceUser
		if err := json.Unmarshal([]byte(s), &u); err != nil {
			continue
		}
		users = append(users, u)
	}
	return users, nil
}

func (r *RedisStore) DeleteProfiles(ctx context.Context, boardID uuid.UUID, ids ...uuid.UUID) error {
	fields := make([]string, 0, len(ids))
	for _, id := range ids {
		fields = append(fields, id.String())
	}
	return r.client.HDel(ctx, profilesKey(boardID), fields...).Err()
}
