-- Chromatic Initial Schema
-- Migration: 001_initial.sql

-- Stream keys for OBS authentication (persistent)
CREATE TABLE stream_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,                 -- "Main Suite", "Laptop"
    key_token TEXT UNIQUE NOT NULL,     -- Secret used in OBS WHIP URL
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Rooms
CREATE TABLE rooms (
    id TEXT PRIMARY KEY,
    slug TEXT UNIQUE NOT NULL,
    name TEXT NOT NULL,
    
    -- Scheduling
    scheduled_at DATETIME,              -- NULL = instant room
    duration_minutes INTEGER,           -- Expected duration (informational)
    
    -- Access Control
    password_hash TEXT,                 -- NULL = no password required
    waiting_room_enabled BOOLEAN DEFAULT FALSE,
    
    -- Stream Key Binding
    stream_key_id TEXT REFERENCES stream_keys(id) ON DELETE SET NULL,
    
    -- Watermark Config
    watermark_mode TEXT DEFAULT 'none', -- 'none', 'text', 'logo', 'both'
    watermark_text TEXT,                -- Template: "{{name}} - {{date}}"
    watermark_logo_path TEXT,           -- Path to uploaded logo file
    watermark_logo_position TEXT DEFAULT 'bottom-right',
    watermark_opacity REAL DEFAULT 0.3,
    
    -- State
    status TEXT DEFAULT 'pending',      -- 'pending', 'live', 'ended'
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    started_at DATETIME,
    ended_at DATETIME
);

-- Participants (ephemeral, cleared on room end)
CREATE TABLE participants (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    role TEXT NOT NULL,                 -- 'admin', 'viewer'
    color TEXT,                         -- Assigned cursor color
    
    is_admitted BOOLEAN DEFAULT FALSE,  -- For waiting room
    joined_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    
    audio_enabled BOOLEAN DEFAULT TRUE,
    video_enabled BOOLEAN DEFAULT TRUE
);

-- Chat Messages
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    participant_id TEXT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    
    type TEXT NOT NULL,                 -- 'text', 'file'
    content TEXT,                       -- Message text or file ref JSON
    
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- File Uploads
CREATE TABLE files (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    uploader_id TEXT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    
    original_name TEXT NOT NULL,
    stored_path TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Admin Configuration (single row)
CREATE TABLE config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    default_watermark_text TEXT,
    default_watermark_logo_path TEXT,
    turn_external_url TEXT,
    turn_external_username TEXT,
    turn_external_credential TEXT
);

-- Initialize config with default row
INSERT INTO config (id) VALUES (1);

-- Indexes for common queries
CREATE INDEX idx_rooms_scheduled ON rooms(scheduled_at) WHERE status = 'pending';
CREATE INDEX idx_rooms_status ON rooms(status);
CREATE INDEX idx_rooms_stream_key ON rooms(stream_key_id);
CREATE INDEX idx_rooms_slug ON rooms(slug);
CREATE INDEX idx_participants_room ON participants(room_id);
CREATE INDEX idx_participants_admitted ON participants(room_id, is_admitted);
CREATE INDEX idx_messages_room ON messages(room_id);
CREATE INDEX idx_messages_created ON messages(room_id, created_at);
CREATE INDEX idx_files_room ON files(room_id);
