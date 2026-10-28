// Package sim is a deterministic, offline fleet simulator. It reuses the geo
// and assign packages and needs no database or cache.
package sim

import (
	"fmt"
	"math"
	"math/rand"

	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
)

// Config holds simulation parameters. Defaults are fixed up front and are the
// same for every strategy.
type Config struct {
	Vehicles     int     // fleet size
	RatePerHour  float64 // off-peak request rate
	PeakMult     float64 // rate multiplier during the peak period (middle third)
	DurationSec  int     // demand is generated in [0, DurationSec)
	BatchSec     int     // dispatcher runs every BatchSec seconds
	AcceptProb   float64 // probability a vehicle accepts a dispatch
	MatchTimeout int     // seconds without assignment before a ride fails
	RadiusM      float64 // first candidate search radius
	ExpandM      float64 // second radius if the first finds nothing
	TopK         int     // candidates per ride
	MaxHungarian int     // matrix dimension cap before falling back to greedy
	StraightKmh  float64 // speed used to convert straight-line distance to ETA
	CacheSize    int     // route cache entries
	Drain        bool    // keep running after demand ends until every ride resolves
	Grid         geo.GridConfig
}

// DefaultConfig returns the default simulation configuration.
func DefaultConfig() Config {
	return Config{
		Vehicles:     80,
		RatePerHour:  150,
		PeakMult:     3,
		DurationSec:  120 * 60,
		BatchSec:     2,
		AcceptProb:   0.9,
		MatchTimeout: 300,
		RadiusM:      3000,
		ExpandM:      6000,
		TopK:         10,
		MaxHungarian: 200,
		StraightKmh:  30,
		CacheSize:    100_000,
		Drain:        true,
		Grid:         geo.DefaultGridConfig(),
	}
}

// Validate checks parameter ranges.
func (c Config) Validate() error {
	switch {
	case c.Vehicles <= 0:
		return fmt.Errorf("vehicles must be positive")
	case c.RatePerHour <= 0 || c.PeakMult < 1:
		return fmt.Errorf("rate must be positive and peak >= 1")
	case c.DurationSec <= 0 || c.BatchSec <= 0:
		return fmt.Errorf("duration and batch must be positive")
	case c.AcceptProb <= 0 || c.AcceptProb > 1:
		return fmt.Errorf("accept probability must be in (0,1]")
	case c.TopK <= 0 || c.RadiusM <= 0 || c.ExpandM < c.RadiusM:
		return fmt.Errorf("invalid candidate search parameters")
	}
	return nil
}

// Request is one ride request in a scenario.
type Request struct {
	ID         int
	T          int // request time, seconds
	Pickup     geo.Location
	Dropoff    geo.Location
	PickupNode geo.NodeID
	DropNode   geo.NodeID
}

// Scenario is the demand and initial fleet shared by every strategy for one seed.
type Scenario struct {
	Seed         int64
	Requests     []Request // sorted by T, then ID
	VehicleStart []geo.NodeID
	Hotspots     []geo.Location
}

const (
	hotspotShare  = 0.7   // fraction of pickups drawn near a hotspot
	hotspotSigmaM = 600.0 // spread of pickups around a hotspot
	minTripM      = 1000.0
	maxTripM      = 5000.0
)

// NewScenario generates demand and vehicle start positions for one seed.
//
// Demand is a non-homogeneous Poisson process sampled by thinning: rate
// RatePerHour outside the peak and RatePerHour*PeakMult during the middle
// third of the run. Pickups come from 3 to 5 Gaussian hotspots (70%) or
// uniformly over the box (30%). Trips are 1 to 5 km straight-line.
func NewScenario(n *geo.Network, cfg Config, seed int64) Scenario {
	demand := rand.New(rand.NewSource(seed))
	fleet := rand.New(rand.NewSource(seed ^ 0x5DEECE66D))
	b := cfg.Grid.BBox

	sc := Scenario{Seed: seed}
	for i := 3 + demand.Intn(3); i > 0; i-- {
		sc.Hotspots = append(sc.Hotspots, geo.Location{
			Lat: b.MinLat + (0.1+0.8*demand.Float64())*(b.MaxLat-b.MinLat),
			Lng: b.MinLng + (0.1+0.8*demand.Float64())*(b.MaxLng-b.MinLng),
		})
	}

	lambdaMax := cfg.RatePerHour * cfg.PeakMult / 3600
	peakStart, peakEnd := float64(cfg.DurationSec)/3, 2*float64(cfg.DurationSec)/3
	t := 0.0
	for {
		t += demand.ExpFloat64() / lambdaMax
		if t >= float64(cfg.DurationSec) {
			break
		}
		lambda := cfg.RatePerHour / 3600
		if t >= peakStart && t < peakEnd {
			lambda *= cfg.PeakMult
		}
		if demand.Float64() >= lambda/lambdaMax {
			continue
		}
		pickup := samplePickup(demand, b, sc.Hotspots)
		dropoff := sampleDropoff(demand, b, pickup)
		sc.Requests = append(sc.Requests, Request{
			ID:         len(sc.Requests),
			T:          int(t),
			Pickup:     pickup,
			Dropoff:    dropoff,
			PickupNode: n.NearestNode(pickup),
			DropNode:   n.NearestNode(dropoff),
		})
	}

	main := n.MainNodes()
	sc.VehicleStart = make([]geo.NodeID, cfg.Vehicles)
	for i := range sc.VehicleStart {
		sc.VehicleStart[i] = main[fleet.Intn(len(main))]
	}
	return sc
}

func metersToDeg(lat, dNorthM, dEastM float64) (dLat, dLng float64) {
	dLat = dNorthM / 111_320
	dLng = dEastM / (111_320 * math.Cos(lat*math.Pi/180))
	return
}

func samplePickup(rng *rand.Rand, b geo.BBox, hotspots []geo.Location) geo.Location {
	for {
		var loc geo.Location
		if rng.Float64() < hotspotShare {
			h := hotspots[rng.Intn(len(hotspots))]
			dLat, dLng := metersToDeg(h.Lat, rng.NormFloat64()*hotspotSigmaM, rng.NormFloat64()*hotspotSigmaM)
			loc = geo.Location{Lat: h.Lat + dLat, Lng: h.Lng + dLng}
		} else {
			loc = geo.Location{
				Lat: b.MinLat + rng.Float64()*(b.MaxLat-b.MinLat),
				Lng: b.MinLng + rng.Float64()*(b.MaxLng-b.MinLng),
			}
		}
		if b.Contains(loc) {
			return loc
		}
	}
}

func sampleDropoff(rng *rand.Rand, b geo.BBox, from geo.Location) geo.Location {
	for {
		d := minTripM + rng.Float64()*(maxTripM-minTripM)
		theta := rng.Float64() * 2 * math.Pi
		dLat, dLng := metersToDeg(from.Lat, d*math.Cos(theta), d*math.Sin(theta))
		loc := geo.Location{Lat: from.Lat + dLat, Lng: from.Lng + dLng}
		if b.Contains(loc) {
			return loc
		}
	}
}
