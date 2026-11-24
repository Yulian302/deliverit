-- +goose Up
CREATE TABLE restaurants (
    id           uuid             PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text             NOT NULL,
    lat          double precision NOT NULL,
    lng          double precision NOT NULL,
    prep_minutes int              NOT NULL CHECK (prep_minutes > 0),
    created_at   timestamptz      NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE restaurants;
