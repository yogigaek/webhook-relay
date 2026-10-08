-- W3C traceparent of the request that received the event. Delivery happens later in a separate
-- trace; this lets the delivery span link back to the receive span.
ALTER TABLE events ADD COLUMN trace_parent TEXT;
