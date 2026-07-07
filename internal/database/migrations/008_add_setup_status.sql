-- Server-owned setup-wizard state.
--
-- The web setup wizard previously tracked completion and dismissal in browser
-- localStorage, which let it mark itself "complete" with no server truth. These
-- columns move that state onto the singleton config row so the install state is
-- global and deterministic across browsers/admins.
--
-- The TURN reachability test columns persist the last test result plus a
-- signature of the effective TURN settings it ran against, so the setup status
-- can tell whether a stored test is still valid for the current configuration.
ALTER TABLE config ADD COLUMN setup_completed_at DATETIME;
ALTER TABLE config ADD COLUMN setup_dismissed_at DATETIME;
ALTER TABLE config ADD COLUMN turn_last_tested_at DATETIME;
ALTER TABLE config ADD COLUMN turn_last_test_success BOOLEAN DEFAULT FALSE;
ALTER TABLE config ADD COLUMN turn_last_test_message TEXT;
ALTER TABLE config ADD COLUMN turn_last_test_signature TEXT;
