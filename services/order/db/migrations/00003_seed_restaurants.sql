-- +goose Up
-- Demo data: 10 made-up restaurants within ~4 km of Kyiv city centre
-- (50.4501, 30.5234). Fixed IDs so demos and curl examples are repeatable.
--
-- Note: being a migration, this runs in every environment that migrates.
-- That's intended for this demo project; a real system would load catalog
-- data through an admin API or a separate seed command instead.
INSERT INTO restaurants (id, name, lat, lng, prep_minutes) VALUES
    ('0193a1b0-0000-7000-8000-000000000001', 'Khreshchatyk Kitchen', 50.4480, 30.5220, 15),
    ('0193a1b0-0000-7000-8000-000000000002', 'Golden Gate Grill',    50.4485, 30.5130, 20),
    ('0193a1b0-0000-7000-8000-000000000003', 'Pechersk Pho',         50.4420, 30.5400, 12),
    ('0193a1b0-0000-7000-8000-000000000004', 'Arsenal Burgers',      50.4440, 30.5450, 10),
    ('0193a1b0-0000-7000-8000-000000000005', 'Podil Pierogi',        50.4650, 30.5160, 18),
    ('0193a1b0-0000-7000-8000-000000000006', 'Lybidska Noodles',     50.4260, 30.5180, 14),
    ('0193a1b0-0000-7000-8000-000000000007', 'Lavra Lunch',          50.4350, 30.5560, 22),
    ('0193a1b0-0000-7000-8000-000000000008', 'Lukianivka Bakery',    50.4600, 30.4850, 25),
    ('0193a1b0-0000-7000-8000-000000000009', 'Hydropark Fish',       50.4460, 30.5750, 16),
    ('0193a1b0-0000-7000-8000-000000000010', 'Obolon Tacos',         50.4800, 30.4990, 11);

-- +goose Down
DELETE FROM restaurants WHERE id IN (
    '0193a1b0-0000-7000-8000-000000000001', '0193a1b0-0000-7000-8000-000000000002',
    '0193a1b0-0000-7000-8000-000000000003', '0193a1b0-0000-7000-8000-000000000004',
    '0193a1b0-0000-7000-8000-000000000005', '0193a1b0-0000-7000-8000-000000000006',
    '0193a1b0-0000-7000-8000-000000000007', '0193a1b0-0000-7000-8000-000000000008',
    '0193a1b0-0000-7000-8000-000000000009', '0193a1b0-0000-7000-8000-000000000010'
);
