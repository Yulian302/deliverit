-- Queries for placing and reading orders. `make gen` turns each named query
-- into a type-safe Go method in internal/ordering/dbgen (sqlc). Never edit dbgen.

-- name: InsertIdempotencyKey :execrows
-- 1 row: this request owns the key. 0 rows: the key exists (if another
-- transaction was inserting it, Postgres waited for that one to finish).
INSERT INTO idempotency_keys (scope, key, request_hash)
VALUES (@scope, @key, @request_hash)
ON CONFLICT (scope, key) DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT request_hash, response FROM idempotency_keys WHERE scope = @scope AND key = @key;

-- name: SetIdempotencyResponse :exec
UPDATE idempotency_keys SET response = @response WHERE scope = @scope AND key = @key;

-- name: GetRestaurantLocation :one
SELECT lat, lng FROM restaurants WHERE id = @id;

-- name: GetMenuItemsByIDs :many
SELECT id, restaurant_id, name, price_minor, available
FROM menu_items
WHERE id = ANY(@ids::uuid[]);

-- name: InsertOrder :exec
INSERT INTO orders (id, customer_id, restaurant_id, status, total_minor, currency,
                    delivery_lat, delivery_lng, delivery_address, version, created_at, updated_at)
VALUES (@id, @customer_id, @restaurant_id, @status, @total_minor, @currency,
        @delivery_lat, @delivery_lng, @delivery_address, 1, @created_at, @created_at);

-- name: InsertOrderItem :exec
INSERT INTO order_items (order_id, line_no, menu_item_id, name, unit_price_minor, quantity)
VALUES (@order_id, @line_no, @menu_item_id, @name, @unit_price_minor, @quantity);

-- name: GetOrder :one
SELECT id, customer_id, restaurant_id, status, total_minor, currency,
       delivery_lat, delivery_lng, delivery_address, version, created_at
FROM orders WHERE id = @id;

-- name: GetOrderItems :many
SELECT menu_item_id, name, unit_price_minor, quantity
FROM order_items WHERE order_id = @order_id ORDER BY line_no;
