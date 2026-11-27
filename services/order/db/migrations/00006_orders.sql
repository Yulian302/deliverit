-- +goose Up
-- Orders, their lines, and the idempotency keys that guard their creation.

CREATE TABLE orders (
    id               uuid             PRIMARY KEY, -- UUIDv7, set by the app
    customer_id      uuid             NOT NULL,
    restaurant_id    uuid             NOT NULL REFERENCES restaurants (id),
    -- The Order state machine (SPEC §5). Transitions are conditional
    -- UPDATEs (WHERE status = <expected>), never blind writes.
    status           text             NOT NULL CHECK (status IN (
                         'PENDING_PAYMENT', 'AWAITING_RESTAURANT', 'PREPARING', 'READY',
                         'PICKED_UP', 'DELIVERED', 'CANCELLED', 'FAILED')),
    total_minor      bigint           NOT NULL CHECK (total_minor > 0),
    currency         text             NOT NULL,
    delivery_lat     double precision NOT NULL,
    delivery_lng     double precision NOT NULL,
    delivery_address text             NOT NULL DEFAULT '',
    -- Bumped on every change; events carry it so consumers can order them.
    version          int              NOT NULL DEFAULT 1,
    created_at       timestamptz      NOT NULL DEFAULT now(),
    updated_at       timestamptz      NOT NULL DEFAULT now()
);
CREATE INDEX orders_customer_idx   ON orders (customer_id, created_at DESC);
CREATE INDEX orders_restaurant_idx ON orders (restaurant_id, created_at DESC);
CREATE INDEX orders_status_idx     ON orders (status);

-- Lines keep a snapshot of the item's name and price at order time, so a
-- later menu change never alters an existing order.
CREATE TABLE order_items (
    order_id         uuid   NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    line_no          int    NOT NULL,
    menu_item_id     uuid   NOT NULL,
    name             text   NOT NULL,
    unit_price_minor bigint NOT NULL CHECK (unit_price_minor > 0),
    quantity         int    NOT NULL CHECK (quantity BETWEEN 1 AND 20),
    PRIMARY KEY (order_id, line_no)
);

-- Public-API idempotency (SPEC §8.1). The row is inserted in the same
-- transaction as the order it guards: a concurrent duplicate blocks on the
-- primary key until that transaction ends, then replays `response`. If the
-- transaction rolls back, the row is gone and a retry starts fresh.
CREATE TABLE idempotency_keys (
    scope        text        NOT NULL, -- the customer's user ID
    key          text        NOT NULL,
    request_hash bytea       NOT NULL, -- same key + different request → rejected
    response     jsonb,                -- set before commit; the order as first returned
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, key)
);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE order_items;
DROP TABLE orders;
