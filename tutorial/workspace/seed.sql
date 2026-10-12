CREATE TABLE customers (id INTEGER PRIMARY KEY, email TEXT NOT NULL, region TEXT);
INSERT INTO customers VALUES
 (1, ' Ada@Example.com ', 'emea'),
 (2, 'grace@example.com', 'amer'),
 (3, 'LINUS@example.com', 'emea'),
 (4, 'ken@example.com', NULL);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL,
  total REAL NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL);
INSERT INTO orders VALUES
 (101, 1, 120.50, 'paid',     '2026-10-01'),
 (102, 2,  35.00, 'paid',     '2026-10-02'),
 (103, 1,   9.99, 'refunded', '2026-10-03'),
 (104, 3, 250.00, 'paid',     '2026-10-05'),
 (105, 4,  42.00, 'pending',  '2026-10-07'),
 (106, 2,  18.25, 'paid',     '2026-10-09');
