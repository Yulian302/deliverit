-- +goose Up
-- What a restaurant sells. Prices are integer minor units (kopecks) of the
-- service's single currency (UAH): never floats for money.
CREATE TABLE menu_items (
    id            uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id uuid    NOT NULL REFERENCES restaurants (id),
    name          text    NOT NULL,
    price_minor   bigint  NOT NULL CHECK (price_minor > 0),
    available     boolean NOT NULL DEFAULT true
);
CREATE INDEX menu_items_restaurant_idx ON menu_items (restaurant_id);

-- +goose Down
DROP TABLE menu_items;
