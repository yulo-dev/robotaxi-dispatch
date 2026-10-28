// Package cache holds the Redis vehicle index and distributed locks.
//
// Keys:
//
//	vehicles:geo      GEO set of AVAILABLE vehicles only
//	vehicle:{id}      HASH status, version, lat, lng, battery, hb (unix ms)
//	lock:ride:{id}    STRING token, SET NX PX
//	lock:vehicle:{id} STRING token, SET NX PX
package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
)

const geoKey = "vehicles:geo"

func vehicleKey(id string) string { return "vehicle:" + id }

// Cache wraps a Redis client.
type Cache struct {
	rdb *redis.Client
}

// New connects to Redis at addr.
func New(addr string, db int) *Cache {
	return &Cache{rdb: redis.NewClient(&redis.Options{Addr: addr, DB: db})}
}

// Client exposes the client for tests.
func (c *Cache) Client() *redis.Client { return c.rdb }

// Close closes the client.
func (c *Cache) Close() error { return c.rdb.Close() }

// Ping checks connectivity.
func (c *Cache) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// upsertScript writes location fields unconditionally and the status only
// when the incoming version is not older than the stored one. Postgres is the
// source of truth; the version check stops a slow heartbeat from overwriting
// a newer status written by the dispatcher. GEO membership follows the
// resulting status.
//
// KEYS[1]=vehicle hash, KEYS[2]=geo set
// ARGV: id, version (-1 = location only), status, lat, lng, battery, hb_ms
var upsertScript = redis.NewScript(`
local cur = tonumber(redis.call('HGET', KEYS[1], 'version') or '-1')
local v = tonumber(ARGV[2])
if v >= 0 and v >= cur then
  redis.call('HSET', KEYS[1], 'version', ARGV[2], 'status', ARGV[3])
end
redis.call('HSET', KEYS[1], 'lat', ARGV[4], 'lng', ARGV[5], 'battery', ARGV[6], 'hb', ARGV[7])
local st = redis.call('HGET', KEYS[1], 'status')
if st == 'AVAILABLE' then
  redis.call('GEOADD', KEYS[2], ARGV[5], ARGV[4], ARGV[1])
else
  redis.call('ZREM', KEYS[2], ARGV[1])
end
return st
`)

func (c *Cache) upsert(ctx context.Context, id string, version int64, status string, lat, lng, battery float64, hb time.Time) error {
	return upsertScript.Run(ctx, c.rdb, []string{vehicleKey(id), geoKey},
		id, version, status, lat, lng, battery, hb.UnixMilli()).Err()
}

// UpsertVehicle mirrors a vehicle row read from Postgres.
func (c *Cache) UpsertVehicle(ctx context.Context, v store.Vehicle) error {
	return c.upsert(ctx, v.ID, v.Version, string(v.Status), v.Lat, v.Lng, v.BatteryPct, v.LastHeartbeatAt)
}

// UpdateLocation refreshes position, battery, and heartbeat time without
// touching status. Used for heartbeats that skip the Postgres write.
func (c *Cache) UpdateLocation(ctx context.Context, id string, lat, lng, battery float64, at time.Time) error {
	return c.upsert(ctx, id, -1, "", lat, lng, battery, at)
}

// Candidate is a vehicle returned by a GEO search.
type Candidate struct {
	ID         string
	Lat, Lng   float64
	DistM      float64
	BatteryPct float64
	Heartbeat  time.Time
}

// SearchQuery configures Candidates.
type SearchQuery struct {
	Lat, Lng   float64
	RadiusM    float64
	ExpandM    float64 // searched once if RadiusM finds nothing usable
	K          int
	MaxAge     time.Duration // heartbeat freshness
	MinBattery float64
	Exclude    map[string]bool
	Now        time.Time
}

// Candidates returns up to K AVAILABLE vehicles with a fresh heartbeat,
// nearest first, searching RadiusM and then ExpandM once.
func (c *Cache) Candidates(ctx context.Context, q SearchQuery) ([]Candidate, error) {
	radii := []float64{q.RadiusM}
	if q.ExpandM > q.RadiusM {
		radii = append(radii, q.ExpandM)
	}
	for _, r := range radii {
		out, err := c.search(ctx, q, r)
		if err != nil || len(out) > 0 {
			return out, err
		}
	}
	return nil, nil
}

func (c *Cache) search(ctx context.Context, q SearchQuery, radius float64) ([]Candidate, error) {
	// Over-fetch so that filtering stale or excluded vehicles still leaves K.
	locs, err := c.rdb.GeoSearchLocation(ctx, geoKey, &redis.GeoSearchLocationQuery{
		GeoSearchQuery: redis.GeoSearchQuery{
			Longitude: q.Lng, Latitude: q.Lat,
			Radius: radius, RadiusUnit: "m",
			Sort: "ASC", Count: 3*q.K + len(q.Exclude),
		},
		WithCoord: true, WithDist: true,
	}).Result()
	if err != nil || len(locs) == 0 {
		return nil, err
	}
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.SliceCmd, len(locs))
	for i, l := range locs {
		cmds[i] = pipe.HMGet(ctx, vehicleKey(l.Name), "status", "battery", "hb")
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	var out []Candidate
	for i, l := range locs {
		if q.Exclude[l.Name] {
			continue
		}
		vals := cmds[i].Val()
		if len(vals) != 3 || vals[0] != string(store.VehicleAvailable) {
			continue
		}
		battery, _ := strconv.ParseFloat(fmt.Sprint(vals[1]), 64)
		hbMs, _ := strconv.ParseInt(fmt.Sprint(vals[2]), 10, 64)
		hb := time.UnixMilli(hbMs)
		if q.MaxAge > 0 && q.Now.Sub(hb) > q.MaxAge {
			continue
		}
		if battery < q.MinBattery {
			continue
		}
		out = append(out, Candidate{ID: l.Name, Lat: l.Latitude, Lng: l.Longitude, DistM: l.Dist,
			BatteryPct: battery, Heartbeat: hb})
		if len(out) == q.K {
			break
		}
	}
	return out, nil
}

// AvailableCount returns the number of vehicles in the GEO index.
func (c *Cache) AvailableCount(ctx context.Context) (int64, error) {
	return c.rdb.ZCard(ctx, geoKey).Result()
}

// ---- locks ----

// Lock is a held lock.
type Lock struct {
	key, token string
}

var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// ErrNotHeld means another holder has the lock.
var ErrNotHeld = errors.New("lock held by another worker")

func (c *Cache) acquire(ctx context.Context, key string, ttl time.Duration) (*Lock, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(b[:])
	ok, err := c.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotHeld
	}
	return &Lock{key: key, token: token}, nil
}

// LockRide acquires lock:ride:{id}.
func (c *Cache) LockRide(ctx context.Context, id string, ttl time.Duration) (*Lock, error) {
	return c.acquire(ctx, "lock:ride:"+id, ttl)
}

// LockVehicle acquires lock:vehicle:{id}.
func (c *Cache) LockVehicle(ctx context.Context, id string, ttl time.Duration) (*Lock, error) {
	return c.acquire(ctx, "lock:vehicle:"+id, ttl)
}

// Release deletes the lock only if this holder still owns it.
func (c *Cache) Release(ctx context.Context, l *Lock) error {
	if l == nil {
		return nil
	}
	return releaseScript.Run(ctx, c.rdb, []string{l.key}, l.token).Err()
}
