package catalog

import (
	"math"
	"testing"
)

// destination returns the point reached by travelling distKm from p on the
// given compass bearing (degrees) along a great circle: an independent way
// to produce points exactly on the search circle.
func destination(p Point, bearingDeg, distKm float64) Point {
	const r = 6371.0
	lat1, lng1 := p.Lat*math.Pi/180, p.Lng*math.Pi/180
	b, d := bearingDeg*math.Pi/180, distKm/r
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(d) + math.Cos(lat1)*math.Sin(d)*math.Cos(b))
	lng2 := lng1 + math.Atan2(math.Sin(b)*math.Sin(d)*math.Cos(lat1), math.Cos(d)-math.Sin(lat1)*math.Sin(lat2))
	lng := math.Mod(lng2*180/math.Pi+540, 360) - 180 // normalise to [-180, 180)
	return Point{Lat: lat2 * 180 / math.Pi, Lng: lng}
}

func (b box) contains(p Point) bool {
	const eps = 1e-9 // floating-point slack at the exact edge
	return p.Lat >= b.MinLat-eps && p.Lat <= b.MaxLat+eps && p.Lng >= b.MinLng-eps && p.Lng <= b.MaxLng+eps
}

// The box must never cut off a point that is within the radius, or the
// query would silently drop restaurants. Check every point on the circle.
func TestBoundingBox_ContainsWholeCircle(t *testing.T) {
	for name, c := range map[string]struct {
		center   Point
		radiusKm float64
	}{
		"kyiv 5km":          {Point{50.4501, 30.5234}, 5},
		"kyiv 20km":         {Point{50.4501, 30.5234}, 20},
		"equator 20km":      {Point{0, 0}, 20},
		"southern 10km":     {Point{-33.86, 151.21}, 10},
		"near north pole":   {Point{89.95, 10}, 20},
		"near antimeridian": {Point{65, 179.9}, 20},
	} {
		t.Run(name, func(t *testing.T) {
			b := boundingBox(c.center, c.radiusKm)
			if !b.contains(c.center) {
				t.Fatalf("box %+v does not contain its own center", b)
			}
			for bearing := 0.0; bearing < 360; bearing += 5 {
				p := destination(c.center, bearing, c.radiusKm)
				if !b.contains(p) {
					t.Errorf("box %+v misses %+v (bearing %v°, on the circle)", b, p, bearing)
				}
			}
		})
	}
}

// The box is only useful if it is tight: for an ordinary point it should be
// close to the circle's size, not the whole world.
func TestBoundingBox_IsTight(t *testing.T) {
	b := boundingBox(Point{50.4501, 30.5234}, 5)
	latSpanKm := (b.MaxLat - b.MinLat) * 111.195 // km per degree of latitude
	if latSpanKm < 9.9 || latSpanKm > 10.1 {
		t.Errorf("latitude span = %.2f km, want ~10 km for a 5 km radius", latSpanKm)
	}
	if b.MaxLng-b.MinLng > 1 {
		t.Errorf("longitude span = %.3f°, want a narrow band, not the world", b.MaxLng-b.MinLng)
	}
}

// Where the circle reaches a pole or crosses ±180°, a lat/lng box can't
// describe it as one range; the box falls back to all longitudes.
func TestBoundingBox_FallsBackToAllLongitudes(t *testing.T) {
	for name, p := range map[string]Point{
		"pole inside circle": {89.99, 0},
		"crosses +180":       {10, 179.99},
		"crosses -180":       {10, -179.99},
	} {
		t.Run(name, func(t *testing.T) {
			b := boundingBox(p, 5)
			if b.MinLng != -180 || b.MaxLng != 180 {
				t.Errorf("lng range = [%v, %v], want [-180, 180]", b.MinLng, b.MaxLng)
			}
		})
	}
}
