-- Remove the sponsored slot. Paid placement is not supported by the payment
-- provider, so the feature is gone: no sponsors, no orders, no webhook ledger.
-- Child tables first: sponsor_orders references sponsors.
--
-- traffic_events rows of kind 'sponsor' are left in place. They are history,
-- the table has no constraint on kind, and nothing reads them any more.
DROP TABLE webhook_deliveries;
DROP TABLE sponsor_orders;
DROP TABLE sponsors;
