-- An admin kick used to just clear is_admitted, which made the kicked row
-- indistinguishable from a fresh join request: it reappeared in the waiting
-- list, counted against the participant cap, and "Admit All" (or a scheduled
-- room's auto-open) silently readmitted it. kicked_at marks the row as
-- revoked so every waiting-room query can exclude it while the old token
-- still fails the is_admitted gate on reconnect.
ALTER TABLE participants ADD COLUMN kicked_at DATETIME;
