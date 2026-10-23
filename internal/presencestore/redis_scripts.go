package presencestore

import (
	"context"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const luaFreshUser = `
local function hasFreshUser(key, cutoff, prefix)
  local fresh = redis.call('ZRANGEBYSCORE', key, '(' .. cutoff, '+inf')
  for _, m in ipairs(fresh) do
    if string.sub(m, 1, #prefix) == prefix then
      return true
    end
  end
  return false
end
`

var enterScript = redis.NewScript(luaFreshUser + `
local present = hasFreshUser(KEYS[1], ARGV[2], ARGV[1])
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
redis.call('HSET', KEYS[2], ARGV[6], ARGV[7])
redis.call('PEXPIRE', KEYS[2], ARGV[5])
if present then
  return 0
end
redis.call('PUBLISH', ARGV[8], ARGV[9])
return 1
`)

var exitScript = redis.NewScript(luaFreshUser + `
redis.call('ZREM', KEYS[1], ARGV[2])
if hasFreshUser(KEYS[1], ARGV[3], ARGV[1]) then
  return 0
end
redis.call('HDEL', KEYS[2], ARGV[4])
redis.call('PUBLISH', ARGV[5], ARGV[6])
return 1
`)

var sweepScript = redis.NewScript(luaFreshUser + `
local stale = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
local departed = {}
if #stale == 0 then
  return departed
end
for i = 1, #stale, 200 do
  redis.call('ZREM', KEYS[1], unpack(stale, i, math.min(i + 199, #stale)))
end
local seen = {}
for _, member in ipairs(stale) do
  local sep = string.find(member, '|', 1, true)
  if sep then
    local uid = string.sub(member, 1, sep - 1)
    if not seen[uid] then
      seen[uid] = true
      if not hasFreshUser(KEYS[1], ARGV[1], uid .. '|') then
        redis.call('HDEL', KEYS[2], uid)
        local s, e = string.find(ARGV[3], ARGV[4], 1, true)
        local payload = ARGV[3]
        if s then
          payload = string.sub(ARGV[3], 1, s - 1) .. uid .. string.sub(ARGV[3], e + 1)
        end
        redis.call('PUBLISH', ARGV[2], payload)
        table.insert(departed, uid)
      end
    end
  end
end
return departed
`)

func (r *RedisStore) Enter(ctx context.Context, boardID uuid.UUID, e presence.Entry) (bool, error) {
	profile, err := marshalProfile(e.User)
	if err != nil {
		return false, err
	}
	join, err := protocol.Encode(protocol.TypePresenceJoin, protocol.PresenceJoin{User: protocol.FromPresenceUser(e.User)})
	if err != nil {
		return false, err
	}
	res, err := enterScript.Run(ctx, r.client,
		[]string{scoreKey(boardID), profilesKey(boardID)},
		e.UserPrefix, toScore(e.Cutoff), toScore(e.At), e.Member, r.keyTTL.Milliseconds(),
		e.User.ID.String(), profile, protocol.PresenceChannel(boardID), join,
	).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (r *RedisStore) Exit(ctx context.Context, boardID uuid.UUID, x presence.Exit) (bool, error) {
	leave, err := protocol.Encode(protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: x.UserID})
	if err != nil {
		return false, err
	}
	res, err := exitScript.Run(ctx, r.client,
		[]string{scoreKey(boardID), profilesKey(boardID)},
		x.UserPrefix, x.Member, toScore(x.Cutoff), x.UserID.String(), protocol.PresenceChannel(boardID), leave,
	).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (r *RedisStore) Sweep(ctx context.Context, boardID uuid.UUID, req presence.SweepRequest) ([]uuid.UUID, error) {
	template, err := protocol.Encode(protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: uuid.Nil})
	if err != nil {
		return nil, err
	}
	raw, err := sweepScript.Run(ctx, r.client,
		[]string{scoreKey(boardID), profilesKey(boardID)},
		toScore(req.Cutoff), protocol.PresenceChannel(boardID), template, uuid.Nil.String(),
	).StringSlice()
	if err != nil {
		return nil, err
	}
	departed := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		if id, err := uuid.Parse(s); err == nil {
			departed = append(departed, id)
		}
	}
	return departed, nil
}
