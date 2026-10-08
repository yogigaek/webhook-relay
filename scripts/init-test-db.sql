-- Runs once, when the postgres volume is first created: the store tests get their own database,
-- so they never mix rows with a relay running against the main one.
CREATE DATABASE relay_test;
