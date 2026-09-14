CREATE TABLE mutation_reconciliation_orders (
  external_request_id varchar(64) NOT NULL,
  product_code varchar(64) NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0),
  customer_code varchar(64) NOT NULL,
  CONSTRAINT uq_mutation_reconciliation_external_request_id UNIQUE (external_request_id)
);
