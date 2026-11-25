ALTER TABLE invokers DROP CONSTRAINT invokers_capacity_check;
ALTER TABLE invokers ADD CONSTRAINT invokers_capacity_check CHECK (capacity >= 0);
