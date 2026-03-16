fix: log dispatch failure to event log on every attempt (Forge-eyds)

recordDispatchFailure only logged to the event log when the circuit
breaker tripped (after 3 consecutive failures). The first and second
failures were silently written only to the retries table, making the
failure reason invisible in Hearth's event log — requiring a direct
SQLite query to diagnose.

Added EventDispatchFailed and log it on every dispatch failure with
the attempt count and reason, so the error surfaces immediately in
Hearth's live activity feed.
