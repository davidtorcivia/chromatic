-- Persistent screen share permission per participant.
-- Once an admin approves a participant's screen share request, the approval
-- sticks for the lifetime of the participant (until explicitly revoked via
-- admin:screenshare-revoke). Admins are implicitly allowed and do not use
-- this flag.
ALTER TABLE participants ADD COLUMN can_screenshare BOOLEAN DEFAULT FALSE;
