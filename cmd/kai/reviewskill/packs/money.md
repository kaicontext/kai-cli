## Money, credits and billing (this change moves value)

- **Direction:** say who is debited and who is credited, from the function that moves the money rather than its name.
- **Units and precision:** cents against whole units, a currency conversion applied twice or not at all, and floating point used for money.
- **Idempotency:** a charge, refund, grant or payout that can run twice (a retry, a duplicate webhook, a double click) and has no idempotency key or unique constraint.
- **Concurrency:** a balance or counter read and written back instead of updated atomically.
- **Webhooks:** a payment event processed before its status or signature is checked, or a new event type the handler does not treat like its siblings.
- **Rounding, fees and caps:** is the rounding direction deliberate, and where does the remainder go?
