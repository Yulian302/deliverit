-- +goose Up
-- Supports the bounding-box pre-filter in ListRestaurantsNear: Postgres can
-- range-scan lat (and check lng from the index) instead of computing the
-- great-circle distance for every row.
CREATE INDEX restaurants_lat_lng_idx ON restaurants (lat, lng);

-- +goose Down
DROP INDEX restaurants_lat_lng_idx;
