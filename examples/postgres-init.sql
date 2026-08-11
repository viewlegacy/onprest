CREATE ROLE capability_user LOGIN PASSWORD 'onprest-example-password';

CREATE TABLE customers (
  id integer PRIMARY KEY,
  name text NOT NULL,
  email text NOT NULL
);

INSERT INTO customers (id, name, email) VALUES
  (1, 'Ada Lovelace', 'ada@example.com'),
  (2, 'Grace Hopper', 'grace@example.com');

GRANT CONNECT ON DATABASE legacy TO capability_user;
GRANT USAGE ON SCHEMA public TO capability_user;
GRANT SELECT ON customers TO capability_user;
GRANT UPDATE (name) ON customers TO capability_user;
