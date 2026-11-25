-- Queries for the restaurant catalog. `make gen` turns each named query into
-- a type-safe Go method in internal/catalog/dbgen (sqlc). Never edit dbgen.

-- name: ListRestaurantsNear :many
-- Restaurants within @radius_km of (@lat, @lng), nearest first.
--
-- Two steps:
--   1. Bounding box (inner WHERE): a cheap, index-friendly range filter on
--      lat/lng that keeps only rows that *could* be within the radius. The
--      caller computes the box so it always contains the whole circle.
--   2. Exact great-circle distance (spherical law of cosines, Earth radius
--      6371 km) on the remaining rows, then the radius filter.
-- greatest/least clamp the acos input to [-1, 1]: floating-point rounding can
-- push it a hair outside, and acos would then fail.
SELECT id, name, lat, lng, prep_minutes, distance_km
FROM (
    SELECT id, name, lat, lng, prep_minutes,
        (6371 * acos(greatest(-1.0, least(1.0,
              cos(radians(@lat::float8)) * cos(radians(lat)) * cos(radians(lng) - radians(@lng::float8))
            + sin(radians(@lat::float8)) * sin(radians(lat))
        ))))::float8 AS distance_km
    FROM restaurants
    WHERE lat BETWEEN @min_lat::float8 AND @max_lat::float8
      AND lng BETWEEN @min_lng::float8 AND @max_lng::float8
) AS r
WHERE distance_km <= @radius_km::float8
ORDER BY distance_km, id
LIMIT @lim::int;
