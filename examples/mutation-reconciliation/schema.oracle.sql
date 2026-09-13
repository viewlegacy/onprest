CREATE TABLE mutation_reconciliation_orders (
  external_request_id VARCHAR2(64) NOT NULL,
  product_code VARCHAR2(64) NOT NULL,
  quantity NUMBER(10) NOT NULL,
  customer_code VARCHAR2(64) NOT NULL,
  CONSTRAINT uq_mutation_reconciliation_external_request_id UNIQUE (external_request_id),
  CONSTRAINT ck_mutation_reconciliation_quantity CHECK (quantity > 0)
);
