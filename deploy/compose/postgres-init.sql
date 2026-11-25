-- Runs once, when the Postgres volume is first created.
--
-- One database AND one login role per service. Each service connects as its
-- own role, which owns its own database and cannot even connect to the
-- others: "a service's data belongs to that service" is enforced by Postgres
-- itself, not just by convention. Services share data only through APIs and
-- events.
--
-- Local development credentials only (password = role name). Real
-- environments inject secrets at deploy time; nothing real is committed.

CREATE ROLE order_svc LOGIN PASSWORD 'order_svc';
CREATE DATABASE order_db OWNER order_svc;
REVOKE ALL ON DATABASE order_db FROM PUBLIC;

-- Used from later slices (Dispatch, Location); created now so the layout is
-- visible from day one.
CREATE ROLE dispatch_svc LOGIN PASSWORD 'dispatch_svc';
CREATE DATABASE dispatch_db OWNER dispatch_svc;
REVOKE ALL ON DATABASE dispatch_db FROM PUBLIC;

CREATE ROLE location_svc LOGIN PASSWORD 'location_svc';
CREATE DATABASE location_db OWNER location_svc;
REVOKE ALL ON DATABASE location_db FROM PUBLIC;
