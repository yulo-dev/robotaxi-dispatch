// Package geo provides locations, a synthetic grid road network, and an A*
// router with an LRU route cache. It performs no I/O and is shared by the API
// service and the offline simulator.
package geo

import "math"

// EarthRadiusM is the mean Earth radius used by Haversine.
const EarthRadiusM = 6371008.8

// Location is a WGS84 coordinate in degrees.
type Location struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Haversine returns the great-circle distance between a and b in meters.
func Haversine(a, b Location) float64 {
	const rad = math.Pi / 180
	lat1, lat2 := a.Lat*rad, b.Lat*rad
	dLat := (b.Lat - a.Lat) * rad
	dLng := (b.Lng - a.Lng) * rad
	s := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * EarthRadiusM * math.Asin(math.Min(1, math.Sqrt(s)))
}

// BBox is an axis-aligned latitude/longitude bounding box.
type BBox struct {
	MinLat, MaxLat float64
	MinLng, MaxLng float64
}

// Contains reports whether l lies inside the box (inclusive).
func (b BBox) Contains(l Location) bool {
	return l.Lat >= b.MinLat && l.Lat <= b.MaxLat && l.Lng >= b.MinLng && l.Lng <= b.MaxLng
}

// SanFrancisco is the default bounding box.
var SanFrancisco = BBox{MinLat: 37.70, MaxLat: 37.81, MinLng: -122.52, MaxLng: -122.36}

// KmhToMps converts km/h to m/s.
func KmhToMps(kmh float64) float64 { return kmh / 3.6 }
